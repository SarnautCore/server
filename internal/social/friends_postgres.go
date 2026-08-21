package social

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresFriendRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresFriendRepository(pool *pgxpool.Pool) (*PostgresFriendRepository, error) {
	if pool == nil {
		return nil, errors.New("social friends: postgres pool is required")
	}
	return &PostgresFriendRepository{pool: pool}, nil
}

func (repository *PostgresFriendRepository) Friends(
	ctx context.Context,
	owner uuid.UUID,
) (FriendsReplacement, error) {
	if owner == uuid.Nil {
		return FriendsReplacement{}, ErrInvalidFriendOwner
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return FriendsReplacement{}, fmt.Errorf("begin friend snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	replacement, err := readPostgresFriends(ctx, tx, owner)
	if err != nil {
		return FriendsReplacement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FriendsReplacement{}, fmt.Errorf("commit friend snapshot: %w", err)
	}
	return replacement, nil
}

func (repository *PostgresFriendRepository) ReplaceFriends(
	ctx context.Context,
	owner uuid.UUID,
	friendIDs []uuid.UUID,
) (FriendsReplacement, error) {
	if err := validateFriendIDs(owner, friendIDs); err != nil {
		return FriendsReplacement{}, err
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return FriendsReplacement{}, fmt.Errorf("begin friend replacement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Resolve every friend through the canonical character table before the
	// relationship changes. Deleted or unknown characters fail the replacement
	// atomically; display names are never accepted from a caller.
	if len(friendIDs) != 0 {
		rows, err := tx.Query(ctx, `
			SELECT character_id
			FROM auth.characters
			WHERE character_id = ANY($1::uuid[]) AND deleted_at IS NULL`, friendIDs)
		if err != nil {
			return FriendsReplacement{}, fmt.Errorf("resolve friend characters: %w", err)
		}
		found := make(map[uuid.UUID]struct{}, len(friendIDs))
		for rows.Next() {
			var characterID uuid.UUID
			if err := rows.Scan(&characterID); err != nil {
				rows.Close()
				return FriendsReplacement{}, fmt.Errorf("scan friend character: %w", err)
			}
			found[characterID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return FriendsReplacement{}, fmt.Errorf("read friend characters: %w", err)
		}
		rows.Close()
		if len(found) != len(friendIDs) {
			return FriendsReplacement{}, ErrInvalidFriend
		}
	}

	var revision int64
	err = tx.QueryRow(ctx, `
		INSERT INTO shard.social_friend_sets (owner_character_id, revision, updated_at)
		VALUES ($1, 1, now())
		ON CONFLICT (owner_character_id) DO UPDATE SET
			revision = social_friend_sets.revision + 1,
			updated_at = now()
		WHERE social_friend_sets.revision < $2
		RETURNING revision`, owner, int64(math.MaxInt64)).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return FriendsReplacement{}, ErrFriendRevision
	}
	if err != nil {
		return FriendsReplacement{}, fmt.Errorf("advance friend revision: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM shard.social_friends WHERE owner_character_id = $1`, owner); err != nil {
		return FriendsReplacement{}, fmt.Errorf("clear friend relationships: %w", err)
	}
	if len(friendIDs) != 0 {
		rows := make([][]any, 0, len(friendIDs))
		for position, friendID := range friendIDs {
			rows = append(rows, []any{owner, friendID, position})
		}
		if _, err := tx.CopyFrom(
			ctx,
			pgx.Identifier{"shard", "social_friends"},
			[]string{"owner_character_id", "friend_character_id", "roster_position"},
			pgx.CopyFromRows(rows),
		); err != nil {
			return FriendsReplacement{}, fmt.Errorf("insert friend relationships: %w", err)
		}
	}
	replacement, err := readPostgresFriends(ctx, tx, owner)
	if err != nil {
		return FriendsReplacement{}, err
	}
	if replacement.Revision != uint64(revision) {
		return FriendsReplacement{}, errors.New("social friends: transaction revision mismatch")
	}
	if err := tx.Commit(ctx); err != nil {
		return FriendsReplacement{}, fmt.Errorf("commit friend replacement: %w", err)
	}
	return replacement, nil
}

type friendQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readPostgresFriends(
	ctx context.Context,
	querier friendQuerier,
	owner uuid.UUID,
) (FriendsReplacement, error) {
	var revision int64
	err := querier.QueryRow(ctx, `
		SELECT revision
		FROM shard.social_friend_sets
		WHERE owner_character_id = $1`, owner).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return FriendsReplacement{}, nil
	}
	if err != nil {
		return FriendsReplacement{}, fmt.Errorf("read friend revision: %w", err)
	}
	rows, err := querier.Query(ctx, `
		SELECT relationships.friend_character_id, characters.name
		FROM shard.social_friends AS relationships
		JOIN auth.characters AS characters
			ON characters.character_id = relationships.friend_character_id
			AND characters.deleted_at IS NULL
		WHERE relationships.owner_character_id = $1
		ORDER BY relationships.roster_position`, owner)
	if err != nil {
		return FriendsReplacement{}, fmt.Errorf("query friend relationships: %w", err)
	}
	defer rows.Close()
	replacement := FriendsReplacement{Revision: uint64(revision)}
	for rows.Next() {
		var friend Friend
		if err := rows.Scan(&friend.CharacterID, &friend.DisplayName); err != nil {
			return FriendsReplacement{}, fmt.Errorf("scan friend relationship: %w", err)
		}
		replacement.Friends = append(replacement.Friends, friend)
	}
	if err := rows.Err(); err != nil {
		return FriendsReplacement{}, fmt.Errorf("read friend relationships: %w", err)
	}
	return replacement, nil
}

var _ FriendRepository = (*PostgresFriendRepository)(nil)
