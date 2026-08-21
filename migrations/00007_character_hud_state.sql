-- Persist the authored HUD state that was previously reconstructed or absent:
-- retail equipment ordinals, the equipped bag, a native bag-layout identity,
-- fourteen ordered innate stats and thirty-six authored action slots.
-- Presentation remains compiled product content. No source sysName is stored.

-- +goose Up

ALTER TABLE shard.character_inventory
    RENAME COLUMN item_id TO product_item_id;

ALTER TABLE shard.character_inventory
    ADD COLUMN instance_id numeric(20, 0),
    ADD COLUMN counter_value integer NOT NULL DEFAULT 0,
    ADD COLUMN is_bound boolean NOT NULL DEFAULT false,
    ADD COLUMN is_cursed boolean NOT NULL DEFAULT false,
    ADD COLUMN is_quest_operator boolean NOT NULL DEFAULT false,
    ADD COLUMN remove_time_raw bigint,
    ADD COLUMN rune_resource_id text,
    ADD COLUMN rune_slot_resource_id text;

-- Existing rows predate instance identity. Their stable per-character identity
-- is their one-based slot order at migration time; later moves carry the id.
WITH ranked AS (
    SELECT character_id, slot,
           row_number() OVER (PARTITION BY character_id ORDER BY slot)::numeric(20, 0) AS instance_id
    FROM shard.character_inventory
)
UPDATE shard.character_inventory AS inventory
SET instance_id = ranked.instance_id
FROM ranked
WHERE inventory.character_id = ranked.character_id AND inventory.slot = ranked.slot;

ALTER TABLE shard.character_inventory
    ALTER COLUMN instance_id SET NOT NULL;

ALTER TABLE shard.character_inventory
    ADD CONSTRAINT character_inventory_instance_id_check
        CHECK (instance_id BETWEEN 1 AND 18446744073709551615),
    ADD CONSTRAINT character_inventory_instance_id_key
        UNIQUE (character_id, instance_id),
    ADD CONSTRAINT character_inventory_product_item_id_check
        CHECK (product_item_id <> ''),
    ADD CONSTRAINT character_inventory_rune_resource_id_check
        CHECK (rune_resource_id IS NULL OR rune_resource_id <> ''),
    ADD CONSTRAINT character_inventory_rune_slot_resource_id_check
        CHECK (rune_slot_resource_id IS NULL OR rune_slot_resource_id <> '');

CREATE TABLE shard.character_equipment (
    character_id         uuid NOT NULL,
    slot                 smallint NOT NULL,
    instance_id          numeric(20, 0) NOT NULL,
    product_item_id      text NOT NULL,
    quantity             integer NOT NULL,
    counter_value        integer NOT NULL DEFAULT 0,
    is_bound             boolean NOT NULL DEFAULT false,
    is_cursed            boolean NOT NULL DEFAULT false,
    is_quest_operator    boolean NOT NULL DEFAULT false,
    remove_time_raw      bigint,
    rune_resource_id     text,
    rune_slot_resource_id text,
    PRIMARY KEY (character_id, slot),
    CONSTRAINT character_equipment_instance_id_key UNIQUE (character_id, instance_id),
    CONSTRAINT character_equipment_slot_check
        CHECK (slot IN (0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 18, 19, 20, 21)),
    CONSTRAINT character_equipment_instance_id_check
        CHECK (instance_id BETWEEN 1 AND 18446744073709551615),
    CONSTRAINT character_equipment_product_item_id_check CHECK (product_item_id <> ''),
    CONSTRAINT character_equipment_quantity_check CHECK (quantity > 0),
    CONSTRAINT character_equipment_rune_resource_id_check
        CHECK (rune_resource_id IS NULL OR rune_resource_id <> ''),
    CONSTRAINT character_equipment_rune_slot_resource_id_check
        CHECK (rune_slot_resource_id IS NULL OR rune_slot_resource_id <> '')
);

-- Five fixed nullable columns make both limits database facts: partitions are
-- contiguous from zero, there can be at most five, and their sum is at most 60.
CREATE TABLE shard.character_bag_layout (
    character_id uuid PRIMARY KEY,
    layout_id    text NOT NULL,
    partition_0 smallint NOT NULL,
    partition_1 smallint,
    partition_2 smallint,
    partition_3 smallint,
    partition_4 smallint,
    CONSTRAINT character_bag_layout_id_check CHECK (layout_id <> ''),
    CONSTRAINT character_bag_layout_partition_0_check CHECK (partition_0 > 0),
    CONSTRAINT character_bag_layout_partition_1_check CHECK (partition_1 IS NULL OR partition_1 > 0),
    CONSTRAINT character_bag_layout_partition_2_check CHECK (partition_2 IS NULL OR partition_2 > 0),
    CONSTRAINT character_bag_layout_partition_3_check CHECK (partition_3 IS NULL OR partition_3 > 0),
    CONSTRAINT character_bag_layout_partition_4_check CHECK (partition_4 IS NULL OR partition_4 > 0),
    CONSTRAINT character_bag_layout_contiguous_check CHECK (
        (partition_1 IS NOT NULL OR partition_2 IS NULL) AND
        (partition_2 IS NOT NULL OR partition_3 IS NULL) AND
        (partition_3 IS NOT NULL OR partition_4 IS NULL)
    ),
    CONSTRAINT character_bag_layout_capacity_check CHECK (
        partition_0 + COALESCE(partition_1, 0) + COALESCE(partition_2, 0) +
        COALESCE(partition_3, 0) + COALESCE(partition_4, 0) <= 60
    )
);

CREATE TABLE shard.character_stats (
    character_id uuid NOT NULL,
    ordinal      smallint NOT NULL,
    base             real,
    result           real,
    result_long_term real,
    PRIMARY KEY (character_id, ordinal),
    CONSTRAINT character_stats_ordinal_check CHECK (ordinal BETWEEN 0 AND 13)
);

CREATE TABLE shard.character_actions (
    character_id uuid NOT NULL,
    ordinal      smallint NOT NULL,
    ability_id   text,
    PRIMARY KEY (character_id, ordinal),
    CONSTRAINT character_actions_ordinal_check CHECK (ordinal BETWEEN 0 AND 35),
    CONSTRAINT character_actions_ability_id_check CHECK (ability_id IS NULL OR ability_id <> '')
);

-- +goose Down

DROP TABLE IF EXISTS shard.character_actions;
DROP TABLE IF EXISTS shard.character_stats;
DROP TABLE IF EXISTS shard.character_bag_layout;
DROP TABLE IF EXISTS shard.character_equipment;

ALTER TABLE shard.character_inventory
    DROP CONSTRAINT IF EXISTS character_inventory_rune_slot_resource_id_check,
    DROP CONSTRAINT IF EXISTS character_inventory_rune_resource_id_check,
    DROP CONSTRAINT IF EXISTS character_inventory_product_item_id_check,
    DROP CONSTRAINT IF EXISTS character_inventory_instance_id_key,
    DROP CONSTRAINT IF EXISTS character_inventory_instance_id_check;

ALTER TABLE shard.character_inventory
    DROP COLUMN IF EXISTS rune_slot_resource_id,
    DROP COLUMN IF EXISTS rune_resource_id,
    DROP COLUMN IF EXISTS remove_time_raw,
    DROP COLUMN IF EXISTS is_quest_operator,
    DROP COLUMN IF EXISTS is_cursed,
    DROP COLUMN IF EXISTS is_bound,
    DROP COLUMN IF EXISTS counter_value,
    DROP COLUMN IF EXISTS instance_id;

ALTER TABLE shard.character_inventory
    RENAME COLUMN product_item_id TO item_id;
