-- +goose Up

CREATE TABLE shard.deferred_script_impacts (
    id              text PRIMARY KEY,
    zone_id         text NOT NULL CHECK (zone_id <> ''),
    scope_id        text NOT NULL CHECK (scope_id <> ''),
    due_at_ms       bigint NOT NULL CHECK (due_at_ms >= 0),
    available_at_ms bigint NOT NULL CHECK (available_at_ms >= 0),
    sequence        bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    payload         bytea NOT NULL CHECK (octet_length(payload) > 0),
    attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    lease_owner     text,
    lease_until_ms  bigint CHECK (lease_until_ms >= 0),
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((lease_owner IS NULL) = (lease_until_ms IS NULL))
);

CREATE INDEX deferred_script_impacts_zone_due_idx
    ON shard.deferred_script_impacts (zone_id, due_at_ms, sequence);

CREATE INDEX deferred_script_impacts_scope_due_idx
    ON shard.deferred_script_impacts (zone_id, scope_id, due_at_ms, sequence);

GRANT SELECT, INSERT, UPDATE, DELETE ON shard.deferred_script_impacts TO sarnaut_shard;
GRANT USAGE, SELECT ON SEQUENCE shard.deferred_script_impacts_sequence_seq TO sarnaut_shard;

-- +goose Down

DROP TABLE shard.deferred_script_impacts;
