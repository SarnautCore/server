package quests

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/party"
)

// Retail quest UI capacities. Projection refuses content that does not fit.
// It never hides an authored row by truncating it.
const (
	HUDVisibleQuestLimit         = 20
	HUDBookmarkLimit             = 3
	HUDDetailObjectiveLimit      = 5
	HUDNPCInfoObjectiveLimit     = 6
	HUDMandatoryRewardLimit      = 5
	HUDAlternativeRewardLimit    = 5
	HUDReputationRewardLimit     = 5
	HUDCurrencyRewardLimit       = 5
	HUDSecretLimit               = 15
	HUDQuestShareRangeM          = 20.0
	HUDQuestShareOnRequestExpiry = 60 * time.Second
	HUDQuestShareOnStartExpiry   = 10 * time.Second
)

var (
	// ErrHUDCapacityExceeded reports content or state that cannot be represented
	// by the retail quest UI.
	ErrHUDCapacityExceeded = errors.New("quests: retail HUD capacity exceeded")
	// ErrHUDQuestNotVisible reports a selected or bookmarked quest that is not in
	// the character's visible quest book.
	ErrHUDQuestNotVisible = errors.New("quests: quest is not visible in the HUD book")
	// ErrHUDBookmarkDuplicate reports the same quest in two bookmark slots.
	ErrHUDBookmarkDuplicate = errors.New("quests: HUD bookmark is duplicated")
)

// HUDCapacityError identifies the retail collection that overflowed.
type HUDCapacityError struct {
	Collection string
	Count      int
	Limit      int
}

func (problem *HUDCapacityError) Error() string {
	return fmt.Sprintf(
		"%s: %s has %d entries, limit %d",
		ErrHUDCapacityExceeded, problem.Collection, problem.Count, problem.Limit,
	)
}

func (problem *HUDCapacityError) Unwrap() error { return ErrHUDCapacityExceeded }

func checkHUDCapacity(collection string, count, limit int) error {
	if count <= limit {
		return nil
	}
	return &HUDCapacityError{Collection: collection, Count: count, Limit: limit}
}

// HUDQuestState is the state vocabulary authored by the retail quest HUD.
// FAILED exists in the read model, but the current quest machine has no failed
// state and therefore never projects it.
type HUDQuestState string

const (
	HUDQuestStateInProgress    HUDQuestState = "IN_PROGRESS"
	HUDQuestStateReadyToReturn HUDQuestState = "READY_TO_RETURN"
	HUDQuestStateCompleted     HUDQuestState = "COMPLETED"
	HUDQuestStateFailed        HUDQuestState = "FAILED"
)

// HUDState maps a real internal instance state onto the retail vocabulary.
// The boolean is false for offered, unavailable, abandoned, or unknown state.
func HUDState(state State) (HUDQuestState, bool) {
	switch state {
	case StateAccepted, StateInProgress:
		return HUDQuestStateInProgress, true
	case StateCompletable:
		return HUDQuestStateReadyToReturn, true
	case StateTurnedIn:
		return HUDQuestStateCompleted, true
	default:
		return "", false
	}
}

// HUDLocalizedText preserves the localization key carried by the content
// pack. It does not pretend the key is resolved display text.
type HUDLocalizedText struct {
	LocalizationKey string `json:"localization_key"`
}

// HUDPosition is an authored point on a zone map. The current quest pack has
// no such field, so location pointers in HUDQuestInfo remain nil.
type HUDPosition struct {
	X float32 `json:"x"`
	Y float32 `json:"y"`
	Z float32 `json:"z"`
}

// HUDQuestLocation is one authored map and position pair.
type HUDQuestLocation struct {
	ZonesMapID string      `json:"zones_map_id"`
	Position   HUDPosition `json:"position"`
}

// HUDQuestInfo is the complete retail quest information record. Pointers and
// nil slices mean the source pack has no authored value. The projector does
// not derive names, locations, timers, repetition, or secret metadata from an
// id or a default.
type HUDQuestInfo struct {
	ID                  string             `json:"id"`
	Name                HUDLocalizedText   `json:"name"`
	DebugName           *string            `json:"debug_name,omitempty"`
	Level               uint32             `json:"level"`
	HideLevel           *bool              `json:"hide_level,omitempty"`
	RequiredLevel       uint32             `json:"required_level"`
	Goal                HUDLocalizedText   `json:"goal"`
	StartText           HUDLocalizedText   `json:"start_text"`
	CheckText           HUDLocalizedText   `json:"check_text"`
	FinishText          HUDLocalizedText   `json:"finish_text"`
	KickText            *HUDLocalizedText  `json:"kick_text,omitempty"`
	PlotLine            *string            `json:"plot_line,omitempty"`
	Shared              *bool              `json:"shared,omitempty"`
	CanCancel           bool               `json:"can_cancel"`
	Type                string             `json:"type"`
	IsPvP               *bool              `json:"is_pvp,omitempty"`
	IsInSecretSequence  *bool              `json:"is_in_secret_sequence,omitempty"`
	IsTutorial          *bool              `json:"is_tutorial,omitempty"`
	IsRepeatable        *bool              `json:"is_repeatable,omitempty"`
	CanRepeat           *bool              `json:"can_repeat,omitempty"`
	RepeatPeriod        *int32             `json:"repeat_period,omitempty"`
	IsSecret            *bool              `json:"is_secret,omitempty"`
	ZoneName            *HUDLocalizedText  `json:"zone_name,omitempty"`
	ZonesMapID          *string            `json:"zones_map_id,omitempty"`
	GoalLocation        *HUDPosition       `json:"goal_location,omitempty"`
	ReturnLocation      *HUDPosition       `json:"return_location,omitempty"`
	AdditionalLocations []HUDQuestLocation `json:"additional_locations,omitempty"`

	// These ids are present in the source pack even though retail's display
	// record does not render them directly.
	ZoneID     string `json:"zone_id"`
	StarterID  string `json:"starter_id"`
	FinisherID string `json:"finisher_id"`
}

// HUDQuestObjectiveType is the retail objective vocabulary represented by the
// current pack. Later objective kinds already have stable names here, but the
// projector emits them only after the pack and state machine carry real data.
type HUDQuestObjectiveType string

const (
	HUDQuestObjectiveKill             HUDQuestObjectiveType = "KILL"
	HUDQuestObjectiveItem             HUDQuestObjectiveType = "ITEM"
	HUDQuestObjectiveSpecial          HUDQuestObjectiveType = "SPECIAL"
	HUDQuestObjectiveHonor            HUDQuestObjectiveType = "HONOR"
	HUDQuestObjectiveKillAvatar       HUDQuestObjectiveType = "KILL_AVATAR"
	HUDQuestObjectiveMoney            HUDQuestObjectiveType = "MONEY"
	HUDQuestObjectiveShipUpgradeMoney HUDQuestObjectiveType = "SHIP_UPGRADE_MONEY"
	HUDQuestObjectiveUpgradableShip   HUDQuestObjectiveType = "UPGRADABLE_SHIP"
)

// HUDQuestObjectiveItemRef preserves one item content id named by an ITEM
// objective. Display text and icon data are absent because quest rows do not
// carry them.
type HUDQuestObjectiveItemRef struct {
	ItemID string `json:"item_id"`
}

// HUDQuestObjective is one visible authored objective plus optional live
// progress. Progress is nil in an NPC information record for a quest the
// character has not accepted. IsInternal remains schema-compatible and is
// always false in this HUD projection.
type HUDQuestObjective struct {
	ID               *string                    `json:"id,omitempty"`
	Name             HUDLocalizedText           `json:"name"`
	SysDebugName     *string                    `json:"sys_debug_name,omitempty"`
	Progress         *int32                     `json:"progress,omitempty"`
	Required         int32                      `json:"required"`
	IsInternal       bool                       `json:"is_internal"`
	Type             HUDQuestObjectiveType      `json:"type"`
	ShowCounterValue bool                       `json:"show_counter_value"`
	Items            []HUDQuestObjectiveItemRef `json:"items,omitempty"`
}

// HUDRewardItem is one exact reward row from the content pack.
type HUDRewardItem struct {
	ItemID string `json:"item_id"`
	Count  int32  `json:"count"`
	Hidden bool   `json:"hidden"`
}

// HUDReputationReward and HUDCurrencyReward describe retail reward rows that
// the current quest pack cannot author. Their slices remain nil.
type HUDReputationReward struct {
	FactionID string `json:"faction_id"`
	Value     int64  `json:"value"`
}

type HUDCurrencyReward struct {
	CurrencyID string `json:"currency_id"`
	Value      int64  `json:"value"`
}

// HUDQuestRewards is the full retail reward shape.
type HUDQuestRewards struct {
	Money               int64                 `json:"money"`
	Experience          int64                 `json:"experience"`
	Honor               int64                 `json:"honor"`
	MandatoryItems      []HUDRewardItem       `json:"mandatory_items,omitempty"`
	MandatoryItemsCount uint32                `json:"mandatory_items_count"`
	AlternativeItems    []HUDRewardItem       `json:"alternative_items,omitempty"`
	Reputations         []HUDReputationReward `json:"reputations,omitempty"`
	Currencies          []HUDCurrencyReward   `json:"currencies,omitempty"`
}

// HUDQuestCatalogEntry is one complete immutable catalog record.
type HUDQuestCatalogEntry struct {
	Info    HUDQuestInfo    `json:"info"`
	Rewards HUDQuestRewards `json:"rewards"`
}

// Validate refuses a reward collection the retail panels cannot display.
func (rewards HUDQuestRewards) Validate() error {
	checks := []struct {
		name  string
		count int
		limit int
	}{
		{"mandatory quest rewards", len(rewards.MandatoryItems), HUDMandatoryRewardLimit},
		{"alternative quest rewards", len(rewards.AlternativeItems), HUDAlternativeRewardLimit},
		{"reputation quest rewards", len(rewards.Reputations), HUDReputationRewardLimit},
		{"currency quest rewards", len(rewards.Currencies), HUDCurrencyRewardLimit},
	}
	for _, check := range checks {
		if err := checkHUDCapacity(check.name, check.count, check.limit); err != nil {
			return err
		}
	}
	return nil
}

// HUDQuestProgress is one real instance projected for the retail client.
type HUDQuestProgress struct {
	ID              string              `json:"id"`
	State           HUDQuestState       `json:"state"`
	TimerDurationMS *int64              `json:"timer_duration_ms,omitempty"`
	TimerTimeLeftMS *int64              `json:"timer_time_left_ms,omitempty"`
	Objectives      []HUDQuestObjective `json:"objectives"`
}

// HUDQuestSummary is one visible row in the quest book.
type HUDQuestSummary struct {
	ID        string           `json:"id"`
	Name      HUDLocalizedText `json:"name"`
	Level     uint32           `json:"level"`
	HideLevel *bool            `json:"hide_level,omitempty"`
	State     HUDQuestState    `json:"state"`
}

// HUDQuestDetail is the selected quest panel.
type HUDQuestDetail struct {
	Info     HUDQuestInfo     `json:"info"`
	Progress HUDQuestProgress `json:"progress"`
	Rewards  HUDQuestRewards  `json:"rewards"`
}

func (detail HUDQuestDetail) Validate() error {
	if err := checkHUDCapacity(
		"selected quest objectives", len(detail.Progress.Objectives), HUDDetailObjectiveLimit,
	); err != nil {
		return err
	}
	return detail.Rewards.Validate()
}

// HUDNPCQuestInfo is the quest information panel opened at an NPC. State is
// nil when there is no real quest instance for this character.
type HUDNPCQuestInfo struct {
	Info       HUDQuestInfo        `json:"info"`
	State      *HUDQuestState      `json:"state,omitempty"`
	Objectives []HUDQuestObjective `json:"objectives"`
	Rewards    HUDQuestRewards     `json:"rewards"`
}

func (info HUDNPCQuestInfo) Validate() error {
	if err := checkHUDCapacity(
		"NPC quest objectives", len(info.Objectives), HUDNPCInfoObjectiveLimit,
	); err != nil {
		return err
	}
	return info.Rewards.Validate()
}

// HUDQuestSecret is a secret row. Current packs expose no secret flag, so the
// projected collection is nil rather than a guessed list.
type HUDQuestSecret struct {
	QuestID string `json:"quest_id"`
}

// HUDBookState is caller-owned selection state. The quest module checks it
// against the live log but does not persist or mutate it.
type HUDBookState struct {
	SelectedQuestID  string
	BookmarkQuestIDs []string
}

// HUDQuestBook is the complete visible journal projection.
type HUDQuestBook struct {
	Quests     []HUDQuestSummary `json:"quests"`
	Bookmarks  []HUDQuestSummary `json:"bookmarks"`
	Selected   *HUDQuestDetail   `json:"selected,omitempty"`
	Secrets    []HUDQuestSecret  `json:"secrets,omitempty"`
	DailyCount *uint32           `json:"daily_count,omitempty"`
	DailyLimit *uint32           `json:"daily_limit,omitempty"`
}

func (book HUDQuestBook) Validate() error {
	if err := checkHUDCapacity("visible quests", len(book.Quests), HUDVisibleQuestLimit); err != nil {
		return err
	}
	if err := checkHUDCapacity("quest bookmarks", len(book.Bookmarks), HUDBookmarkLimit); err != nil {
		return err
	}
	if err := checkHUDCapacity("quest secrets", len(book.Secrets), HUDSecretLimit); err != nil {
		return err
	}
	if book.Selected != nil {
		return book.Selected.Validate()
	}
	return nil
}

// HUDQuestShareInvite is one server-owned offer. OnStart distinguishes the
// automatic ten-second offer from a manual sixty-second offer. The client's
// incoming modal timeout is presentation state and is not represented here.
type HUDQuestShareInvite struct {
	ShareID              string    `json:"share_id"`
	QuestID              string    `json:"quest_id"`
	SharerName           string    `json:"sharer_name"`
	RecipientCharacterID uuid.UUID `json:"recipient_character_id"`
	OnStart              bool      `json:"on_start"`
	ExpiresAt            time.Time `json:"expires_at"`
}

// HUDQuestShareRefusal is a domain refusal, not a transport error.
type HUDQuestShareRefusal string

const (
	HUDQuestShareRefusalNone        HUDQuestShareRefusal = ""
	HUDQuestShareRefusalNoParty     HUDQuestShareRefusal = "NO_PARTY"
	HUDQuestShareRefusalNotPossible HUDQuestShareRefusal = "NOT_POSSIBLE"
)

// HUDQuestShareRecipientResult preserves which party member received an
// invitation and which eligible nearby member produced NOT_POSSIBLE. A member
// outside the zone or range is filtered before this result is built.
type HUDQuestShareRecipientResult struct {
	RecipientCharacterID uuid.UUID            `json:"recipient_character_id"`
	Invite               *HUDQuestShareInvite `json:"invite,omitempty"`
	Refusal              HUDQuestShareRefusal `json:"refusal,omitempty"`
}

// HUDQuestShareResult is the value a command owner may fan out. Refusal is the
// request-wide NO_PARTY result. Recipient results carry mixed successes and
// NOT_POSSIBLE outcomes without aborting the successful invitations.
type HUDQuestShareResult struct {
	Recipients []HUDQuestShareRecipientResult `json:"recipients"`
	Refusal    HUDQuestShareRefusal           `json:"refusal,omitempty"`
}

// HUDQuestShareRecipientEligibility is the quest-owned view needed after
// membership is known. Party authority does not decide zone distance, life,
// or whether this character can start this quest.
type HUDQuestShareRecipientEligibility struct {
	SameZone      bool
	DistanceM     float32
	Alive         bool
	CanStartQuest bool
}

// HUDQuestShareEligibility resolves one party member against live world and
// quest state. A false second result is treated as NOT_POSSIBLE.
type HUDQuestShareEligibility interface {
	CanShareQuest(sharerCharacterID uuid.UUID, questID string) bool
	QuestShareEligibility(
		sharerCharacterID uuid.UUID,
		recipientCharacterID uuid.UUID,
		questID string,
	) (HUDQuestShareRecipientEligibility, bool)
}

// HUDQuestShareState binds the sharer identity and party authority so the
// ShareQuest command itself takes only the quest id.
type HUDQuestShareState struct {
	sharerCharacterID uuid.UUID
	sharerName        string
	party             party.AudienceReader
	eligibility       HUDQuestShareEligibility
	now               func() time.Time
	newShareID        func() string
	mu                sync.Mutex
	pending           map[string]HUDQuestShareInvite
}

func NewHUDQuestShareState(
	sharerCharacterID uuid.UUID,
	sharerName string,
	partyAudience party.AudienceReader,
	eligibility HUDQuestShareEligibility,
) *HUDQuestShareState {
	return &HUDQuestShareState{
		sharerCharacterID: sharerCharacterID,
		sharerName:        sharerName,
		party:             partyAudience,
		eligibility:       eligibility,
		now:               time.Now,
		newShareID:        func() string { return uuid.NewString() },
		pending:           make(map[string]HUDQuestShareInvite),
	}
}

// ShareQuest creates expiring invitations after party, zone, range, life, and
// quest eligibility checks. Context is server-owned cancellation state. The
// authenticated wire command supplies only questID and cannot pick recipients.
func (state *HUDQuestShareState) ShareQuest(ctx context.Context, questID string) HUDQuestShareResult {
	return state.shareQuest(ctx, questID, HUDQuestShareOnRequestExpiry, false)
}

// ShareQuestOnStart creates the automatic invitation with retail's shorter
// lifetime. It shares pending state with manual offers, so an existing offer
// is not duplicated or shortened.
func (state *HUDQuestShareState) ShareQuestOnStart(
	ctx context.Context,
	questID string,
) HUDQuestShareResult {
	return state.shareQuest(ctx, questID, HUDQuestShareOnStartExpiry, true)
}

func (state *HUDQuestShareState) shareQuest(
	ctx context.Context,
	questID string,
	expiry time.Duration,
	onStart bool,
) HUDQuestShareResult {
	if state == nil || state.party == nil {
		return HUDQuestShareResult{Refusal: HUDQuestShareRefusalNoParty}
	}
	recipientSnapshot, audienceRefusal := state.party.Audience(ctx, state.sharerCharacterID)
	switch audienceRefusal {
	case party.AudienceAllowed:
	case party.AudienceNoParty:
		return HUDQuestShareResult{Refusal: HUDQuestShareRefusalNoParty}
	default:
		return HUDQuestShareResult{Refusal: HUDQuestShareRefusalNotPossible}
	}
	if state.eligibility == nil || !state.eligibility.CanShareQuest(state.sharerCharacterID, questID) {
		return HUDQuestShareResult{Refusal: HUDQuestShareRefusalNotPossible}
	}
	recipients := append([]uuid.UUID(nil), recipientSnapshot...)
	now := state.now()
	expiresAt := now.Add(expiry)
	state.mu.Lock()
	state.expirePendingLocked(now)
	state.mu.Unlock()
	result := HUDQuestShareResult{
		Recipients: make([]HUDQuestShareRecipientResult, 0, len(recipients)),
	}
	seen := make(map[uuid.UUID]struct{}, len(recipients))
	for _, recipientID := range recipients {
		if recipientID == state.sharerCharacterID {
			continue
		}
		if _, duplicate := seen[recipientID]; duplicate {
			continue
		}
		seen[recipientID] = struct{}{}
		eligibility, resolved := HUDQuestShareRecipientEligibility{}, false
		if state.eligibility != nil {
			eligibility, resolved = state.eligibility.QuestShareEligibility(
				state.sharerCharacterID, recipientID, questID,
			)
		}
		if resolved && (!eligibility.SameZone || eligibility.DistanceM > HUDQuestShareRangeM) {
			continue
		}
		if !resolved || eligibility.DistanceM < 0 || !eligibility.Alive || !eligibility.CanStartQuest {
			result.Recipients = append(result.Recipients, HUDQuestShareRecipientResult{
				RecipientCharacterID: recipientID,
				Refusal:              HUDQuestShareRefusalNotPossible,
			})
			continue
		}
		key := sharePendingKey(questID, recipientID)
		state.mu.Lock()
		invite, pending := state.pending[key]
		if !pending {
			invite = HUDQuestShareInvite{
				ShareID:              state.newShareID(),
				QuestID:              questID,
				SharerName:           state.sharerName,
				RecipientCharacterID: recipientID,
				OnStart:              onStart,
				ExpiresAt:            expiresAt,
			}
			state.pending[key] = invite
		}
		state.mu.Unlock()
		result.Recipients = append(result.Recipients, HUDQuestShareRecipientResult{
			RecipientCharacterID: recipientID,
			Invite:               &invite,
		})
	}
	return result
}

// CanShareQuest reports whether the sharer holds a real visible instance.
// It performs no party lookup and no mutation.
func (module *Module) CanShareQuest(characterID uuid.UUID, questID string) bool {
	shareable := false
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		log, ok := module.logs[characterID]
		if !ok {
			return nil
		}
		held, ok := log.instances[questID]
		if !ok || !hudVisible(held.state) {
			return nil
		}
		_, shareable = module.catalog.Definition(questID)
		return nil
	})
	return shareable
}

// QuestShareEligibility resolves live same-zone, distance, life, and start
// eligibility for one party member. It lets Module satisfy
// HUDQuestShareEligibility without exposing world or quest state.
func (module *Module) QuestShareEligibility(
	sharerCharacterID uuid.UUID,
	recipientCharacterID uuid.UUID,
	questID string,
) (HUDQuestShareRecipientEligibility, bool) {
	var (
		eligibility HUDQuestShareRecipientEligibility
		resolved    bool
	)
	_ = module.zone.GameCommand(func(tick gametypes.Tick) error {
		definition, ok := module.catalog.Definition(questID)
		if !ok {
			return nil
		}
		sharerEntityID, sharerPresent := module.entityForCharacter(sharerCharacterID)
		recipientEntityID, recipientPresent := module.entityForCharacter(recipientCharacterID)
		if !sharerPresent {
			return nil
		}
		if !recipientPresent {
			eligibility.SameZone = false
			resolved = true
			return nil
		}
		sharer := tick.Entity(sharerEntityID)
		recipient := tick.Entity(recipientEntityID)
		if sharer == nil || recipient == nil {
			return nil
		}
		recipientLog, ok := module.logs[recipientCharacterID]
		if !ok {
			return nil
		}
		eligibility = HUDQuestShareRecipientEligibility{
			SameZone:      true,
			DistanceM:     gametypes.Distance(tick.Position(sharer), tick.Position(recipient)),
			Alive:         recipient.Alive,
			CanStartQuest: module.gate(recipientLog, definition) == StateOffered,
		}
		resolved = true
		return nil
	})
	return eligibility, resolved
}

func (module *Module) entityForCharacter(characterID uuid.UUID) (uint64, bool) {
	for entityID, candidateID := range module.actors {
		if candidateID == characterID {
			return entityID, true
		}
	}
	return 0, false
}

// PendingInvites returns unexpired invitations in stable recipient/share-id
// order. The returned slice is a copy.
func (state *HUDQuestShareState) PendingInvites() []HUDQuestShareInvite {
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.expirePendingLocked(state.now())
	keys := make([]string, 0, len(state.pending))
	for key := range state.pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	invites := make([]HUDQuestShareInvite, 0, len(keys))
	for _, key := range keys {
		invites = append(invites, state.pending[key])
	}
	return invites
}

func (state *HUDQuestShareState) expirePendingLocked(now time.Time) {
	for key, invite := range state.pending {
		if !now.Before(invite.ExpiresAt) {
			delete(state.pending, key)
		}
	}
}

func sharePendingKey(questID string, recipientID uuid.UUID) string {
	return recipientID.String() + "\x00" + questID
}

// HUDInfo returns the authored information and rewards for one definition.
func (catalog Catalog) HUDInfo(questID string) (HUDQuestCatalogEntry, bool, error) {
	definition, ok := catalog.Definition(questID)
	if !ok {
		return HUDQuestCatalogEntry{}, false, nil
	}
	entry := HUDQuestCatalogEntry{
		Info:    hudInfo(definition),
		Rewards: hudRewards(definition.Rewards),
	}
	if err := entry.Rewards.Validate(); err != nil {
		return HUDQuestCatalogEntry{}, true, fmt.Errorf("quest %q: %w", questID, err)
	}
	return entry, true, nil
}

// HUDCatalog lists every authored quest in canonical-id order.
func (catalog Catalog) HUDCatalog() ([]HUDQuestCatalogEntry, error) {
	entries := make([]HUDQuestCatalogEntry, 0, catalog.Count())
	for _, questID := range catalog.IDs() {
		entry, _, err := catalog.HUDInfo(questID)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// HUDBook projects one admitted character's visible journal. It is a read-only
// query under the zone lock.
func (module *Module) HUDBook(characterID uuid.UUID, state HUDBookState) (HUDQuestBook, bool, error) {
	var (
		book  HUDQuestBook
		found bool
		err   error
	)
	commandErr := module.zone.GameCommand(func(gametypes.Tick) error {
		log, ok := module.logs[characterID]
		if !ok {
			return nil
		}
		found = true
		book, err = projectHUDBook(module.catalog, log, state)
		return nil
	})
	if commandErr != nil {
		return HUDQuestBook{}, false, commandErr
	}
	return book, found, err
}

// HUDSelect returns the selected quest panel for a real visible instance. It
// does not change server state or complete a quest.
func (module *Module) HUDSelect(characterID uuid.UUID, questID string) (HUDQuestDetail, bool, error) {
	var (
		detail HUDQuestDetail
		found  bool
		err    error
	)
	commandErr := module.zone.GameCommand(func(gametypes.Tick) error {
		log, ok := module.logs[characterID]
		if !ok {
			return nil
		}
		held, ok := log.instances[questID]
		if !ok || !hudVisible(held.state) {
			return nil
		}
		definition, ok := module.catalog.Definition(questID)
		if !ok {
			return nil
		}
		found = true
		detail, err = hudDetail(definition, held)
		return nil
	})
	if commandErr != nil {
		return HUDQuestDetail{}, false, commandErr
	}
	return detail, found, err
}

// HUDNPCInfo returns the NPC information panel for an authored quest. If the
// character holds a real instance, the result also carries its state and
// counters. An unaccepted quest keeps those fields absent.
func (module *Module) HUDNPCInfo(
	characterID uuid.UUID,
	questID string,
) (HUDNPCQuestInfo, bool, error) {
	definition, defined := module.catalog.Definition(questID)
	if !defined {
		return HUDNPCQuestInfo{}, false, nil
	}
	var (
		info  HUDNPCQuestInfo
		found bool
		err   error
	)
	commandErr := module.zone.GameCommand(func(gametypes.Tick) error {
		log, ok := module.logs[characterID]
		if !ok {
			return nil
		}
		found = true
		info, err = hudNPCInfo(definition, log.instances[questID])
		return nil
	})
	if commandErr != nil {
		return HUDNPCQuestInfo{}, false, commandErr
	}
	return info, found, err
}

// HUDProgress returns the projected state of any real held instance, including
// COMPLETED after turn-in. Abandoned or unavailable rows have no retail state.
func (module *Module) HUDProgress(
	characterID uuid.UUID,
	questID string,
) (HUDQuestProgress, bool, error) {
	var (
		progress HUDQuestProgress
		found    bool
		err      error
	)
	commandErr := module.zone.GameCommand(func(gametypes.Tick) error {
		log, ok := module.logs[characterID]
		if !ok {
			return nil
		}
		held, ok := log.instances[questID]
		if !ok {
			return nil
		}
		definition, ok := module.catalog.Definition(questID)
		if !ok {
			return nil
		}
		found = true
		progress, err = hudProgress(definition, held)
		return nil
	})
	if commandErr != nil {
		return HUDQuestProgress{}, false, commandErr
	}
	return progress, found, err
}

func projectHUDBook(catalog Catalog, log *questLog, state HUDBookState) (HUDQuestBook, error) {
	book := HUDQuestBook{
		Quests:    make([]HUDQuestSummary, 0),
		Bookmarks: make([]HUDQuestSummary, 0, len(state.BookmarkQuestIDs)),
	}
	visible := make(map[string]HUDQuestSummary)
	instances := make(map[string]*instance)
	for _, questID := range sortedIDs(log.instances) {
		held := log.instances[questID]
		if !hudVisible(held.state) {
			continue
		}
		definition, ok := catalog.Definition(questID)
		if !ok {
			continue
		}
		retailState, ok := HUDState(held.state)
		if !ok {
			continue
		}
		summary := HUDQuestSummary{
			ID:    questID,
			Name:  HUDLocalizedText{LocalizationKey: definition.NameKey},
			Level: definition.Level,
			State: retailState,
		}
		book.Quests = append(book.Quests, summary)
		visible[questID] = summary
		instances[questID] = held
	}
	if err := checkHUDCapacity("visible quests", len(book.Quests), HUDVisibleQuestLimit); err != nil {
		return HUDQuestBook{}, err
	}
	if err := checkHUDCapacity("quest bookmarks", len(state.BookmarkQuestIDs), HUDBookmarkLimit); err != nil {
		return HUDQuestBook{}, err
	}
	bookmarked := make(map[string]struct{}, len(state.BookmarkQuestIDs))
	for _, questID := range state.BookmarkQuestIDs {
		if _, duplicate := bookmarked[questID]; duplicate {
			return HUDQuestBook{}, fmt.Errorf("%w: %q", ErrHUDBookmarkDuplicate, questID)
		}
		summary, ok := visible[questID]
		if !ok {
			return HUDQuestBook{}, fmt.Errorf("%w: bookmark %q", ErrHUDQuestNotVisible, questID)
		}
		bookmarked[questID] = struct{}{}
		book.Bookmarks = append(book.Bookmarks, summary)
	}
	if state.SelectedQuestID != "" {
		held, ok := instances[state.SelectedQuestID]
		if !ok {
			return HUDQuestBook{}, fmt.Errorf(
				"%w: selection %q", ErrHUDQuestNotVisible, state.SelectedQuestID,
			)
		}
		definition, _ := catalog.Definition(state.SelectedQuestID)
		detail, err := hudDetail(definition, held)
		if err != nil {
			return HUDQuestBook{}, err
		}
		book.Selected = &detail
	}
	if err := book.Validate(); err != nil {
		return HUDQuestBook{}, err
	}
	return book, nil
}

func hudVisible(state State) bool {
	return state == StateAccepted || state == StateInProgress || state == StateCompletable
}

func visibleQuestCount(log *questLog) int {
	count := 0
	for _, held := range log.instances {
		if hudVisible(held.state) {
			count++
		}
	}
	return count
}

func hudDetail(definition pack.Quest, held *instance) (HUDQuestDetail, error) {
	progress, err := hudProgress(definition, held)
	if err != nil {
		return HUDQuestDetail{}, err
	}
	detail := HUDQuestDetail{
		Info:     hudInfo(definition),
		Progress: progress,
		Rewards:  hudRewards(definition.Rewards),
	}
	if err := detail.Validate(); err != nil {
		return HUDQuestDetail{}, fmt.Errorf("quest %q: %w", definition.ID, err)
	}
	return detail, nil
}

func hudNPCInfo(definition pack.Quest, held *instance) (HUDNPCQuestInfo, error) {
	info := HUDNPCQuestInfo{
		Info:       hudInfo(definition),
		Objectives: hudObjectives(definition.Objectives, nil),
		Rewards:    hudRewards(definition.Rewards),
	}
	if held != nil {
		if state, ok := HUDState(held.state); ok {
			info.State = &state
			info.Objectives = hudObjectives(definition.Objectives, held.counters)
		}
	}
	if err := info.Validate(); err != nil {
		return HUDNPCQuestInfo{}, fmt.Errorf("quest %q: %w", definition.ID, err)
	}
	return info, nil
}

func hudProgress(definition pack.Quest, held *instance) (HUDQuestProgress, error) {
	state, ok := HUDState(held.state)
	if !ok {
		return HUDQuestProgress{}, fmt.Errorf(
			"quest %q state %s has no retail HUD state", definition.ID, held.state,
		)
	}
	return HUDQuestProgress{
		ID:         definition.ID,
		State:      state,
		Objectives: hudObjectives(definition.Objectives, held.counters),
	}, nil
}

func hudInfo(definition pack.Quest) HUDQuestInfo {
	repeatPeriod := max(int32(0), definition.RepeatPeriod)
	return HUDQuestInfo{
		ID:            definition.ID,
		Name:          HUDLocalizedText{LocalizationKey: definition.NameKey},
		Level:         definition.Level,
		RequiredLevel: definition.RequiredLevel,
		Goal:          HUDLocalizedText{LocalizationKey: definition.GoalKey},
		StartText:     HUDLocalizedText{LocalizationKey: definition.StartKey},
		CheckText:     HUDLocalizedText{LocalizationKey: definition.CheckKey},
		FinishText:    HUDLocalizedText{LocalizationKey: definition.FinishKey},
		CanCancel:     definition.CanCancel,
		Type:          definition.QuestType,
		RepeatPeriod:  &repeatPeriod,
		ZoneID:        definition.ZoneID,
		StarterID:     definition.StarterID,
		FinisherID:    definition.FinisherID,
	}
}

func hudObjectives(definitions []pack.QuestObjective, counters []int32) []HUDQuestObjective {
	objectives := make([]HUDQuestObjective, 0, len(definitions))
	for index, definition := range definitions {
		if definition.Internal {
			continue
		}
		objective := HUDQuestObjective{
			Name:             HUDLocalizedText{LocalizationKey: definition.CounterKey},
			Required:         definition.Limit,
			IsInternal:       definition.Internal,
			Type:             hudObjectiveType(definition.Kind),
			ShowCounterValue: definition.ShowCount,
		}
		if index < len(counters) {
			progress := counters[index]
			objective.Progress = &progress
		}
		if definition.Kind == pack.QuestObjectiveCountItem {
			objective.Items = make([]HUDQuestObjectiveItemRef, 0, len(definition.TargetIDs))
			for _, itemID := range definition.TargetIDs {
				objective.Items = append(objective.Items, HUDQuestObjectiveItemRef{ItemID: itemID})
			}
		}
		objectives = append(objectives, objective)
	}
	return objectives
}

func hudObjectiveType(kind pack.QuestObjectiveKind) HUDQuestObjectiveType {
	switch kind {
	case pack.QuestObjectiveCountKill:
		return HUDQuestObjectiveKill
	case pack.QuestObjectiveCountItem:
		return HUDQuestObjectiveItem
	case pack.QuestObjectiveCountSpecial:
		return HUDQuestObjectiveSpecial
	default:
		return ""
	}
}

func hudRewards(rewards pack.QuestRewards) HUDQuestRewards {
	projected := HUDQuestRewards{
		Money:               rewards.Money,
		Experience:          rewards.Experience,
		Honor:               rewards.Honor,
		MandatoryItems:      hudRewardItems(rewards.MandatoryItems),
		MandatoryItemsCount: uint32(len(rewards.MandatoryItems)),
		AlternativeItems:    hudRewardItems(rewards.AlternativeItems),
	}
	return projected
}

func hudRewardItems(items []pack.QuestRewardItem) []HUDRewardItem {
	if len(items) == 0 {
		return nil
	}
	projected := make([]HUDRewardItem, 0, len(items))
	for _, item := range items {
		projected = append(projected, HUDRewardItem{
			ItemID: item.ItemID,
			Count:  item.Count,
			Hidden: item.Hidden,
		})
	}
	return projected
}
