-- Friend relationships are durable and keyed by character UUID. The client
-- receives full name replacements, but names remain authoritative in
-- auth.characters and are never copied from a client payload.

-- +goose Up

CREATE TABLE shard.social_friend_sets (
    owner_character_id uuid PRIMARY KEY,
    revision           bigint NOT NULL,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT social_friend_sets_revision_check CHECK (revision >= 0)
);

CREATE TABLE shard.social_friends (
    owner_character_id  uuid NOT NULL,
    friend_character_id uuid NOT NULL,
    roster_position     integer NOT NULL,
    PRIMARY KEY (owner_character_id, friend_character_id),
    CONSTRAINT social_friends_distinct_check CHECK (owner_character_id <> friend_character_id),
    CONSTRAINT social_friends_position_check CHECK (roster_position >= 0),
    CONSTRAINT social_friends_owner_position_key UNIQUE (owner_character_id, roster_position)
);

CREATE INDEX social_friends_friend_character_id_idx
    ON shard.social_friends (friend_character_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON shard.social_friend_sets TO sarnaut_shard;
GRANT SELECT, INSERT, UPDATE, DELETE ON shard.social_friends TO sarnaut_shard;

-- +goose Down

DROP TABLE IF EXISTS shard.social_friends;
DROP TABLE IF EXISTS shard.social_friend_sets;
