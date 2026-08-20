-- mechanics/loot.md rule 5.6.1: money credits the looting character's purse
-- directly. It occupies no bag slot and has no stack limit, so it is a column
-- on the character rather than a row in shard.character_inventory.
--
-- The column is on shard.character_state and not on a table of its own because
-- the purse is per-character scalar progression, exactly like level and
-- experience, and splitting it out would mean two writes where ADR 0031 §5
-- promises one transaction.

-- +goose Up

ALTER TABLE shard.character_state
    ADD COLUMN currency bigint NOT NULL DEFAULT 0;

-- A negative purse is not a state the game has a rule for. The check is here
-- rather than in Go so that a bug in a future spend path fails the transaction
-- instead of leaving a character owing money.
ALTER TABLE shard.character_state
    ADD CONSTRAINT character_state_currency_check CHECK (currency >= 0);

-- +goose Down

ALTER TABLE shard.character_state
    DROP CONSTRAINT IF EXISTS character_state_currency_check;

ALTER TABLE shard.character_state
    DROP COLUMN IF EXISTS currency;
