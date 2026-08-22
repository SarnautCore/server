-- Durable idempotency for irreversible experience awards. The evaluator's
-- execution key is stable across a retry, so committing it in the same
-- transaction as character_state prevents a crash or queue replay from
-- granting the same experience twice.

-- +goose Up

ALTER TABLE shard.character_state
    ADD COLUMN resurrection_sickness_ms bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT character_state_resurrection_sickness_check
        CHECK (resurrection_sickness_ms BETWEEN 0 AND 9223372036854);

CREATE TABLE shard.character_progression_grants (
    character_id uuid NOT NULL,
    execution_key text NOT NULL,
    amount bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (character_id, execution_key),
    CONSTRAINT character_progression_grants_character_fk
        FOREIGN KEY (character_id) REFERENCES shard.character_state(character_id)
        ON DELETE CASCADE,
    CONSTRAINT character_progression_grants_execution_key_check
        CHECK (length(execution_key) > 0),
    CONSTRAINT character_progression_grants_amount_check CHECK (amount > 0)
);

GRANT SELECT, INSERT, UPDATE, DELETE
    ON shard.character_progression_grants TO sarnaut_shard;

-- +goose Down

DROP TABLE IF EXISTS shard.character_progression_grants;

ALTER TABLE shard.character_state
    DROP CONSTRAINT IF EXISTS character_state_resurrection_sickness_check,
    DROP COLUMN IF EXISTS resurrection_sickness_ms;
