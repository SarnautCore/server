-- The private gameplay pack defines two distinct alternative currencies used
-- by paid chat. They are rows rather than fields on character_state because a
-- character owns a balance per resource identity, not one interchangeable
-- chat-point purse.

-- +goose Up

CREATE TABLE shard.character_alternative_currency (
    character_id uuid NOT NULL,
    resource_id  bigint NOT NULL,
    balance      bigint NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (character_id, resource_id),
    CONSTRAINT character_alternative_currency_resource_check CHECK (
        resource_id IN (455213063, 455213071)
    ),
    CONSTRAINT character_alternative_currency_balance_check CHECK (balance >= 0)
);

GRANT SELECT, INSERT, UPDATE, DELETE
    ON shard.character_alternative_currency TO sarnaut_shard;

-- +goose Down

DROP TABLE IF EXISTS shard.character_alternative_currency;
