-- ADR 0031 §4: two schemas in one database, no cross-schema foreign keys, one
-- owning module per table. `auth` belongs to the auth service, `shard` to the
-- shard's character store.

-- +goose Up

-- citext carries the case-insensitive unique indexes on email and normalized
-- character name. It is a trusted extension since PostgreSQL 13, so the database
-- owner may create it without superuser rights.
CREATE EXTENSION IF NOT EXISTS citext;

CREATE SCHEMA IF NOT EXISTS auth;
CREATE SCHEMA IF NOT EXISTS shard;

-- Ownership is enforced twice: by the import-graph check in code, and by these
-- roles in the database. A stray query from the shard against auth.accounts
-- fails with a permission error instead of quietly working.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'sarnaut_auth') THEN
        CREATE ROLE sarnaut_auth NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'sarnaut_shard') THEN
        CREATE ROLE sarnaut_shard NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

CREATE TABLE auth.accounts (
    account_id    uuid PRIMARY KEY,
    email         citext NOT NULL,
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    disabled_at   timestamptz,
    CONSTRAINT accounts_email_key UNIQUE (email)
);

-- ADR 0032 §3: uniqueness of a character name is enforced by this index and by
-- nothing else. Creation does a plain INSERT and maps SQLSTATE 23505 to
-- NAME_TAKEN; there is no check-then-insert path, because that is a race.
CREATE TABLE auth.characters (
    character_id      uuid PRIMARY KEY,
    account_id        uuid NOT NULL REFERENCES auth.accounts (account_id) ON DELETE CASCADE,
    name              text NOT NULL,
    name_normalized   citext NOT NULL,
    chargen_option_id text NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz,
    CONSTRAINT characters_name_normalized_key UNIQUE (name_normalized),
    CONSTRAINT characters_name_length_check CHECK (char_length(name) BETWEEN 3 AND 16)
);

CREATE INDEX characters_account_id_idx ON auth.characters (account_id, created_at);

-- A courtesy for the creation form, not an authority (ADR 0032 §3). Expired rows
-- are ignored on read and swept lazily.
CREATE TABLE auth.name_reservations (
    name_normalized citext PRIMARY KEY,
    account_id      uuid NOT NULL,
    reserved_until  timestamptz NOT NULL
);

-- Deliberately no foreign key to auth.characters: the shard never reads the auth
-- schema and learns character_id from ticket redemption only (ADR 0030 §4).
CREATE TABLE shard.character_state (
    character_id uuid PRIMARY KEY,
    zone_id      text NOT NULL,
    position_x   real NOT NULL,
    position_y   real NOT NULL,
    position_z   real NOT NULL,
    heading      real NOT NULL,
    level        integer NOT NULL,
    experience   bigint NOT NULL,
    health       integer NOT NULL,
    -- A save whose save_seq does not advance is rejected, so a slow write from a
    -- dying session cannot clobber a newer write from a reconnect (ADR 0031 §6).
    save_seq     bigint NOT NULL,
    saved_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT character_state_save_seq_check CHECK (save_seq >= 0),
    CONSTRAINT character_state_level_check CHECK (level >= 1),
    CONSTRAINT character_state_experience_check CHECK (experience >= 0)
);

CREATE TABLE shard.character_inventory (
    character_id uuid NOT NULL,
    slot         integer NOT NULL,
    item_id      text NOT NULL,
    quantity     integer NOT NULL,
    PRIMARY KEY (character_id, slot),
    CONSTRAINT character_inventory_slot_check CHECK (slot >= 0),
    CONSTRAINT character_inventory_quantity_check CHECK (quantity > 0)
);

CREATE TABLE shard.character_quests (
    character_id uuid NOT NULL,
    quest_id     text NOT NULL,
    state        text NOT NULL,
    objectives   jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (character_id, quest_id)
);

GRANT USAGE ON SCHEMA auth TO sarnaut_auth;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA auth TO sarnaut_auth;

GRANT USAGE ON SCHEMA shard TO sarnaut_shard;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA shard TO sarnaut_shard;

-- +goose Down

-- CASCADE takes the tables, indexes and grants with the schema. The roles are
-- cluster-global, so their remaining privileges are dropped explicitly before
-- the roles themselves.
DROP SCHEMA IF EXISTS shard CASCADE;
DROP SCHEMA IF EXISTS auth CASCADE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'sarnaut_shard') THEN
        EXECUTE 'DROP OWNED BY sarnaut_shard';
        DROP ROLE sarnaut_shard;
    END IF;
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'sarnaut_auth') THEN
        EXECUTE 'DROP OWNED BY sarnaut_auth';
        DROP ROLE sarnaut_auth;
    END IF;
END
$$;
-- +goose StatementEnd

-- citext is left in place. Dropping it would take any column outside these two
-- schemas that happens to use it, and it is cheap to keep.
