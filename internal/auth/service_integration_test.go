//go:build integration

package auth_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SarnautCore/server/internal/auth"
	"github.com/SarnautCore/server/internal/auth/credential"
	"github.com/SarnautCore/server/internal/auth/secret"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/pack"
)

// postgresDSNEnvironment is the same variable every service reads.
const postgresDSNEnvironment = "SARNAUT_POSTGRES_DSN"

// These tests run against the CI Postgres service container. The behaviour they
// cover is the database's, not the service's: the citext unique index on
// email, the unique index on name_normalized that decides NAME_TAKEN, the soft
// delete, and the reservation table's conflict rule. An in-memory store can
// imitate all four, and imitating them is not evidence.
func requirePool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(postgresDSNEnvironment)
	if dsn == "" {
		t.Skipf("skipping database-backed test: %s is not set", postgresDSNEnvironment)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func newIntegrationService(t *testing.T) *auth.Service {
	t.Helper()

	pool := requirePool(t)
	repository, err := charstore.NewPostgres(pool)
	if err != nil {
		t.Fatalf("NewPostgres() error = %v", err)
	}
	catalogue, err := auth.NewCatalogue(loadFixtureChargen(t))
	if err != nil {
		t.Fatalf("NewCatalogue() error = %v", err)
	}
	service, err := auth.New(auth.Options{
		Repository: repository,
		Keys:       auth.NewMemoryKeyValue(),
		Catalogue:  catalogue,
		Hasher: credential.Hasher{Params: credential.Params{
			Time: 1, Memory: 8, Parallelism: 1, SaltLength: 16, KeyLength: 32,
		}},
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return service
}

// loadFixtureChargen reads the vendored golden pack, so the option this test
// creates a character with is the one the shard would materialize.
func loadFixtureChargen(t *testing.T) []pack.ChargenOption {
	t.Helper()
	content, err := pack.Load("../../testdata/packs/demo", pack.Options{})
	if err != nil {
		t.Fatalf("pack.Load() error = %v", err)
	}
	options := content.ChargenOptions()
	if len(options) == 0 {
		t.Fatal("the fixture pack carries no chargen options")
	}
	return options
}

// uniqueEmail keeps repeated runs against a persistent database from colliding
// on the email unique index.
func uniqueEmail(t *testing.T) secret.Value {
	t.Helper()
	return secret.New("it-" + uuid.NewString() + "@example.invalid")
}

// uniqueName produces a name that satisfies the ADR 0032 shape rules and is
// unlikely to collide: an uppercase letter followed by lowercase letters drawn
// from a fresh uuid.
func uniqueName(t *testing.T) string {
	t.Helper()
	raw := uuid.NewString()
	letters := []rune{}
	for _, character := range raw {
		if character >= 'a' && character <= 'f' {
			letters = append(letters, character)
		}
		if len(letters) == 12 {
			break
		}
	}
	for len(letters) < 6 {
		letters = append(letters, 'a')
	}
	name := string(letters)
	return strings.ToUpper(name[:1]) + name[1:]
}

func TestRegisterLoginCreateListDeleteAgainstPostgres(t *testing.T) {
	service := newIntegrationService(t)
	ctx := t.Context()
	email := uniqueEmail(t)
	password := secret.New("integration-password")

	accountID, err := service.Register(ctx, email, password)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	// The email column is citext: a different case is the same account.
	if _, err := service.Register(ctx, secret.New(strings.ToUpper(email.Reveal())), password); !errors.Is(err, charstore.ErrEmailTaken) {
		t.Errorf("Register(same email, different case) error = %v, want ErrEmailTaken", err)
	}

	session, err := service.Login(ctx, email, password)
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if session.AccountID != accountID {
		t.Fatalf("Login() account = %s, want %s", session.AccountID, accountID)
	}
	if _, err := service.Login(ctx, email, secret.New("wrong-password")); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("Login(wrong password) error = %v, want ErrInvalidCredentials", err)
	}

	options := service.PlayableOptions()
	if len(options) == 0 {
		t.Fatal("the catalogue offers no playable option")
	}
	name := uniqueName(t)
	character, err := service.CreateCharacter(ctx, accountID, name, options[0].ID)
	if err != nil {
		t.Fatalf("CreateCharacter() error = %v", err)
	}

	// The unique index on name_normalized is the only authority on uniqueness,
	// and it is what maps SQLSTATE 23505 onto NAME_TAKEN.
	if _, err := service.CreateCharacter(ctx, accountID, name, options[0].ID); !errors.Is(err, charstore.ErrNameTaken) {
		t.Errorf("CreateCharacter(duplicate) error = %v, want ErrNameTaken", err)
	}
	punctuated := string(name[0]) + "'" + name[1:]
	if len(punctuated) <= auth.MaximumNameLength {
		if _, err := service.CreateCharacter(ctx, accountID, punctuated, options[0].ID); !errors.Is(err, charstore.ErrNameTaken) {
			t.Errorf("CreateCharacter(%q) error = %v, want ErrNameTaken from normalization", punctuated, err)
		}
	}

	roster, err := service.ListCharacters(ctx, accountID)
	if err != nil {
		t.Fatalf("ListCharacters() error = %v", err)
	}
	if len(roster) != 1 || roster[0].CharacterID != character.CharacterID {
		t.Fatalf("ListCharacters() = %+v, want the created character", roster)
	}

	// A ticket for it round-trips through redemption.
	ticket, err := service.MintTicket(ctx, accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	admission, err := service.RedeemTicket(ctx, ticket.Token, "integration-shard")
	if err != nil {
		t.Fatalf("RedeemTicket() error = %v", err)
	}
	if admission.CharacterID != character.CharacterID || admission.ChargenOptionID != options[0].ID {
		t.Errorf("RedeemTicket() = %+v, want the created character", admission)
	}

	if err := service.DeleteCharacter(ctx, accountID, character.CharacterID); err != nil {
		t.Fatalf("DeleteCharacter() error = %v", err)
	}
	roster, err = service.ListCharacters(ctx, accountID)
	if err != nil {
		t.Fatalf("ListCharacters() error = %v", err)
	}
	if len(roster) != 0 {
		t.Errorf("ListCharacters() after delete = %+v, want none", roster)
	}
	// The soft delete keeps the name taken.
	if _, err := service.CreateCharacter(ctx, accountID, name, options[0].ID); !errors.Is(err, charstore.ErrNameTaken) {
		t.Errorf("CreateCharacter() after delete error = %v, want ErrNameTaken", err)
	}
}

func TestAnotherAccountCannotSeeOrTakeACharacterAgainstPostgres(t *testing.T) {
	service := newIntegrationService(t)
	ctx := t.Context()
	password := secret.New("integration-password")

	accountA, err := service.Register(ctx, uniqueEmail(t), password)
	if err != nil {
		t.Fatalf("Register(A) error = %v", err)
	}
	accountB, err := service.Register(ctx, uniqueEmail(t), password)
	if err != nil {
		t.Fatalf("Register(B) error = %v", err)
	}

	options := service.PlayableOptions()
	character, err := service.CreateCharacter(ctx, accountA, uniqueName(t), options[0].ID)
	if err != nil {
		t.Fatalf("CreateCharacter() error = %v", err)
	}

	if _, err := service.MintTicket(ctx, accountB, character.CharacterID); !errors.Is(err, auth.ErrCharacterNotFound) {
		t.Errorf("MintTicket(other account) error = %v, want ErrCharacterNotFound", err)
	}
	if err := service.DeleteCharacter(ctx, accountB, character.CharacterID); !errors.Is(err, auth.ErrCharacterNotFound) {
		t.Errorf("DeleteCharacter(other account) error = %v, want ErrCharacterNotFound", err)
	}
	roster, err := service.ListCharacters(ctx, accountB)
	if err != nil {
		t.Fatalf("ListCharacters(B) error = %v", err)
	}
	if len(roster) != 0 {
		t.Errorf("account B sees %d characters, want none", len(roster))
	}
	// A's character survived both attempts.
	roster, err = service.ListCharacters(ctx, accountA)
	if err != nil || len(roster) != 1 {
		t.Errorf("account A's roster = %+v, %v; want the one character", roster, err)
	}
}

func TestNameReservationsAgainstPostgres(t *testing.T) {
	service := newIntegrationService(t)
	ctx := t.Context()
	password := secret.New("integration-password")

	accountA, err := service.Register(ctx, uniqueEmail(t), password)
	if err != nil {
		t.Fatalf("Register(A) error = %v", err)
	}
	accountB, err := service.Register(ctx, uniqueEmail(t), password)
	if err != nil {
		t.Fatalf("Register(B) error = %v", err)
	}

	name := uniqueName(t)
	check, err := service.CheckName(ctx, accountA, name)
	if err != nil || !check.Available {
		t.Fatalf("CheckName() = %+v, %v; want available", check, err)
	}
	if other, err := service.CheckName(ctx, accountB, name); err != nil || other.Available {
		t.Errorf("CheckName(other account) = %+v, %v; want unavailable while reserved", other, err)
	}
	if again, err := service.CheckName(ctx, accountA, name); err != nil || !again.Available {
		t.Errorf("CheckName(holder) = %+v, %v; want available to the holder", again, err)
	}

	if _, err := service.CreateCharacter(ctx, accountA, name, service.PlayableOptions()[0].ID); err != nil {
		t.Fatalf("CreateCharacter() error = %v", err)
	}
	if taken, err := service.CheckName(ctx, accountB, name); err != nil || taken.Reason != "NAME_TAKEN" {
		t.Errorf("CheckName() after creation = %+v, %v; want NAME_TAKEN", taken, err)
	}
}
