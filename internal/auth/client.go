package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SarnautCore/server/internal/auth/secret"
)

// Client is the auth HTTP API as seen from a tool: the probe, the slice script
// and anything else that has to reach "logged in with a character" before it
// can open a game connection.
//
// It exists so those callers do not each hand-roll JSON against the same six
// endpoints, and so a change to the API breaks compilation rather than a
// PowerShell string.
type Client struct {
	// BaseURL is the auth service root, for example http://127.0.0.1:8082.
	BaseURL string
	// HTTP is optional; nil means a client with a 10-second timeout.
	HTTP *http.Client
}

// APIError is a refusal the service reported, carrying the machine-readable
// code so a caller can branch on it.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (err *APIError) Error() string {
	return fmt.Sprintf("auth api: %s (%s, HTTP %d)", err.Message, err.Code, err.Status)
}

// Code reports the API error code of err, or the empty string.
func Code(err error) string {
	var apiError *APIError
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}

func (client Client) httpClient() *http.Client {
	if client.HTTP != nil {
		return client.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (client Client) do(
	ctx context.Context,
	method, path string,
	token secret.Value,
	body any,
	into any,
) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("auth api: encode request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		method,
		strings.TrimRight(client.BaseURL, "/")+path,
		reader,
	)
	if err != nil {
		return fmt.Errorf("auth api: build request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if !token.Empty() {
		request.Header.Set("Authorization", "Bearer "+token.Reveal())
	}

	response, err := client.httpClient().Do(request)
	if err != nil {
		return fmt.Errorf("auth api: %s %s: %w", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= 400 {
		var refusal errorResponse
		_ = json.NewDecoder(io.LimitReader(response.Body, maxRequestBytes)).Decode(&refusal)
		return &APIError{Status: response.StatusCode, Code: refusal.Error, Message: refusal.Message}
	}
	if into == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxRequestBytes)).Decode(into); err != nil {
		return fmt.Errorf("auth api: decode response: %w", err)
	}
	return nil
}

// credentialBody is the one place this package deliberately serialises a
// secret: the request body that carries it to the service.
//
// It exists because [secret.Value] marshals to the redaction, which is the
// point — a struct that encoded the real value by default would make every
// accidental encode a leak. Writing plaintext therefore costs an explicit
// [secret.Value.Reveal] here, where it is visible in review.
type credentialBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Register creates an account. A caller that does not care whether the account
// already existed can ignore an EMAIL_TAKEN [APIError].
func (client Client) Register(ctx context.Context, email, password secret.Value) (string, error) {
	var response registerResponse
	if err := client.do(ctx, http.MethodPost, "/v1/accounts", secret.Value{}, credentialBody{
		Email:    email.Reveal(),
		Password: password.Reveal(),
	}, &response); err != nil {
		return "", err
	}
	return response.AccountID, nil
}

// Login exchanges credentials for an account session token.
func (client Client) Login(ctx context.Context, email, password secret.Value) (secret.Value, error) {
	var response loginResponse
	if err := client.do(ctx, http.MethodPost, "/v1/sessions", secret.Value{}, credentialBody{
		Email:    email.Reveal(),
		Password: password.Reveal(),
	}, &response); err != nil {
		return secret.Value{}, err
	}
	return secret.New(response.SessionToken), nil
}

// CharacterSummary is one row of an account's roster.
type CharacterSummary struct {
	CharacterID     string
	Name            string
	ChargenOptionID string
}

// ListCharacters returns the account's roster.
func (client Client) ListCharacters(ctx context.Context, token secret.Value) ([]CharacterSummary, error) {
	var response characterListResponse
	if err := client.do(ctx, http.MethodGet, "/v1/characters", token, nil, &response); err != nil {
		return nil, err
	}
	summaries := make([]CharacterSummary, 0, len(response.Characters))
	for _, document := range response.Characters {
		summaries = append(summaries, CharacterSummary{
			CharacterID:     document.CharacterID,
			Name:            document.Name,
			ChargenOptionID: document.ChargenOptionID,
		})
	}
	return summaries, nil
}

// CreateCharacter creates one character of the named chargen option.
func (client Client) CreateCharacter(
	ctx context.Context,
	token secret.Value,
	name, chargenOptionID string,
) (CharacterSummary, error) {
	var document characterDocument
	if err := client.do(ctx, http.MethodPost, "/v1/characters", token, createCharacterRequest{
		Name:            name,
		ChargenOptionID: chargenOptionID,
	}, &document); err != nil {
		return CharacterSummary{}, err
	}
	return CharacterSummary{
		CharacterID:     document.CharacterID,
		Name:            document.Name,
		ChargenOptionID: document.ChargenOptionID,
	}, nil
}

// DeleteCharacter soft-deletes one character.
func (client Client) DeleteCharacter(ctx context.Context, token secret.Value, characterID string) error {
	return client.do(ctx, http.MethodDelete, "/v1/characters/"+characterID, token, nil, nil)
}

// Ticket mints a single-use shard ticket for one character.
func (client Client) Ticket(ctx context.Context, token secret.Value, characterID string) (secret.Value, error) {
	var response ticketResponse
	if err := client.do(ctx, http.MethodPost, "/v1/tickets", token, ticketRequest{
		CharacterID: characterID,
	}, &response); err != nil {
		return secret.Value{}, err
	}
	return secret.New(response.Ticket), nil
}

// PlayableOptions lists the chargen options the service will accept.
func (client Client) PlayableOptions(ctx context.Context) ([]string, error) {
	var response chargenOptionsResponse
	if err := client.do(ctx, http.MethodGet, "/v1/chargen/options", secret.Value{}, nil, &response); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(response.Options))
	for _, option := range response.Options {
		ids = append(ids, option.ID)
	}
	return ids, nil
}

// EnsureTicket is the whole out-of-band flow a tool needs before it can
// connect: register if the account is new, log in, create the named character
// if the roster does not have it, and mint a ticket for it (ADR 0030,
// protocol/session.md rule 5.3).
func (client Client) EnsureTicket(
	ctx context.Context,
	email, password secret.Value,
	characterName string,
) (secret.Value, string, error) {
	if _, err := client.Register(ctx, email, password); err != nil && Code(err) != "EMAIL_TAKEN" {
		return secret.Value{}, "", err
	}
	token, err := client.Login(ctx, email, password)
	if err != nil {
		return secret.Value{}, "", err
	}
	characters, err := client.ListCharacters(ctx, token)
	if err != nil {
		return secret.Value{}, "", err
	}

	var chosen CharacterSummary
	for _, character := range characters {
		if character.Name == characterName {
			chosen = character
			break
		}
	}
	if chosen.CharacterID == "" {
		options, err := client.PlayableOptions(ctx)
		if err != nil {
			return secret.Value{}, "", err
		}
		if len(options) == 0 {
			return secret.Value{}, "", errors.New("auth api: the service offers no chargen options")
		}
		chosen, err = client.CreateCharacter(ctx, token, characterName, options[0])
		if err != nil {
			return secret.Value{}, "", err
		}
	}

	ticket, err := client.Ticket(ctx, token, chosen.CharacterID)
	if err != nil {
		return secret.Value{}, "", err
	}
	return ticket, chosen.CharacterID, nil
}
