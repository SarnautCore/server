package auth

import (
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/auth/credential"
	"github.com/SarnautCore/server/internal/auth/secret"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/pack"
)

// cheapHasher keeps a suite that is not about Argon2id's cost from paying it.
// The cost itself is pinned by the credential package's own tests.
var cheapHasher = credential.Hasher{Params: credential.Params{
	Time:        1,
	Memory:      8,
	Parallelism: 1,
	SaltLength:  16,
	KeyLength:   32,
}}

// testClock is a clock a test advances, so token expiry is asserted rather than
// waited for.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
}

func (clock *testClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *testClock) advance(by time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(by)
}

// testOptions is the fixture: an in-memory repository, an in-memory key-value
// store on the test clock, and the pack's real chargen catalogue.
func testOptions(t *testing.T) (Options, *testClock) {
	t.Helper()
	clock := newTestClock()
	return Options{
		Repository: charstore.NewMemoryWithClock(clock.Now),
		Keys:       newTestKeyValue(clock.Now),
		Catalogue:  testCatalogue(t),
		Hasher:     cheapHasher,
		Logger:     slog.New(slog.DiscardHandler),
		Clock:      clock.Now,
	}, clock
}

func newTestService(t *testing.T) (*Service, *testClock) {
	t.Helper()
	options, clock := testOptions(t)
	service, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return service, clock
}

// testCatalogue is built from real chargen rows rather than a hand-written
// struct, so a change to the pack's row shape reaches these tests.
func testCatalogue(t *testing.T) *Catalogue {
	t.Helper()
	catalogue, err := NewCatalogue([]pack.ChargenOption{
		{
			ID:            "chargen.league.warrior",
			Race:          "race.human",
			Class:         "class.warrior",
			Sex:           "female",
			Faction:       "faction.league",
			Enabled:       true,
			SpawnZoneID:   "zone.paper-harbor",
			SpawnPosition: pack.Vec3{X: 12, Y: 4.5},
			StartingLevel: 1,
		},
		{
			ID:            "chargen.empire.warrior",
			Race:          "race.orc",
			Class:         "class.warrior",
			Sex:           "male",
			Faction:       "faction.empire",
			Enabled:       false,
			SpawnZoneID:   "zone.paper-harbor",
			SpawnPosition: pack.Vec3{X: -2},
			StartingLevel: 1,
		},
	})
	if err != nil {
		t.Fatalf("NewCatalogue() error = %v", err)
	}
	return catalogue
}

const (
	testEmail    = "player@example.invalid"
	testPassword = "a-password-long-enough"
)

func registerAndLogin(t *testing.T, service *Service) (uuid.UUID, Session) {
	t.Helper()
	ctx := t.Context()
	accountID, err := service.Register(ctx, secret.New(testEmail), secret.New(testPassword))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	session, err := service.Login(ctx, secret.New(testEmail), secret.New(testPassword))
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	return accountID, session
}

func TestRegisterThenLoginMintsASessionToken(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	accountID, session := registerAndLogin(t, service)

	if session.AccountID != accountID {
		t.Errorf("Login() account = %s, want %s", session.AccountID, accountID)
	}
	if got := session.Token.Reveal(); len(got) != len(SessionTokenPrefix)+43 {
		t.Errorf("session token is %d characters, want %d", len(got), len(SessionTokenPrefix)+43)
	}
	resolved, err := service.Authenticate(t.Context(), session.Token)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if resolved != accountID {
		t.Errorf("Authenticate() = %s, want %s", resolved, accountID)
	}
}

func TestLoginWithTheWrongPasswordIsRefusedWithoutTheDummyPath(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	registerAndLogin(t, service)

	before := service.Counters()
	_, err := service.Login(t.Context(), secret.New(testEmail), secret.New("not-the-password"))
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login() error = %v, want ErrInvalidCredentials", err)
	}
	after := service.Counters()
	if after.FailedLogins != before.FailedLogins+1 {
		t.Errorf("FailedLogins = %d, want %d", after.FailedLogins, before.FailedLogins+1)
	}
	// A known account verifies against the stored hash, not the dummy one.
	if after.DummyHashVerifications != before.DummyHashVerifications {
		t.Errorf("DummyHashVerifications = %d, want %d unchanged",
			after.DummyHashVerifications, before.DummyHashVerifications)
	}
}

// The assertion is on the code path, not on a stopwatch: a timing test of
// Argon2id would be flaky on any loaded machine and would still not prove which
// branch ran.
func TestLoginWithAnUnknownEmailTakesTheDummyHashPath(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	registerAndLogin(t, service)

	before := service.Counters()
	_, err := service.Login(t.Context(), secret.New("nobody@example.invalid"), secret.New(testPassword))
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login() error = %v, want ErrInvalidCredentials", err)
	}
	after := service.Counters()
	if after.DummyHashVerifications != before.DummyHashVerifications+1 {
		t.Errorf("DummyHashVerifications = %d, want %d; an unknown email must still derive a hash",
			after.DummyHashVerifications, before.DummyHashVerifications+1)
	}
	// An unknown email and a wrong password are the same answer to the peer.
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Error("an unknown email produced a distinguishable error")
	}
}

func TestRegisterRefusesADuplicateEmailAndAnUnusableOne(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	registerAndLogin(t, service)

	// citext: the same address in a different case is the same account.
	_, err := service.Register(t.Context(), secret.New("PLAYER@Example.Invalid"), secret.New(testPassword))
	if !errors.Is(err, charstore.ErrEmailTaken) {
		t.Errorf("Register(duplicate) error = %v, want ErrEmailTaken", err)
	}
	if _, err := service.Register(t.Context(), secret.New("not-an-address"), secret.New(testPassword)); !errors.Is(err, ErrEmailInvalid) {
		t.Errorf("Register(no domain) error = %v, want ErrEmailInvalid", err)
	}
	if _, err := service.Register(t.Context(), secret.New("a@b.invalid"), secret.New("short")); !errors.Is(err, credential.ErrPasswordTooShort) {
		t.Errorf("Register(short password) error = %v, want ErrPasswordTooShort", err)
	}
}

func TestAnExpiredSessionTokenIsRejected(t *testing.T) {
	t.Parallel()

	service, clock := newTestService(t)
	_, session := registerAndLogin(t, service)

	clock.advance(SessionLifetime - time.Second)
	if _, err := service.Authenticate(t.Context(), session.Token); err != nil {
		t.Fatalf("Authenticate() error = %v just before expiry", err)
	}
	clock.advance(2 * time.Second)
	if _, err := service.Authenticate(t.Context(), session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("Authenticate() error = %v after expiry, want ErrUnauthenticated", err)
	}
}

func TestATamperedOrForgedTokenIsRejected(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	_, session := registerAndLogin(t, service)

	raw := session.Token.Reveal()
	tampered := raw[:len(raw)-1] + flipLast(raw)
	cases := map[string]string{
		"one character changed": tampered,
		"prefix stripped":       raw[len(SessionTokenPrefix):],
		"ticket prefix":         TicketPrefix + raw[len(SessionTokenPrefix):],
		"empty":                 "",
		"not base64":            SessionTokenPrefix + "!!!!",
		"truncated":             raw[:len(raw)-4],
	}
	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := service.Authenticate(t.Context(), secret.New(candidate)); !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("Authenticate() error = %v, want ErrUnauthenticated", err)
			}
		})
	}
	// The genuine token still works: the test above must not have burned it.
	if _, err := service.Authenticate(t.Context(), session.Token); err != nil {
		t.Errorf("Authenticate(genuine) error = %v", err)
	}
}

func flipLast(raw string) string {
	last := raw[len(raw)-1]
	if last == 'A' {
		return "B"
	}
	return "A"
}

func TestLogoutDropsTheSession(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	_, session := registerAndLogin(t, service)

	if err := service.Logout(t.Context(), session.Token); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if _, err := service.Authenticate(t.Context(), session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("Authenticate() after logout error = %v, want ErrUnauthenticated", err)
	}
	// Logging out twice is not an error: the token is already gone.
	if err := service.Logout(t.Context(), session.Token); err != nil {
		t.Errorf("Logout() twice error = %v", err)
	}
}

func TestCreateCharacterRejectsADisallowedOptionAndADuplicateName(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	accountID, _ := registerAndLogin(t, service)
	ctx := t.Context()

	character, err := service.CreateCharacter(ctx, accountID, "Anne", "chargen.league.warrior")
	if err != nil {
		t.Fatalf("CreateCharacter() error = %v", err)
	}
	if character.ChargenOptionID != "chargen.league.warrior" || character.NameNormalized != "anne" {
		t.Errorf("CreateCharacter() = %+v", character)
	}

	// A race and class the pack does not offer.
	if _, err := service.CreateCharacter(ctx, accountID, "Balthar", "chargen.league.mage"); !errors.Is(err, ErrUnknownOption) {
		t.Errorf("CreateCharacter(unknown option) error = %v, want ErrUnknownOption", err)
	}
	// One the pack ships disabled: it exists, and it is not playable.
	if _, err := service.CreateCharacter(ctx, accountID, "Balthar", "chargen.empire.warrior"); !errors.Is(err, ErrOptionDisabled) {
		t.Errorf("CreateCharacter(disabled option) error = %v, want ErrOptionDisabled", err)
	}
	// The duplicate is refused by the unique index, including through the
	// punctuation normalization.
	for _, duplicate := range []string{"Anne", "An-ne", "A'nne"} {
		if _, err := service.CreateCharacter(ctx, accountID, duplicate, "chargen.league.warrior"); !errors.Is(err, charstore.ErrNameTaken) {
			t.Errorf("CreateCharacter(%q) error = %v, want ErrNameTaken", duplicate, err)
		}
	}
	// Another account cannot take the name either: uniqueness is global.
	other, err := service.Register(ctx, secret.New("other@example.invalid"), secret.New(testPassword))
	if err != nil {
		t.Fatalf("Register(other) error = %v", err)
	}
	if _, err := service.CreateCharacter(ctx, other, "Anne", "chargen.league.warrior"); !errors.Is(err, charstore.ErrNameTaken) {
		t.Errorf("CreateCharacter(other account, same name) error = %v, want ErrNameTaken", err)
	}
	// And the shape and blocklist rules refuse before anything is inserted.
	if _, err := service.CreateCharacter(ctx, accountID, "Ann3", "chargen.league.warrior"); !errors.Is(err, ErrNameInvalid) {
		t.Errorf("CreateCharacter(digit) error = %v, want ErrNameInvalid", err)
	}
	if _, err := service.CreateCharacter(ctx, accountID, "Gmsomebody", "chargen.league.warrior"); !errors.Is(err, ErrNameBlocked) {
		t.Errorf("CreateCharacter(blocked) error = %v, want ErrNameBlocked", err)
	}
}

func TestListCreateDeleteRoundTrip(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	accountID, _ := registerAndLogin(t, service)
	ctx := t.Context()

	characters, err := service.ListCharacters(ctx, accountID)
	if err != nil {
		t.Fatalf("ListCharacters() error = %v", err)
	}
	if len(characters) != 0 {
		t.Fatalf("a fresh account has %d characters, want none", len(characters))
	}

	created, err := service.CreateCharacter(ctx, accountID, "Anne", "chargen.league.warrior")
	if err != nil {
		t.Fatalf("CreateCharacter() error = %v", err)
	}
	characters, err = service.ListCharacters(ctx, accountID)
	if err != nil {
		t.Fatalf("ListCharacters() error = %v", err)
	}
	if len(characters) != 1 || characters[0].CharacterID != created.CharacterID {
		t.Fatalf("ListCharacters() = %+v, want the created character", characters)
	}

	if err := service.DeleteCharacter(ctx, accountID, created.CharacterID); err != nil {
		t.Fatalf("DeleteCharacter() error = %v", err)
	}
	characters, err = service.ListCharacters(ctx, accountID)
	if err != nil {
		t.Fatalf("ListCharacters() error = %v", err)
	}
	if len(characters) != 0 {
		t.Errorf("ListCharacters() after delete = %+v, want none", characters)
	}
	// A deleted character's name stays taken indefinitely in M2.
	if _, err := service.CreateCharacter(ctx, accountID, "Anne", "chargen.league.warrior"); !errors.Is(err, charstore.ErrNameTaken) {
		t.Errorf("CreateCharacter() after delete error = %v, want ErrNameTaken", err)
	}
	// Deleting twice, and deleting somebody else's character, are the same
	// answer: not found.
	if err := service.DeleteCharacter(ctx, accountID, created.CharacterID); !errors.Is(err, ErrCharacterNotFound) {
		t.Errorf("DeleteCharacter() twice error = %v, want ErrCharacterNotFound", err)
	}
}

func TestDeleteRefusesAnotherAccountsCharacter(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	accountA, _ := registerAndLogin(t, service)
	ctx := t.Context()
	accountB, err := service.Register(ctx, secret.New("b@example.invalid"), secret.New(testPassword))
	if err != nil {
		t.Fatalf("Register(B) error = %v", err)
	}

	character, err := service.CreateCharacter(ctx, accountA, "Anne", "chargen.league.warrior")
	if err != nil {
		t.Fatalf("CreateCharacter() error = %v", err)
	}
	if err := service.DeleteCharacter(ctx, accountB, character.CharacterID); !errors.Is(err, ErrCharacterNotFound) {
		t.Errorf("DeleteCharacter(other account) error = %v, want ErrCharacterNotFound", err)
	}
	// It is still there.
	characters, err := service.ListCharacters(ctx, accountA)
	if err != nil || len(characters) != 1 {
		t.Errorf("the character was deleted by another account: %+v, %v", characters, err)
	}
}

func TestCheckNameReservesForTheAskingAccountOnly(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	accountA, _ := registerAndLogin(t, service)
	ctx := t.Context()
	accountB, err := service.Register(ctx, secret.New("b@example.invalid"), secret.New(testPassword))
	if err != nil {
		t.Fatalf("Register(B) error = %v", err)
	}

	check, err := service.CheckName(ctx, accountA, "Anne")
	if err != nil || !check.Available {
		t.Fatalf("CheckName() = %+v, %v; want available", check, err)
	}
	// The same account may re-check while it types.
	if again, err := service.CheckName(ctx, accountA, "Anne"); err != nil || !again.Available {
		t.Errorf("CheckName() again = %+v, %v; want available to the holder", again, err)
	}
	// Another account is told it is reserved rather than being beaten to it.
	if other, err := service.CheckName(ctx, accountB, "Anne"); err != nil || other.Available || other.Reason != "NAME_RESERVED" {
		t.Errorf("CheckName(other) = %+v, %v; want NAME_RESERVED", other, err)
	}
	// A name that fails shape or blocklist never reaches the reservation.
	if invalid, _ := service.CheckName(ctx, accountA, "Ann3"); invalid.Available || invalid.Reason != "NAME_INVALID" {
		t.Errorf("CheckName(Ann3) = %+v, want NAME_INVALID", invalid)
	}
	if blocked, _ := service.CheckName(ctx, accountA, "Adminia"); blocked.Available || blocked.Reason != "NAME_BLOCKED" {
		t.Errorf("CheckName(Adminia) = %+v, want NAME_BLOCKED", blocked)
	}

	// Creating consumes the reservation, and an existing character makes the
	// name taken rather than reserved.
	if _, err := service.CreateCharacter(ctx, accountA, "Anne", "chargen.league.warrior"); err != nil {
		t.Fatalf("CreateCharacter() error = %v", err)
	}
	if taken, _ := service.CheckName(ctx, accountB, "Anne"); taken.Available || taken.Reason != "NAME_TAKEN" {
		t.Errorf("CheckName() after creation = %+v, want NAME_TAKEN", taken)
	}
}

func TestReservationsExpire(t *testing.T) {
	t.Parallel()

	service, clock := newTestService(t)
	accountA, _ := registerAndLogin(t, service)
	ctx := t.Context()
	accountB, err := service.Register(ctx, secret.New("b@example.invalid"), secret.New(testPassword))
	if err != nil {
		t.Fatalf("Register(B) error = %v", err)
	}

	if _, err := service.CheckName(ctx, accountA, "Anne"); err != nil {
		t.Fatalf("CheckName() error = %v", err)
	}
	clock.advance(NameReservationWindow + time.Second)
	check, err := service.CheckName(ctx, accountB, "Anne")
	if err != nil || !check.Available {
		t.Errorf("CheckName() after expiry = %+v, %v; want available", check, err)
	}
}
