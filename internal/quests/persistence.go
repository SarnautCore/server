package quests

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/inventory"
)

var ErrGrantWouldNotFit = errors.New("quests: the quest grant does not fit in the bag")

type ItemCount struct {
	ItemID string
	Count  int32
}

type QuestState struct {
	QuestID    string
	State      string
	Objectives []byte
	UpdatedAt  time.Time
}

type Grant struct {
	CharacterID uuid.UUID
	Consume     []ItemCount
	Grants      []ItemCount
	Experience  int64
	Money       int64
	Honor       int64
	Quest       QuestState
}

type GrantResult struct {
	Inventory  []inventory.InventoryItem
	Currency   int64
	Experience int64
	Honor      int64
	SaveSeq    int64
}
