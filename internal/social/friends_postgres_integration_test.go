//go:build integration

package social_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/social"
)

func TestPostgresFriendsPersistCanonicalNamesAndSerializeRevisions(t *testing.T) {
	dsn := os.Getenv("SARNAUT_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SARNAUT_POSTGRES_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	migrator, err := charstore.NewMigrator(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrator.Close() }()
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	accountID, owner, first, second := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO auth.accounts (account_id, email, password_hash)
		VALUES ($1, $2, 'hash')`, accountID, accountID.String()+"@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	for position, character := range []struct {
		id   uuid.UUID
		name string
	}{{owner, "Owner"}, {first, "CanonicalOne"}, {second, "CanonicalTwo"}} {
		_, err = pool.Exec(ctx, `
			INSERT INTO auth.characters (
				character_id, account_id, name, name_normalized, chargen_option_id
			) VALUES ($1, $2, $3, $4, 'chargen.fixture')`,
			character.id, accountID, character.name, character.name+string(rune('a'+position)))
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, _ = pool.Exec(cleanup, `DELETE FROM shard.social_friends WHERE owner_character_id = $1`, owner)
		_, _ = pool.Exec(cleanup, `DELETE FROM shard.social_friend_sets WHERE owner_character_id = $1`, owner)
		_, _ = pool.Exec(cleanup, `DELETE FROM auth.accounts WHERE account_id = $1`, accountID)
	})

	repository, err := social.NewPostgresFriendRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	firstReplacement, err := repository.ReplaceFriends(ctx, owner, []uuid.UUID{second, first})
	if err != nil {
		t.Fatal(err)
	}
	if firstReplacement.Revision != 1 || firstReplacement.Friends[0].DisplayName != "CanonicalTwo" ||
		firstReplacement.Friends[1].DisplayName != "CanonicalOne" {
		t.Fatalf("first replacement = %+v", firstReplacement)
	}

	const concurrent = 24
	revisions := make(chan uint64, concurrent)
	errors := make(chan error, concurrent)
	for index := 0; index < concurrent; index++ {
		go func(include bool) {
			ids := []uuid.UUID(nil)
			if include {
				ids = []uuid.UUID{first}
			}
			replacement, err := repository.ReplaceFriends(ctx, owner, ids)
			if err != nil {
				errors <- err
				return
			}
			revisions <- replacement.Revision
		}(index%2 == 0)
	}
	seen := make(map[uint64]struct{}, concurrent)
	for index := 0; index < concurrent; index++ {
		select {
		case err := <-errors:
			t.Fatal(err)
		case revision := <-revisions:
			seen[revision] = struct{}{}
		}
	}
	for revision := uint64(2); revision <= concurrent+1; revision++ {
		if _, ok := seen[revision]; !ok {
			t.Fatalf("missing committed revision %d from %+v", revision, seen)
		}
	}

	reconstructed, _ := social.NewPostgresFriendRepository(pool)
	snapshot, err := reconstructed.Friends(ctx, owner)
	if err != nil || snapshot.Revision != concurrent+1 {
		t.Fatalf("reconstructed snapshot = %+v, %v", snapshot, err)
	}
}
