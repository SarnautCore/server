package store

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// memoryState is the whole database as plain maps. It is a value that can be
// copied, which is what gives the in-memory [Repository] real rollback instead
// of a comment claiming rollback.
type memoryState struct {
	accounts   map[uuid.UUID]Account
	emails     map[string]uuid.UUID
	characters map[uuid.UUID]Character
	names      map[string]uuid.UUID
	states     map[uuid.UUID]CharacterState
	inventory  map[uuid.UUID]map[int32]InventoryItem
	quests     map[uuid.UUID]map[string]QuestState
}

func newMemoryState() *memoryState {
	return &memoryState{
		accounts:   make(map[uuid.UUID]Account),
		emails:     make(map[string]uuid.UUID),
		characters: make(map[uuid.UUID]Character),
		names:      make(map[string]uuid.UUID),
		states:     make(map[uuid.UUID]CharacterState),
		inventory:  make(map[uuid.UUID]map[int32]InventoryItem),
		quests:     make(map[uuid.UUID]map[string]QuestState),
	}
}

func (state *memoryState) clone() *memoryState {
	copied := &memoryState{
		accounts:   copyMap(state.accounts),
		emails:     copyMap(state.emails),
		characters: copyMap(state.characters),
		names:      copyMap(state.names),
		states:     copyMap(state.states),
		inventory:  make(map[uuid.UUID]map[int32]InventoryItem, len(state.inventory)),
		quests:     make(map[uuid.UUID]map[string]QuestState, len(state.quests)),
	}
	for characterID, slots := range state.inventory {
		copied.inventory[characterID] = copyMap(slots)
	}
	for characterID, quests := range state.quests {
		byQuest := make(map[string]QuestState, len(quests))
		for questID, quest := range quests {
			quest.Objectives = slices.Clone(quest.Objectives)
			byQuest[questID] = quest
		}
		copied.quests[characterID] = byQuest
	}
	return copied
}

func copyMap[K comparable, V any](source map[K]V) map[K]V {
	copied := make(map[K]V, len(source))
	for key, value := range source {
		copied[key] = value
	}
	return copied
}

// memoryStore is the DSN-free [Repository]. It exists so `go test ./...` proves
// gameplay logic without a container, and so a developer can run the shard
// against nothing. It is not a cache and is never used with a DSN configured.
type memoryStore struct {
	mu    sync.Mutex
	state *memoryState
	// root is nil for the store a caller constructed and non-nil for the view
	// handed to a RunInTx callback. A view does not lock: its root holds the
	// mutex for the whole transaction.
	root *memoryStore
	now  func() time.Time
}

// NewMemory returns an in-memory [Repository] with the same observable
// behaviour as the Postgres one, including name-uniqueness rejection, save
// sequence rejection and transaction rollback.
func NewMemory() Repository {
	return &memoryStore{state: newMemoryState(), now: time.Now}
}

func (store *memoryStore) begin(ctx context.Context) (*memoryState, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if store.root != nil {
		return store.state, func() {}, nil
	}
	store.mu.Lock()
	return store.state, store.mu.Unlock, nil
}

func (store *memoryStore) timestamp() time.Time {
	if store.now == nil {
		return time.Now().UTC()
	}
	return store.now().UTC()
}

func (store *memoryStore) RunInTx(ctx context.Context, fn func(ctx context.Context, tx Repository) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if store.root != nil {
		return fn(ctx, store)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	working := store.state.clone()
	view := &memoryStore{state: working, root: store, now: store.now}
	if err := runAndRecover(ctx, view, fn); err != nil {
		// The clone is discarded, so nothing fn wrote is visible. That is the
		// rollback.
		return err
	}
	store.state = working
	return nil
}

func (store *memoryStore) CreateAccount(ctx context.Context, account Account) error {
	state, done, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer done()

	email := NormalizeEmail(account.Email)
	if _, exists := state.emails[email]; exists {
		return ErrEmailTaken
	}
	if account.CreatedAt.IsZero() {
		account.CreatedAt = store.timestamp()
	}
	state.accounts[account.AccountID] = account
	state.emails[email] = account.AccountID
	return nil
}

func (store *memoryStore) AccountByID(ctx context.Context, accountID uuid.UUID) (Account, error) {
	state, done, err := store.begin(ctx)
	if err != nil {
		return Account{}, err
	}
	defer done()

	account, ok := state.accounts[accountID]
	if !ok {
		return Account{}, ErrNotFound
	}
	return account, nil
}

func (store *memoryStore) AccountByEmail(ctx context.Context, email string) (Account, error) {
	state, done, err := store.begin(ctx)
	if err != nil {
		return Account{}, err
	}
	defer done()

	accountID, ok := state.emails[NormalizeEmail(email)]
	if !ok {
		return Account{}, ErrNotFound
	}
	return state.accounts[accountID], nil
}

func (store *memoryStore) CreateCharacter(ctx context.Context, character Character) error {
	state, done, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer done()

	if character.NameNormalized == "" {
		character.NameNormalized = NormalizeCharacterName(character.Name)
	}
	if _, exists := state.names[character.NameNormalized]; exists {
		return ErrNameTaken
	}
	if character.CreatedAt.IsZero() {
		character.CreatedAt = store.timestamp()
	}
	state.characters[character.CharacterID] = character
	state.names[character.NameNormalized] = character.CharacterID
	return nil
}

func (store *memoryStore) CharacterByID(ctx context.Context, characterID uuid.UUID) (Character, error) {
	state, done, err := store.begin(ctx)
	if err != nil {
		return Character{}, err
	}
	defer done()

	character, ok := state.characters[characterID]
	if !ok {
		return Character{}, ErrNotFound
	}
	return character, nil
}

func (store *memoryStore) CharactersByAccount(ctx context.Context, accountID uuid.UUID) ([]Character, error) {
	state, done, err := store.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	characters := make([]Character, 0)
	for _, character := range state.characters {
		if character.AccountID == accountID && character.DeletedAt == nil {
			characters = append(characters, character)
		}
	}
	sort.Slice(characters, func(left, right int) bool {
		if !characters[left].CreatedAt.Equal(characters[right].CreatedAt) {
			return characters[left].CreatedAt.Before(characters[right].CreatedAt)
		}
		return characters[left].CharacterID.String() < characters[right].CharacterID.String()
	})
	return characters, nil
}

func (store *memoryStore) SaveCharacterState(ctx context.Context, incoming CharacterState) error {
	state, done, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer done()

	if stored, ok := state.states[incoming.CharacterID]; ok && stored.SaveSeq >= incoming.SaveSeq {
		return ErrStaleSave
	}
	incoming.SavedAt = store.timestamp()
	state.states[incoming.CharacterID] = incoming
	return nil
}

func (store *memoryStore) LoadCharacterState(ctx context.Context, characterID uuid.UUID) (CharacterState, error) {
	state, done, err := store.begin(ctx)
	if err != nil {
		return CharacterState{}, err
	}
	defer done()

	stored, ok := state.states[characterID]
	if !ok {
		return CharacterState{}, ErrNotFound
	}
	return stored, nil
}

func (store *memoryStore) PutItem(ctx context.Context, characterID uuid.UUID, item InventoryItem) error {
	state, done, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer done()

	if item.Quantity <= 0 {
		return errQuantity(item.Quantity)
	}
	slots := state.inventorySlots(characterID)
	existing, occupied := slots[item.Slot]
	switch {
	case !occupied:
		slots[item.Slot] = item
	case existing.ItemID == item.ItemID:
		existing.Quantity += item.Quantity
		slots[item.Slot] = existing
	default:
		return ErrSlotOccupied
	}
	return nil
}

func (store *memoryStore) MoveItem(ctx context.Context, characterID uuid.UUID, fromSlot, toSlot int32) error {
	if fromSlot == toSlot {
		return nil
	}

	state, done, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer done()

	slots := state.inventorySlots(characterID)
	source, ok := slots[fromSlot]
	if !ok {
		return ErrNotFound
	}

	destination, occupied := slots[toSlot]
	switch {
	case !occupied:
		delete(slots, fromSlot)
		source.Slot = toSlot
		slots[toSlot] = source
	case destination.ItemID == source.ItemID:
		delete(slots, fromSlot)
		destination.Quantity += source.Quantity
		slots[toSlot] = destination
	default:
		source.Slot, destination.Slot = toSlot, fromSlot
		slots[toSlot] = source
		slots[fromSlot] = destination
	}
	return nil
}

func (store *memoryStore) ReplaceInventory(ctx context.Context, characterID uuid.UUID, items []InventoryItem) error {
	state, done, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer done()

	slots := make(map[int32]InventoryItem, len(items))
	for _, item := range items {
		if item.Quantity <= 0 {
			return errQuantity(item.Quantity)
		}
		slots[item.Slot] = item
	}
	state.inventory[characterID] = slots
	return nil
}

func (store *memoryStore) LoadInventory(ctx context.Context, characterID uuid.UUID) ([]InventoryItem, error) {
	state, done, err := store.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	items := make([]InventoryItem, 0, len(state.inventory[characterID]))
	for _, item := range state.inventory[characterID] {
		items = append(items, item)
	}
	sort.Slice(items, func(left, right int) bool { return items[left].Slot < items[right].Slot })
	return items, nil
}

func (store *memoryStore) UpsertQuestState(ctx context.Context, characterID uuid.UUID, quest QuestState) error {
	state, done, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer done()

	byQuest, ok := state.quests[characterID]
	if !ok {
		byQuest = make(map[string]QuestState)
		state.quests[characterID] = byQuest
	}
	quest.Objectives = slices.Clone(objectivesOrEmpty(quest.Objectives))
	quest.UpdatedAt = store.timestamp()
	byQuest[quest.QuestID] = quest
	return nil
}

func (store *memoryStore) LoadQuestStates(ctx context.Context, characterID uuid.UUID) ([]QuestState, error) {
	state, done, err := store.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	quests := make([]QuestState, 0, len(state.quests[characterID]))
	for _, quest := range state.quests[characterID] {
		quest.Objectives = slices.Clone(quest.Objectives)
		quests = append(quests, quest)
	}
	sort.Slice(quests, func(left, right int) bool { return quests[left].QuestID < quests[right].QuestID })
	return quests, nil
}

func (state *memoryState) inventorySlots(characterID uuid.UUID) map[int32]InventoryItem {
	slots, ok := state.inventory[characterID]
	if !ok {
		slots = make(map[int32]InventoryItem)
		state.inventory[characterID] = slots
	}
	return slots
}
