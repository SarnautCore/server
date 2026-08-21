-- Durable membership is shard authority. Live connection state remains in
-- memory and is never persisted.

-- +goose Up

CREATE TABLE shard.guild_memberships (
    character_id   uuid PRIMARY KEY,
    guild_id       uuid NOT NULL,
    roster_position integer NOT NULL,
    chat           boolean NOT NULL,
    officer_chat   boolean NOT NULL,
    CONSTRAINT guild_memberships_position_check CHECK (roster_position >= 0),
    CONSTRAINT guild_memberships_guild_position_key UNIQUE (guild_id, roster_position)
);

CREATE INDEX guild_memberships_guild_id_idx
    ON shard.guild_memberships (guild_id, roster_position);

CREATE TABLE shard.raid_memberships (
    character_id   uuid PRIMARY KEY,
    raid_id        uuid NOT NULL,
    roster_position integer NOT NULL,
    CONSTRAINT raid_memberships_position_check CHECK (roster_position >= 0),
    CONSTRAINT raid_memberships_raid_position_key UNIQUE (raid_id, roster_position)
);

CREATE INDEX raid_memberships_raid_id_idx
    ON shard.raid_memberships (raid_id, roster_position);

GRANT SELECT, INSERT, UPDATE, DELETE ON shard.guild_memberships TO sarnaut_shard;
GRANT SELECT, INSERT, UPDATE, DELETE ON shard.raid_memberships TO sarnaut_shard;

-- +goose Down

DROP TABLE IF EXISTS shard.raid_memberships;
DROP TABLE IF EXISTS shard.guild_memberships;
