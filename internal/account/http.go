package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/account/credential"
	"github.com/SarnautCore/server/internal/account/secret"
	"github.com/SarnautCore/server/internal/store"
)

// maxRequestBytes bounds a request body. Every document this API accepts is a
// few hundred bytes; anything larger is a mistake or an attack, and either way
// there is no reason to buffer it.
const maxRequestBytes = 16 << 10

// The HTTP surface ADR 0030 chose, served with stdlib net/http and no
// framework. Handlers do no logic: they decode, call the service, and map its
// sentinel errors onto a status and a code.
//
//	POST   /v1/accounts               register
//	POST   /v1/sessions               log in
//	DELETE /v1/sessions               log out
//	GET    /v1/characters             this account's roster
//	POST   /v1/characters             create
//	DELETE /v1/characters/{id}        delete
//	POST   /v1/characters/name-checks  reserve a name for the creation form
//	POST   /v1/tickets                mint a shard ticket
//	GET    /v1/chargen/options        the option list the client renders
func (service *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/accounts", service.handleRegister)
	mux.HandleFunc("POST /v1/sessions", service.handleLogin)
	mux.HandleFunc("DELETE /v1/sessions", service.authenticated(service.handleLogout))
	mux.HandleFunc("GET /v1/characters", service.authenticated(service.handleListCharacters))
	mux.HandleFunc("POST /v1/characters", service.authenticated(service.handleCreateCharacter))
	mux.HandleFunc("DELETE /v1/characters/{characterID}", service.authenticated(service.handleDeleteCharacter))
	mux.HandleFunc("POST /v1/characters/name-checks", service.authenticated(service.handleNameCheck))
	mux.HandleFunc("POST /v1/tickets", service.authenticated(service.handleMintTicket))
	mux.HandleFunc("GET /v1/chargen/options", service.handleChargenOptions)
	return mux
}

// authenticatedHandler is a handler that has already resolved a bearer token to
// an account, so no handler below repeats the check or forgets it.
type authenticatedHandler func(http.ResponseWriter, *http.Request, uuid.UUID)

func (service *Service) authenticated(next authenticatedHandler) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		header := request.Header.Get("Authorization")
		raw, found := strings.CutPrefix(header, "Bearer ")
		if !found {
			writeError(writer, http.StatusUnauthorized, "UNAUTHENTICATED", "a bearer token is required")
			return
		}
		accountID, err := service.Authenticate(request.Context(), secret.New(strings.TrimSpace(raw)))
		if err != nil {
			// Never echo the token, not even its length.
			writeError(writer, http.StatusUnauthorized, "UNAUTHENTICATED", "the session token is not valid")
			return
		}
		next(writer, request, accountID)
	}
}

type registerRequest struct {
	Email    secret.Value `json:"email"`
	Password secret.Value `json:"password"`
}

type registerResponse struct {
	AccountID string `json:"account_id"`
}

func (service *Service) handleRegister(writer http.ResponseWriter, request *http.Request) {
	var body registerRequest
	if !decode(writer, request, &body) {
		return
	}
	accountID, err := service.Register(request.Context(), body.Email, body.Password)
	switch {
	case errors.Is(err, ErrEmailInvalid):
		writeError(writer, http.StatusBadRequest, "EMAIL_INVALID", "that email address is not usable")
	case errors.Is(err, credential.ErrEmptyPassword):
		writeError(writer, http.StatusBadRequest, "PASSWORD_REQUIRED", "a password is required")
	case errors.Is(err, credential.ErrPasswordTooShort):
		writeError(writer, http.StatusBadRequest, "PASSWORD_TOO_SHORT",
			fmt.Sprintf("a password is at least %d characters", credential.MinimumPasswordLength))
	case errors.Is(err, store.ErrEmailTaken):
		// An honest 409. Registration necessarily discloses that an address is
		// taken; hiding it would mean accepting a registration that did nothing.
		writeError(writer, http.StatusConflict, "EMAIL_TAKEN", "that email address is already registered")
	case err != nil:
		service.internal(writer, request, "register", err)
	default:
		writeJSON(writer, http.StatusCreated, registerResponse{AccountID: accountID.String()})
	}
}

type loginRequest struct {
	Email    secret.Value `json:"email"`
	Password secret.Value `json:"password"`
}

type loginResponse struct {
	SessionToken string `json:"session_token"`
	AccountID    string `json:"account_id"`
	ExpiresAt    string `json:"expires_at"`
}

func (service *Service) handleLogin(writer http.ResponseWriter, request *http.Request) {
	var body loginRequest
	if !decode(writer, request, &body) {
		return
	}
	session, err := service.Login(request.Context(), body.Email, body.Password)
	switch {
	case errors.Is(err, ErrInvalidCredentials), errors.Is(err, ErrAccountDisabled):
		writeError(writer, http.StatusUnauthorized, "INVALID_CREDENTIALS", "email or password is wrong")
	case err != nil:
		service.internal(writer, request, "login", err)
	default:
		// The one place a token plaintext is written out. It goes to the peer
		// that just proved it owns the account, and nowhere else.
		writeJSON(writer, http.StatusOK, loginResponse{
			SessionToken: session.Token.Reveal(),
			AccountID:    session.AccountID.String(),
			ExpiresAt:    session.ExpiresAt.Format(time.RFC3339),
		})
	}
}

func (service *Service) handleLogout(writer http.ResponseWriter, request *http.Request, _ uuid.UUID) {
	raw, _ := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	if err := service.Logout(request.Context(), secret.New(strings.TrimSpace(raw))); err != nil {
		service.internal(writer, request, "logout", err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

type characterDocument struct {
	CharacterID     string `json:"character_id"`
	Name            string `json:"name"`
	ChargenOptionID string `json:"chargen_option_id"`
	CreatedAt       string `json:"created_at"`
}

type characterListResponse struct {
	Characters []characterDocument `json:"characters"`
}

func (service *Service) handleListCharacters(writer http.ResponseWriter, request *http.Request, accountID uuid.UUID) {
	characters, err := service.ListCharacters(request.Context(), accountID)
	if err != nil {
		service.internal(writer, request, "list characters", err)
		return
	}
	writeJSON(writer, http.StatusOK, characterListResponse{Characters: characterDocuments(characters)})
}

func characterDocuments(characters []store.Character) []characterDocument {
	documents := make([]characterDocument, 0, len(characters))
	for _, character := range characters {
		documents = append(documents, characterDocument{
			CharacterID:     character.CharacterID.String(),
			Name:            character.Name,
			ChargenOptionID: character.ChargenOptionID,
			CreatedAt:       character.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return documents
}

type createCharacterRequest struct {
	Name            string `json:"name"`
	ChargenOptionID string `json:"chargen_option_id"`
}

func (service *Service) handleCreateCharacter(writer http.ResponseWriter, request *http.Request, accountID uuid.UUID) {
	var body createCharacterRequest
	if !decode(writer, request, &body) {
		return
	}
	character, err := service.CreateCharacter(request.Context(), accountID, body.Name, body.ChargenOptionID)
	switch {
	case errors.Is(err, ErrNameInvalid):
		writeError(writer, http.StatusBadRequest, "NAME_INVALID",
			fmt.Sprintf("a name is %d to %d characters: an uppercase letter, then lowercase letters, "+
				"with at most single apostrophes or hyphens between them",
				MinimumNameLength, MaximumNameLength))
	case errors.Is(err, ErrNameBlocked):
		writeError(writer, http.StatusBadRequest, "NAME_BLOCKED", "that name is not available")
	case errors.Is(err, ErrUnknownOption):
		writeError(writer, http.StatusBadRequest, "UNKNOWN_OPTION", "no such character-creation option")
	case errors.Is(err, ErrOptionDisabled):
		writeError(writer, http.StatusBadRequest, "OPTION_DISABLED", "that character-creation option is not playable")
	case errors.Is(err, store.ErrNameTaken):
		writeError(writer, http.StatusConflict, "NAME_TAKEN", "that name is already taken")
	case err != nil:
		service.internal(writer, request, "create character", err)
	default:
		writeJSON(writer, http.StatusCreated, characterDocuments([]store.Character{character})[0])
	}
}

func (service *Service) handleDeleteCharacter(writer http.ResponseWriter, request *http.Request, accountID uuid.UUID) {
	characterID, err := uuid.Parse(request.PathValue("characterID"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "CHARACTER_ID_INVALID", "that is not a character id")
		return
	}
	err = service.DeleteCharacter(request.Context(), accountID, characterID)
	switch {
	case errors.Is(err, ErrCharacterNotFound):
		writeError(writer, http.StatusNotFound, "CHARACTER_NOT_FOUND", "no such character")
	case err != nil:
		service.internal(writer, request, "delete character", err)
	default:
		writer.WriteHeader(http.StatusNoContent)
	}
}

type nameCheckRequest struct {
	Name string `json:"name"`
}

type nameCheckResponse struct {
	Name          string `json:"name"`
	Available     bool   `json:"available"`
	Reason        string `json:"reason,omitempty"`
	ReservedUntil string `json:"reserved_until,omitempty"`
}

func (service *Service) handleNameCheck(writer http.ResponseWriter, request *http.Request, accountID uuid.UUID) {
	var body nameCheckRequest
	if !decode(writer, request, &body) {
		return
	}
	check, err := service.CheckName(request.Context(), accountID, body.Name)
	if err != nil {
		service.internal(writer, request, "check name", err)
		return
	}
	response := nameCheckResponse{Name: check.Name, Available: check.Available, Reason: check.Reason}
	if check.Available {
		response.ReservedUntil = check.ReservedUntil.Format(time.RFC3339)
	}
	writeJSON(writer, http.StatusOK, response)
}

type ticketRequest struct {
	CharacterID string `json:"character_id"`
}

type ticketResponse struct {
	Ticket      string `json:"ticket"`
	CharacterID string `json:"character_id"`
	ExpiresAt   string `json:"expires_at"`
}

func (service *Service) handleMintTicket(writer http.ResponseWriter, request *http.Request, accountID uuid.UUID) {
	var body ticketRequest
	if !decode(writer, request, &body) {
		return
	}
	characterID, err := uuid.Parse(body.CharacterID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "CHARACTER_ID_INVALID", "that is not a character id")
		return
	}
	ticket, err := service.MintTicket(request.Context(), accountID, characterID)
	switch {
	case errors.Is(err, ErrCharacterNotFound):
		writeError(writer, http.StatusNotFound, "CHARACTER_NOT_FOUND", "no such character")
	case err != nil:
		service.internal(writer, request, "mint ticket", err)
	default:
		writeJSON(writer, http.StatusCreated, ticketResponse{
			Ticket:      ticket.Token.Reveal(),
			CharacterID: ticket.CharacterID.String(),
			ExpiresAt:   ticket.ExpiresAt.Format(time.RFC3339),
		})
	}
}

type chargenOptionDocument struct {
	ID             string  `json:"id"`
	Race           string  `json:"race"`
	Class          string  `json:"class"`
	Sex            string  `json:"sex"`
	Faction        string  `json:"faction"`
	NameKey        string  `json:"name_key"`
	DescriptionKey string  `json:"description_key"`
	VisualRef      string  `json:"visual_ref"`
	SpawnZoneID    string  `json:"spawn_zone_id"`
	StartingLevel  uint32  `json:"starting_level"`
	SpawnX         float32 `json:"spawn_x"`
	SpawnY         float32 `json:"spawn_y"`
	SpawnZ         float32 `json:"spawn_z"`
}

type chargenOptionsResponse struct {
	Options []chargenOptionDocument `json:"options"`
}

// handleChargenOptions is unauthenticated on purpose: the option list is
// content, it is in the pack the client already has, and a login form that
// cannot render the creation screen until you log in is a worse experience for
// no security gain.
func (service *Service) handleChargenOptions(writer http.ResponseWriter, _ *http.Request) {
	options := service.PlayableOptions()
	documents := make([]chargenOptionDocument, 0, len(options))
	for _, option := range options {
		documents = append(documents, chargenOptionDocument{
			ID:             option.ID,
			Race:           option.Race,
			Class:          option.Class,
			Sex:            option.Sex,
			Faction:        option.Faction,
			NameKey:        option.NameKey,
			DescriptionKey: option.DescriptionKey,
			VisualRef:      option.VisualRef,
			SpawnZoneID:    option.SpawnZoneID,
			StartingLevel:  option.StartingLevel,
			SpawnX:         option.SpawnPosition.X,
			SpawnY:         option.SpawnPosition.Y,
			SpawnZ:         option.SpawnPosition.Z,
		})
	}
	writeJSON(writer, http.StatusOK, chargenOptionsResponse{Options: documents})
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func decode(writer http.ResponseWriter, request *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		// The decode error is not echoed: it can quote the body, and the body
		// is where the password is.
		writeError(writer, http.StatusBadRequest, "MALFORMED_REQUEST", "the request body is not the expected JSON document")
		return false
	}
	return true
}

func writeJSON(writer http.ResponseWriter, status int, document any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(document)
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, errorResponse{Error: code, Message: message})
}

// internal logs the cause and tells the peer nothing about it. Error strings
// are where an input gets stapled to a message, so none of them travels.
func (service *Service) internal(writer http.ResponseWriter, request *http.Request, operation string, err error) {
	service.logger.ErrorContext(request.Context(), "auth request failed",
		"operation", operation,
		"error", err,
	)
	writeError(writer, http.StatusInternalServerError, "INTERNAL", "the request could not be completed")
}
