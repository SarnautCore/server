//go:build integration

package cohort_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/cohort"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const cohortPostgresDSN = "SARNAUT_POSTGRES_DSN"

func TestPostgresRostersSurviveRepositoryReconstruction(t *testing.T) {
	dsn := os.Getenv(cohortPostgresDSN)
	if dsn == "" {
		t.Skipf("skipping database-backed cohort test: %s is not set", cohortPostgresDSN)
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

	firstRepository, err := cohort.NewPostgresRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	first, second := uuid.New(), uuid.New()
	guildID, raidID := uuid.New(), uuid.New()
	rights := cohort.MustRights(cohort.GMRChat, cohort.GMROfficerChat)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, _ = pool.Exec(cleanup, `DELETE FROM shard.guild_memberships WHERE guild_id = $1`, guildID)
		_, _ = pool.Exec(cleanup, `DELETE FROM shard.raid_memberships WHERE raid_id = $1`, raidID)
		pool.Close()
	})

	if err := firstRepository.ReplaceGuild(ctx, cohort.GuildRoster{
		GuildID: guildID,
		Members: []cohort.GuildMember{
			{CharacterID: first, Rights: rights},
			{CharacterID: second, Rights: cohort.MustRights(cohort.GMRChat)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := firstRepository.ReplaceRaid(ctx, cohort.RaidRoster{
		RaidID: raidID, Members: []uuid.UUID{first, second},
	}); err != nil {
		t.Fatal(err)
	}

	reconstructed, err := cohort.NewPostgresRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	guild, err := reconstructed.GuildByMember(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if guild.GuildID != guildID || len(guild.Members) != 2 || guild.Members[0].Rights != rights {
		t.Fatalf("reconstructed guild = %+v", guild)
	}
	raid, err := reconstructed.RaidByMember(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if raid.RaidID != raidID || len(raid.Members) != 2 || raid.Members[0] != first {
		t.Fatalf("reconstructed raid = %+v", raid)
	}
}
