-- Inventory and quest rewards can outlive an uncertain response. Persist the
-- exact committed result under a caller-stable key so a retry cannot duplicate
-- items, money, experience, honor, or quest state.

-- +goose Up

CREATE TABLE shard.inventory_execution_receipts (
    character_id uuid NOT NULL REFERENCES auth.characters(character_id) ON DELETE CASCADE,
    execution_key text NOT NULL,
    operation text NOT NULL,
    result jsonb NOT NULL,
    committed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (character_id, execution_key),
    CONSTRAINT inventory_execution_receipts_key_check CHECK (execution_key <> ''),
    CONSTRAINT inventory_execution_receipts_operation_check CHECK (operation <> '')
);

GRANT SELECT, INSERT ON shard.inventory_execution_receipts TO sarnaut_shard;

-- +goose Down

DROP TABLE IF EXISTS shard.inventory_execution_receipts;
