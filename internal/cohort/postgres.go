package cohort

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresRepository persists guild and raid rosters in the shard schema. It
// owns neither the pool nor migration lifetime.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) (*PostgresRepository, error) {
	if pool == nil {
		return nil, errors.New("cohort repository: postgres pool is required")
	}
	return &PostgresRepository{pool: pool}, nil
}

func (repository *PostgresRepository) GuildByMember(
	ctx context.Context,
	characterID uuid.UUID,
) (GuildRoster, error) {
	const statement = `
		SELECT roster.guild_id, roster.character_id, roster.chat, roster.officer_chat
		FROM shard.guild_memberships AS anchor
		JOIN shard.guild_memberships AS roster ON roster.guild_id = anchor.guild_id
		WHERE anchor.character_id = $1
		ORDER BY roster.roster_position`
	rows, err := repository.pool.Query(ctx, statement, characterID)
	if err != nil {
		return GuildRoster{}, fmt.Errorf("query guild roster: %w", err)
	}
	defer rows.Close()

	var result GuildRoster
	for rows.Next() {
		var (
			guildID, memberID uuid.UUID
			chat, officer     bool
		)
		if err := rows.Scan(&guildID, &memberID, &chat, &officer); err != nil {
			return GuildRoster{}, fmt.Errorf("scan guild roster: %w", err)
		}
		result.GuildID = guildID
		var rights GuildRights
		if chat {
			rights |= 1 << GMRChat
		}
		if officer {
			rights |= 1 << GMROfficerChat
		}
		result.Members = append(result.Members, GuildMember{CharacterID: memberID, Rights: rights})
	}
	if err := rows.Err(); err != nil {
		return GuildRoster{}, fmt.Errorf("read guild roster: %w", err)
	}
	if result.GuildID == uuid.Nil {
		return GuildRoster{}, ErrNotFound
	}
	return result, nil
}

func (repository *PostgresRepository) RaidByMember(
	ctx context.Context,
	characterID uuid.UUID,
) (RaidRoster, error) {
	const statement = `
		SELECT roster.raid_id, roster.character_id
		FROM shard.raid_memberships AS anchor
		JOIN shard.raid_memberships AS roster ON roster.raid_id = anchor.raid_id
		WHERE anchor.character_id = $1
		ORDER BY roster.roster_position`
	rows, err := repository.pool.Query(ctx, statement, characterID)
	if err != nil {
		return RaidRoster{}, fmt.Errorf("query raid roster: %w", err)
	}
	defer rows.Close()

	var result RaidRoster
	for rows.Next() {
		var raidID, memberID uuid.UUID
		if err := rows.Scan(&raidID, &memberID); err != nil {
			return RaidRoster{}, fmt.Errorf("scan raid roster: %w", err)
		}
		result.RaidID = raidID
		result.Members = append(result.Members, memberID)
	}
	if err := rows.Err(); err != nil {
		return RaidRoster{}, fmt.Errorf("read raid roster: %w", err)
	}
	if result.RaidID == uuid.Nil {
		return RaidRoster{}, ErrNotFound
	}
	return result, nil
}

func (repository *PostgresRepository) ReplaceGuild(ctx context.Context, roster GuildRoster) error {
	if err := validateGuild(roster); err != nil {
		return err
	}
	return repository.inSerializableTransaction(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM shard.guild_memberships WHERE guild_id = $1`, roster.GuildID); err != nil {
			return fmt.Errorf("clear guild roster: %w", err)
		}
		const statement = `
			INSERT INTO shard.guild_memberships (
				character_id, guild_id, roster_position, chat, officer_chat
			) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (character_id) DO UPDATE SET
				guild_id = EXCLUDED.guild_id,
				roster_position = EXCLUDED.roster_position,
				chat = EXCLUDED.chat,
				officer_chat = EXCLUDED.officer_chat`
		for position, member := range roster.Members {
			_, err := tx.Exec(
				ctx,
				statement,
				member.CharacterID,
				roster.GuildID,
				position,
				member.Rights.Has(GMRChat),
				member.Rights.Has(GMROfficerChat),
			)
			if err != nil {
				return fmt.Errorf("write guild member %s: %w", member.CharacterID, err)
			}
		}
		return nil
	})
}

func (repository *PostgresRepository) ReplaceRaid(ctx context.Context, roster RaidRoster) error {
	if err := validateRaid(roster); err != nil {
		return err
	}
	return repository.inSerializableTransaction(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM shard.raid_memberships WHERE raid_id = $1`, roster.RaidID); err != nil {
			return fmt.Errorf("clear raid roster: %w", err)
		}
		const statement = `
			INSERT INTO shard.raid_memberships (character_id, raid_id, roster_position)
			VALUES ($1, $2, $3)
			ON CONFLICT (character_id) DO UPDATE SET
				raid_id = EXCLUDED.raid_id,
				roster_position = EXCLUDED.roster_position`
		for position, memberID := range roster.Members {
			if _, err := tx.Exec(ctx, statement, memberID, roster.RaidID, position); err != nil {
				return fmt.Errorf("write raid member %s: %w", memberID, err)
			}
		}
		return nil
	})
}

func (repository *PostgresRepository) inSerializableTransaction(
	ctx context.Context,
	work func(pgx.Tx) error,
) error {
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("begin cohort transaction: %w", err)
	}
	if err := work(tx); err != nil {
		return errors.Join(err, ignoreClosed(tx.Rollback(ctx)))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit cohort transaction: %w", err)
	}
	return nil
}

func ignoreClosed(err error) error {
	if err == nil || errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return fmt.Errorf("roll back cohort transaction: %w", err)
}

var _ Repository = (*PostgresRepository)(nil)
