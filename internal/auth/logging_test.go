package auth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/auth/secret"
	"github.com/SarnautCore/server/internal/charstore"
)

// Sentinels chosen so that a partial leak — a truncated token, a quoted email —
// is still a substring match.
const (
	sentinelPassword = "SENTINEL-PW-2f9c41"
	sentinelEmail    = "sentinel-2f9c41@example.invalid"
)

// TestAuthLogsCarryNoSecrets is the test ADR 0030 section 5 names.
//
// It drives the whole surface with sentinel values, capturing every token the
// service mints along the way, and asserts that neither sentinel nor any
// captured token appears in any log record or in any returned error string.
//
// The failure paths are enumerated deliberately. Success paths rarely leak, and
// error wrapping is where an input gets stapled to a message.
func TestAuthLogsCarryNoSecrets(t *testing.T) {
	t.Parallel()

	var buffer safeBuffer
	options, _ := testOptions(t)
	options.Logger = slog.New(slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	service, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	captured := []string{sentinelPassword, sentinelEmail}
	var failures []error
	record := func(err error) {
		if err != nil {
			failures = append(failures, err)
		}
	}
	ctx := t.Context()

	// Register, then log in.
	accountID, err := service.Register(ctx, secret.New(sentinelEmail), secret.New(sentinelPassword))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	session, err := service.Login(ctx, secret.New(sentinelEmail), secret.New(sentinelPassword))
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	captured = append(captured, session.Token.Reveal())

	// List and create characters.
	if _, err := service.ListCharacters(ctx, accountID); err != nil {
		t.Fatalf("ListCharacters() error = %v", err)
	}
	character, err := service.CreateCharacter(ctx, accountID, "Anne", "chargen.league.warrior")
	if err != nil {
		t.Fatalf("CreateCharacter() error = %v", err)
	}

	// Mint and redeem a ticket.
	ticket, err := service.MintTicket(ctx, accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	captured = append(captured, ticket.Token.Reveal())
	if _, err := service.RedeemTicket(ctx, ticket.Token, testHolder); err != nil {
		t.Fatalf("RedeemTicket() error = %v", err)
	}

	// The failure paths ADR 0030 section 5 enumerates.
	_, err = service.Login(ctx, secret.New(sentinelEmail), secret.New(sentinelPassword+"-wrong"))
	record(err)
	captured = append(captured, sentinelPassword+"-wrong")

	_, err = service.Login(ctx, secret.New("unknown-"+sentinelEmail), secret.New(sentinelPassword))
	record(err)
	captured = append(captured, "unknown-"+sentinelEmail)

	expired, err := service.MintTicket(ctx, accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	captured = append(captured, expired.Token.Reveal())
	// Burn it, then redeem it again: an already-redeemed ticket.
	if _, err := service.RedeemTicket(ctx, expired.Token, testHolder); err != nil {
		t.Fatalf("RedeemTicket() error = %v", err)
	}
	_, err = service.RedeemTicket(ctx, expired.Token, testHolder)
	record(err)

	// A malformed request, and a taken name.
	forged := secret.New(TicketPrefix + "definitely-not-a-real-ticket")
	captured = append(captured, forged.Reveal())
	_, err = service.RedeemTicket(ctx, forged, testHolder)
	record(err)

	_, err = service.CreateCharacter(ctx, accountID, "Anne", "chargen.league.warrior")
	record(err)
	_, err = service.CreateCharacter(ctx, accountID, "Ann3", "chargen.league.warrior")
	record(err)
	_, err = service.Authenticate(ctx, secret.New(SessionTokenPrefix+"forged"))
	record(err)
	record(service.Logout(ctx, session.Token))

	// The same surface over HTTP, where a handler could echo a body.
	driveHTTP(t, service, &captured)

	logs := buffer.String()
	for _, plaintext := range captured {
		if plaintext == "" {
			continue
		}
		if strings.Contains(logs, plaintext) {
			t.Errorf("a log record carries the secret %q", redactForFailure(plaintext))
		}
		for _, failure := range failures {
			if strings.Contains(failure.Error(), plaintext) {
				t.Errorf("an error string carries the secret %q: %v", redactForFailure(plaintext), failure)
			}
		}
	}

	// The test is only meaningful if something was actually logged.
	if !strings.Contains(logs, "account registered") || !strings.Contains(logs, "login refused") {
		t.Fatalf("the service logged nothing recognisable; the assertion above proves nothing:\n%s", logs)
	}
	// The derived forms ADR 0030 does permit must be present, or the logs are
	// useless for an incident.
	if !strings.Contains(logs, accountID.String()) {
		t.Error("no log record carries account_id, which is the one identifier an operator has")
	}
	if !strings.Contains(logs, "example.invalid") {
		t.Error("no log record carries email_domain")
	}
	if !strings.Contains(logs, session.Token.ID()) {
		t.Error("no log record carries token_id, so two records about one token cannot be tied together")
	}
}

// driveHTTP repeats the flow through the handlers, where a decoder error or a
// refusal message could echo the body.
func driveHTTP(t *testing.T, service *Service, captured *[]string) {
	t.Helper()
	handler := service.Handler()

	post := func(path, body, token string) *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	// A registration whose body is malformed, so the decoder error is the one
	// that would quote the body — and the body is where the password is.
	post("/v1/accounts", `{"email":"http-`+sentinelEmail+`","password":"`+sentinelPassword+`",`, "")
	*captured = append(*captured, "http-"+sentinelEmail)

	post("/v1/accounts", `{"email":"http-`+sentinelEmail+`","password":"`+sentinelPassword+`"}`, "")
	recorder := post("/v1/sessions", `{"email":"http-`+sentinelEmail+`","password":"`+sentinelPassword+`"}`, "")
	var login loginResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &login); err == nil && login.SessionToken != "" {
		*captured = append(*captured, login.SessionToken)
	}
	// A wrong password over HTTP.
	post("/v1/sessions", `{"email":"http-`+sentinelEmail+`","password":"`+sentinelPassword+`-no"}`, "")
	*captured = append(*captured, sentinelPassword+"-no")
	// An unknown field, which DisallowUnknownFields refuses while the body is
	// still in hand.
	post("/v1/accounts", `{"email":"x@y.invalid","password":"`+sentinelPassword+`","extra":1}`, "")
	// A forged bearer token.
	post("/v1/characters", `{"name":"Bea","chargen_option_id":"chargen.league.warrior"}`,
		SessionTokenPrefix+"forged-bearer")
	*captured = append(*captured, SessionTokenPrefix+"forged-bearer")
}

// The full HTTP flow, asserted end to end: this is the surface the launcher
// and the smoke script drive.
func TestHTTPAPIRegisterLoginCreateListDelete(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL, HTTP: server.Client()}
	ctx := t.Context()

	accountID, err := client.Register(ctx, secret.New(testEmail), secret.New(testPassword))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, err := uuid.Parse(accountID); err != nil {
		t.Errorf("Register() returned %q, which is not a uuid", accountID)
	}
	if _, err := client.Register(ctx, secret.New(testEmail), secret.New(testPassword)); Code(err) != "EMAIL_TAKEN" {
		t.Errorf("Register() twice error = %v, want EMAIL_TAKEN", err)
	}

	if _, err := client.Login(ctx, secret.New(testEmail), secret.New("wrong-password")); Code(err) != "INVALID_CREDENTIALS" {
		t.Errorf("Login(wrong) error = %v, want INVALID_CREDENTIALS", err)
	}
	token, err := client.Login(ctx, secret.New(testEmail), secret.New(testPassword))
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	options, err := client.PlayableOptions(ctx)
	if err != nil {
		t.Fatalf("PlayableOptions() error = %v", err)
	}
	if len(options) != 1 || options[0] != "chargen.league.warrior" {
		t.Fatalf("PlayableOptions() = %v, want the one enabled option", options)
	}

	if _, err := client.CreateCharacter(ctx, token, "Ann3", options[0]); Code(err) != "NAME_INVALID" {
		t.Errorf("CreateCharacter(Ann3) error = %v, want NAME_INVALID", err)
	}
	if _, err := client.CreateCharacter(ctx, token, "Bea", "chargen.empire.warrior"); Code(err) != "OPTION_DISABLED" {
		t.Errorf("CreateCharacter(disabled) error = %v, want OPTION_DISABLED", err)
	}
	character, err := client.CreateCharacter(ctx, token, "Anne", options[0])
	if err != nil {
		t.Fatalf("CreateCharacter() error = %v", err)
	}
	if _, err := client.CreateCharacter(ctx, token, "Anne", options[0]); Code(err) != "NAME_TAKEN" {
		t.Errorf("CreateCharacter(duplicate) error = %v, want NAME_TAKEN", err)
	}

	roster, err := client.ListCharacters(ctx, token)
	if err != nil {
		t.Fatalf("ListCharacters() error = %v", err)
	}
	if len(roster) != 1 || roster[0].CharacterID != character.CharacterID {
		t.Fatalf("ListCharacters() = %+v, want the created character", roster)
	}

	ticket, err := client.Ticket(ctx, token, character.CharacterID)
	if err != nil {
		t.Fatalf("Ticket() error = %v", err)
	}
	if !strings.HasPrefix(ticket.Reveal(), TicketPrefix) {
		t.Error("the minted ticket does not carry the ticket prefix")
	}

	if err := client.DeleteCharacter(ctx, token, character.CharacterID); err != nil {
		t.Fatalf("DeleteCharacter() error = %v", err)
	}
	roster, err = client.ListCharacters(ctx, token)
	if err != nil {
		t.Fatalf("ListCharacters() error = %v", err)
	}
	if len(roster) != 0 {
		t.Errorf("ListCharacters() after delete = %+v, want none", roster)
	}

	// Every authenticated route refuses an anonymous caller.
	for _, call := range []func() error{
		func() error { _, err := client.ListCharacters(ctx, secret.Value{}); return err },
		func() error { _, err := client.CreateCharacter(ctx, secret.Value{}, "Cara", options[0]); return err },
		func() error { _, err := client.Ticket(ctx, secret.Value{}, character.CharacterID); return err },
		func() error { return client.DeleteCharacter(ctx, secret.Value{}, character.CharacterID) },
	} {
		if err := call(); Code(err) != "UNAUTHENTICATED" {
			t.Errorf("an anonymous call returned %v, want UNAUTHENTICATED", err)
		}
	}
}

// safeBuffer is a bytes.Buffer a logger and a test goroutine may share.
type safeBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *safeBuffer) Write(payload []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(payload)
}

func (buffer *safeBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

// redactForFailure keeps the failure message itself from printing the secret it
// just caught, which would put it in the CI log the assertion exists to protect.
func redactForFailure(plaintext string) string {
	if len(plaintext) <= 4 {
		return "****"
	}
	return plaintext[:4] + "…(" + secret.New(plaintext).ID() + ")"
}

// A compile-time reminder that the store's sentinel errors are the ones the
// handlers map, so a renamed error is a build failure rather than a 500.
var _ = charstore.ErrNameTaken
