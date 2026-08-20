-- mechanics/quests.md rule 5.7.4: a turn-in credits experience, money and
-- honor together, in the transaction that marks the quest turned in. Two of
-- the three already had a column; honor did not, and a grant that silently
-- dropped one of its three currencies would be a partial turn-in, which rule
-- 5.7.6 says must not exist.
--
-- It is a column on shard.character_state and not a table of its own for the
-- reason currency is: honor is per-character scalar progression exactly like
-- level, experience and the purse, and splitting it out would mean two writes
-- where ADR 0031 §5 promises one transaction.
--
-- Every M2 quest awards zero honor. The column is here anyway, because the
-- alternative is a grant path that works for the fixture and loses a reward
-- the first time content authors one.

-- +goose Up

ALTER TABLE shard.character_state
    ADD COLUMN honor bigint NOT NULL DEFAULT 0;

-- Negative honor is not a state the game has a rule for, and unlike experience
-- there is no spend path that could legitimately drive it below zero.
ALTER TABLE shard.character_state
    ADD CONSTRAINT character_state_honor_check CHECK (honor >= 0);

-- +goose Down

ALTER TABLE shard.character_state
    DROP CONSTRAINT IF EXISTS character_state_honor_check;

ALTER TABLE shard.character_state
    DROP COLUMN IF EXISTS honor;
