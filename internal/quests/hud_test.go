package quests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/party"
	"github.com/SarnautCore/server/internal/world"
)

func TestHUDInfoGoldenPreservesOnlyAuthoredFields(t *testing.T) {
	definition := hudTestDefinition("quest.test.tide-ledger")
	catalog := hudTestCatalog(t, definition)

	entry, found, err := catalog.HUDInfo(definition.ID)
	if err != nil {
		t.Fatalf("HUDInfo() error = %v", err)
	}
	if !found {
		t.Fatal("HUDInfo() did not find the authored quest")
	}
	if entry.Info.DebugName != nil || entry.Info.HideLevel != nil || entry.Info.KickText != nil ||
		entry.Info.PlotLine != nil || entry.Info.Shared != nil || entry.Info.IsPvP != nil ||
		entry.Info.IsInSecretSequence != nil || entry.Info.IsTutorial != nil ||
		entry.Info.IsRepeatable != nil || entry.Info.CanRepeat != nil ||
		entry.Info.RepeatPeriodMS != nil || entry.Info.IsSecret != nil || entry.Info.ZoneName != nil ||
		entry.Info.ZonesMapID != nil || entry.Info.GoalLocation != nil || entry.Info.ReturnLocation != nil ||
		entry.Info.AdditionalLocations != nil {
		t.Fatal("HUDInfo() filled a field the quest pack cannot author")
	}
	if entry.Rewards.Reputations != nil || entry.Rewards.Currencies != nil {
		t.Fatal("HUDInfo() invented reputation or currency reward rows")
	}

	assertHUDGolden(t, "hud_info.golden.json", entry)
}

func TestHUDDetailGoldenCarriesVisibleItemObjectives(t *testing.T) {
	definition := hudTestDefinition("quest.test.tide-ledger")
	held := &instance{
		questID: definition.ID,
		state:   StateCompletable,
		counters: []int32{
			3,
			1,
		},
	}
	detail, err := hudDetail(definition, held)
	if err != nil {
		t.Fatalf("hudDetail() error = %v", err)
	}
	if got := detail.Progress.State; got != HUDQuestStateReadyToReturn {
		t.Fatalf("state = %q, want %q", got, HUDQuestStateReadyToReturn)
	}
	if got := len(detail.Progress.Objectives); got != 2 {
		t.Fatalf("objectives = %d, want 2", got)
	}
	if detail.Progress.Objectives[1].IsInternal {
		t.Error("a visible objective was marked internal")
	}
	if got := detail.Progress.Objectives[1].Items; len(got) != 2 ||
		got[0].ItemID != "item.test.tonic" || got[1].ItemID != "item.test.elixir" {
		t.Errorf("item objective targets = %+v, want the two authored item ids", got)
	}
	if detail.Progress.TimerDurationMS != nil || detail.Progress.TimerTimeLeftMS != nil {
		t.Error("the projection invented a quest timer")
	}
	assertHUDGolden(t, "hud_detail.golden.json", detail)
}

func TestHUDProjectionFiltersInternalObjectivesWithoutChangingAuthorityState(t *testing.T) {
	definition := hudTestDefinition("quest.test.internal")
	definition.Objectives = append(definition.Objectives, pack.QuestObjective{
		Kind:       pack.QuestObjectiveCountKill,
		Limit:      9,
		Internal:   true,
		CounterKey: "quest.test.internal.hidden",
	})
	held := &instance{
		questID: definition.ID,
		state:   StateInProgress,
		counters: []int32{
			1,
			1,
			7,
		},
	}
	detail, err := hudDetail(definition, held)
	if err != nil {
		t.Fatalf("hudDetail() error = %v", err)
	}
	if got := len(detail.Progress.Objectives); got != 2 {
		t.Fatalf("visible objectives = %d, want 2", got)
	}
	for index, objective := range detail.Progress.Objectives {
		if objective.IsInternal {
			t.Errorf("visible objective %d is marked internal", index)
		}
	}
	if got := held.counters[2]; got != 7 {
		t.Errorf("internal authority counter = %d after projection, want 7", got)
	}
}

func TestHUDStateMapsOnlyRealInstanceStates(t *testing.T) {
	tests := []struct {
		internal State
		want     HUDQuestState
		ok       bool
	}{
		{StateAccepted, HUDQuestStateInProgress, true},
		{StateInProgress, HUDQuestStateInProgress, true},
		{StateCompletable, HUDQuestStateReadyToReturn, true},
		{StateTurnedIn, HUDQuestStateCompleted, true},
		{StateUnavailable, "", false},
		{StateOffered, "", false},
		{StateAbandoned, "", false},
		{StateUnspecified, "", false},
	}
	for _, test := range tests {
		got, ok := HUDState(test.internal)
		if got != test.want || ok != test.ok {
			t.Errorf("HUDState(%s) = %q, %t, want %q, %t", test.internal, got, ok, test.want, test.ok)
		}
	}
	if HUDQuestStateFailed != "FAILED" {
		t.Fatalf("FAILED vocabulary = %q", HUDQuestStateFailed)
	}
}

func TestHUDBookUsesTheTwentyVisibleRowsAndThreeBookmarks(t *testing.T) {
	definitions := []pack.Quest{
		hudTestDefinition("quest.test.a"),
		hudTestDefinition("quest.test.b"),
		hudTestDefinition("quest.test.completed"),
		hudTestDefinition("quest.test.abandoned"),
	}
	catalog := hudTestCatalog(t, definitions...)
	log := &questLog{instances: map[string]*instance{
		"quest.test.a":         {questID: "quest.test.a", state: StateAccepted, counters: []int32{0, 0}},
		"quest.test.b":         {questID: "quest.test.b", state: StateInProgress, counters: []int32{1, 0}},
		"quest.test.completed": {questID: "quest.test.completed", state: StateTurnedIn, counters: []int32{3, 2}},
		"quest.test.abandoned": {questID: "quest.test.abandoned", state: StateAbandoned, counters: []int32{0, 0}},
	}}

	book, err := projectHUDBook(catalog, log, HUDBookState{
		SelectedQuestID:  "quest.test.b",
		BookmarkQuestIDs: []string{"quest.test.b", "quest.test.a"},
	})
	if err != nil {
		t.Fatalf("projectHUDBook() error = %v", err)
	}
	if got := len(book.Quests); got != 2 {
		t.Fatalf("visible quests = %d, want 2", got)
	}
	if book.Quests[0].ID != "quest.test.a" || book.Quests[1].ID != "quest.test.b" {
		t.Errorf("visible order = %q, %q, want canonical id order", book.Quests[0].ID, book.Quests[1].ID)
	}
	if got := len(book.Bookmarks); got != 2 || book.Bookmarks[0].ID != "quest.test.b" {
		t.Errorf("bookmarks = %+v, want caller-owned order", book.Bookmarks)
	}
	if book.Selected == nil || book.Selected.Info.ID != "quest.test.b" {
		t.Fatalf("selected = %+v, want quest.test.b", book.Selected)
	}
	if book.Secrets != nil || book.DailyCount != nil || book.DailyLimit != nil {
		t.Error("the book invented secret or daily quest state")
	}

	completed, err := hudProgress(definitions[2], log.instances["quest.test.completed"])
	if err != nil {
		t.Fatalf("hudProgress(completed) error = %v", err)
	}
	if completed.State != HUDQuestStateCompleted {
		t.Errorf("turned-in state = %q, want COMPLETED", completed.State)
	}
	if got := visibleQuestCount(log); got != 2 {
		t.Errorf("visibleQuestCount() = %d, want terminal rows excluded", got)
	}
}

func TestModuleHUDReadAPIsUseTheLiveLogWithoutMutatingIt(t *testing.T) {
	definition := hudTestDefinition("quest.test.live")
	catalog := hudTestCatalog(t, definition)
	characterID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	held := &instance{questID: definition.ID, state: StateInProgress, counters: []int32{2, 1}}
	module := New(nil, hudZone{}, catalog, nil)
	module.logs[characterID] = &questLog{
		characterID: characterID,
		instances:   map[string]*instance{definition.ID: held},
	}

	book, found, err := module.HUDBook(characterID, HUDBookState{SelectedQuestID: definition.ID})
	if err != nil || !found || book.Selected == nil {
		t.Fatalf("HUDBook() = found %t, error %v, selected %+v", found, err, book.Selected)
	}
	detail, found, err := module.HUDSelect(characterID, definition.ID)
	if err != nil || !found || detail.Progress.State != HUDQuestStateInProgress {
		t.Fatalf("HUDSelect() = found %t, error %v, detail %+v", found, err, detail)
	}
	info, found, err := module.HUDNPCInfo(characterID, definition.ID)
	if err != nil || !found || info.State == nil || *info.State != HUDQuestStateInProgress {
		t.Fatalf("HUDNPCInfo() = found %t, error %v, info %+v", found, err, info)
	}
	progress, found, err := module.HUDProgress(characterID, definition.ID)
	if err != nil || !found || progress.State != HUDQuestStateInProgress {
		t.Fatalf("HUDProgress() = found %t, error %v, progress %+v", found, err, progress)
	}
	if held.state != StateInProgress || held.counters[0] != 2 || held.counters[1] != 1 {
		t.Errorf("HUD reads changed the live instance to %+v", held)
	}
}

func TestHUDBookRejectsAnInvisibleSelectionOrBookmark(t *testing.T) {
	definition := hudTestDefinition("quest.test.completed")
	catalog := hudTestCatalog(t, definition)
	log := &questLog{instances: map[string]*instance{
		definition.ID: {questID: definition.ID, state: StateTurnedIn, counters: []int32{3, 2}},
	}}

	_, err := projectHUDBook(catalog, log, HUDBookState{SelectedQuestID: definition.ID})
	if !errors.Is(err, ErrHUDQuestNotVisible) {
		t.Errorf("selection error = %v, want ErrHUDQuestNotVisible", err)
	}
	_, err = projectHUDBook(catalog, log, HUDBookState{BookmarkQuestIDs: []string{definition.ID}})
	if !errors.Is(err, ErrHUDQuestNotVisible) {
		t.Errorf("bookmark error = %v, want ErrHUDQuestNotVisible", err)
	}
}

func TestHUDProjectionRejectsEveryRetailCapacityOverflow(t *testing.T) {
	objective := HUDQuestObjective{}
	reward := HUDRewardItem{}
	reputation := HUDReputationReward{}
	currency := HUDCurrencyReward{}
	summary := HUDQuestSummary{}
	secret := HUDQuestSecret{}

	tests := []struct {
		name string
		err  error
	}{
		{"detail objectives", HUDQuestDetail{Progress: HUDQuestProgress{Objectives: repeat(objective, 6)}}.Validate()},
		{"NPC objectives", HUDNPCQuestInfo{Objectives: repeat(objective, 7)}.Validate()},
		{"mandatory rewards", HUDQuestRewards{MandatoryItems: repeat(reward, 6)}.Validate()},
		{"alternative rewards", HUDQuestRewards{AlternativeItems: repeat(reward, 6)}.Validate()},
		{"reputation rewards", HUDQuestRewards{Reputations: repeat(reputation, 6)}.Validate()},
		{"currency rewards", HUDQuestRewards{Currencies: repeat(currency, 6)}.Validate()},
		{"visible quests", HUDQuestBook{Quests: repeat(summary, 21)}.Validate()},
		{"bookmarks", HUDQuestBook{Bookmarks: repeat(summary, 4)}.Validate()},
		{"secrets", HUDQuestBook{Secrets: repeat(secret, 16)}.Validate()},
	}
	for _, test := range tests {
		if !errors.Is(test.err, ErrHUDCapacityExceeded) {
			t.Errorf("%s error = %v, want ErrHUDCapacityExceeded", test.name, test.err)
		}
	}
}

func TestHUDProjectionDoesNotTruncateOverflowingContent(t *testing.T) {
	definition := hudTestDefinition("quest.test.too-many-rewards")
	definition.Rewards.MandatoryItems = make([]pack.QuestRewardItem, 6)
	for index := range definition.Rewards.MandatoryItems {
		definition.Rewards.MandatoryItems[index] = pack.QuestRewardItem{ItemID: "item.test.reward", Count: 1}
	}
	catalog := hudTestCatalog(t, definition)
	_, found, err := catalog.HUDInfo(definition.ID)
	if !found {
		t.Fatal("HUDInfo() hid the authored definition after projection failed")
	}
	if !errors.Is(err, ErrHUDCapacityExceeded) {
		t.Fatalf("HUDInfo() error = %v, want ErrHUDCapacityExceeded", err)
	}
}

func TestObjectivePanelsEnforceTheirDifferentAuthoredLimits(t *testing.T) {
	definition := hudTestDefinition("quest.test.six-objectives")
	definition.Objectives = repeat(definition.Objectives[0], HUDNPCInfoObjectiveLimit)
	held := &instance{
		questID:  definition.ID,
		state:    StateInProgress,
		counters: make([]int32, HUDNPCInfoObjectiveLimit),
	}
	if _, err := hudNPCInfo(definition, held); err != nil {
		t.Fatalf("six-objective NPC info error = %v", err)
	}
	if _, err := hudDetail(definition, held); !errors.Is(err, ErrHUDCapacityExceeded) {
		t.Fatalf("six-objective detail error = %v, want ErrHUDCapacityExceeded", err)
	}

	definition.Objectives = append(definition.Objectives, definition.Objectives[0])
	if _, err := hudNPCInfo(definition, held); !errors.Is(err, ErrHUDCapacityExceeded) {
		t.Fatalf("seven-objective NPC info error = %v, want ErrHUDCapacityExceeded", err)
	}
}

func TestTwentyVisibleQuestsFitAndTwentyOneAreRefused(t *testing.T) {
	definitions := make([]pack.Quest, 0, HUDVisibleQuestLimit+1)
	log := &questLog{instances: make(map[string]*instance, HUDVisibleQuestLimit+1)}
	for index := 0; index <= HUDVisibleQuestLimit; index++ {
		id := fmt.Sprintf("quest.test.visible-%02d", index)
		definition := hudTestDefinition(id)
		definitions = append(definitions, definition)
		log.instances[id] = &instance{questID: id, state: StateAccepted, counters: []int32{0, 0}}
	}
	catalog := hudTestCatalog(t, definitions...)
	last := fmt.Sprintf("quest.test.visible-%02d", HUDVisibleQuestLimit)
	delete(log.instances, last)
	if book, err := projectHUDBook(catalog, log, HUDBookState{}); err != nil || len(book.Quests) != 20 {
		t.Fatalf("twenty-row book = %d rows, error %v", len(book.Quests), err)
	}
	log.instances[last] = &instance{questID: last, state: StateAccepted, counters: []int32{0, 0}}
	if _, err := projectHUDBook(catalog, log, HUDBookState{}); !errors.Is(err, ErrHUDCapacityExceeded) {
		t.Fatalf("twenty-one-row book error = %v, want ErrHUDCapacityExceeded", err)
	}
}

func TestNPCInfoLeavesUnacceptedProgressAbsent(t *testing.T) {
	definition := hudTestDefinition("quest.test.offer")
	info, err := hudNPCInfo(definition, nil)
	if err != nil {
		t.Fatalf("hudNPCInfo() error = %v", err)
	}
	if info.State != nil {
		t.Errorf("state = %q, want absent for an unaccepted quest", *info.State)
	}
	for index, objective := range info.Objectives {
		if objective.Progress != nil {
			t.Errorf("objective %d progress = %d, want absent", index, *objective.Progress)
		}
	}
}

func TestShareQuestUsesPartyAuthorityAndSixtySecondExpiry(t *testing.T) {
	sharerID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	recipients := []uuid.UUID{
		uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		uuid.MustParse("33333333-3333-3333-3333-333333333333"),
	}
	partyAuthority := &hudPartyAuthority{recipients: recipients, refusal: party.AudienceAllowed}
	eligibility := &hudShareEligibility{members: map[uuid.UUID]HUDQuestShareRecipientEligibility{
		recipients[0]: {SameZone: true, DistanceM: 4, Alive: true, CanStartQuest: true},
		recipients[1]: {SameZone: true, DistanceM: 20, Alive: true, CanStartQuest: true},
	}}
	state := NewHUDQuestShareState(sharerID, "Harbor Runner", partyAuthority, eligibility)
	now := time.Date(2026, 8, 21, 19, 0, 0, 0, time.UTC)
	clock := now
	state.now = func() time.Time { return clock }
	shareSequence := 0
	state.newShareID = func() string {
		shareSequence++
		return fmt.Sprintf("share.test.%d", shareSequence)
	}

	result := state.ShareQuest(t.Context(), "quest.test.tide-ledger")
	if result.Refusal != HUDQuestShareRefusalNone {
		t.Fatalf("refusal = %q, want none", result.Refusal)
	}
	if partyAuthority.sharerID != sharerID {
		t.Errorf("party authority received sharer %s, want %s", partyAuthority.sharerID, sharerID)
	}
	if got := len(result.Recipients); got != len(recipients) {
		t.Fatalf("invites = %d, want %d from party authority", got, len(recipients))
	}
	for index, recipient := range result.Recipients {
		if recipient.RecipientCharacterID != recipients[index] {
			t.Errorf("invite %d recipient = %s, want %s", index, recipient.RecipientCharacterID, recipients[index])
		}
		if recipient.Invite == nil {
			t.Fatalf("recipient %d has no invitation", index)
		}
		invite := *recipient.Invite
		if invite.ShareID != fmt.Sprintf("share.test.%d", index+1) ||
			invite.QuestID != "quest.test.tide-ledger" ||
			invite.SharerName != "Harbor Runner" {
			t.Errorf("invite %d = %+v, want exact bound/request values", index, invite)
		}
		if want := now.Add(HUDQuestShareOnRequestExpiry); !invite.ExpiresAt.Equal(want) {
			t.Errorf("invite %d expires at %s, want %s", index, invite.ExpiresAt, want)
		}
		if invite.OnStart {
			t.Errorf("invite %d OnStart = true for a manual share", index)
		}
	}
	assertHUDGolden(t, "hud_share_request.golden.json", result)
	if got := len(state.PendingInvites()); got != 2 {
		t.Fatalf("pending invites = %d, want 2", got)
	}
	clock = now.Add(10 * time.Second)
	repeated := state.ShareQuest(t.Context(), "quest.test.tide-ledger")
	if got := len(repeated.Recipients); got != 2 {
		t.Fatalf("repeated recipient results = %d, want 2", got)
	}
	for index, recipient := range repeated.Recipients {
		if recipient.Invite == nil || recipient.Invite.ShareID != fmt.Sprintf("share.test.%d", index+1) ||
			!recipient.Invite.ExpiresAt.Equal(now.Add(HUDQuestShareOnRequestExpiry)) || recipient.Invite.OnStart {
			t.Errorf("repeated invite %d = %+v, want the unexpired original", index, recipient.Invite)
		}
	}
	clock = now.Add(HUDQuestShareOnRequestExpiry)
	if got := state.PendingInvites(); len(got) != 0 {
		t.Errorf("pending invites at expiry = %+v, want none", got)
	}
}

func TestShareQuestReturnsTypedNoPartyRefusal(t *testing.T) {
	state := NewHUDQuestShareState(
		uuid.New(), "Solo", &hudPartyAuthority{refusal: party.AudienceNoParty}, nil,
	)
	result := state.ShareQuest(t.Context(), "quest.test.offer")
	if result.Refusal != HUDQuestShareRefusalNoParty {
		t.Fatalf("refusal = %q, want NO_PARTY", result.Refusal)
	}
	if result.Recipients != nil {
		t.Errorf("recipients = %+v, want none", result.Recipients)
	}
}

func TestShareQuestMapsUnavailableAudienceToNotPossible(t *testing.T) {
	state := NewHUDQuestShareState(
		uuid.New(), "Offline", &hudPartyAuthority{refusal: party.AudienceUnavailable}, nil,
	)
	result := state.ShareQuest(t.Context(), "quest.test.offer")
	if result.Refusal != HUDQuestShareRefusalNotPossible {
		t.Fatalf("refusal = %q, want NOT_POSSIBLE", result.Refusal)
	}
}

func TestShareQuestRefusesAQuestTheSharerDoesNotHold(t *testing.T) {
	recipientID := uuid.New()
	state := NewHUDQuestShareState(
		uuid.New(),
		"Sharer",
		&hudPartyAuthority{recipients: []uuid.UUID{recipientID}, refusal: party.AudienceAllowed},
		&hudShareEligibility{cannotShare: true},
	)
	result := state.ShareQuest(t.Context(), "quest.test.not-held")
	if result.Refusal != HUDQuestShareRefusalNotPossible {
		t.Fatalf("refusal = %q, want NOT_POSSIBLE", result.Refusal)
	}
	if result.Recipients != nil {
		t.Errorf("recipients = %+v, want none", result.Recipients)
	}
}

func TestModuleOwnsLiveQuestShareEligibility(t *testing.T) {
	definition := hudTestDefinition("quest.test.shareable")
	catalog := hudTestCatalog(t, definition)
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               definition.ZoneID,
		TickInterval:     10 * time.Millisecond,
		SnapshotInterval: 20 * time.Millisecond,
		MaxMoveSpeed:     6,
	})
	if err != nil {
		t.Fatalf("world.NewZone() error = %v", err)
	}
	sharerEntityID, _ := zone.JoinAt(world.Vec3{X: 0}, 0)
	recipientEntityID, _ := zone.JoinAt(world.Vec3{X: 3}, 0)
	sharerID := uuid.New()
	recipientID := uuid.New()
	module := New(nil, zone, catalog, nil)
	module.logs[sharerID] = &questLog{
		characterID: sharerID,
		instances: map[string]*instance{
			definition.ID: {questID: definition.ID, state: StateAccepted, counters: []int32{0, 0}},
		},
		level: 7,
	}
	module.logs[recipientID] = &questLog{
		characterID: recipientID,
		instances:   make(map[string]*instance),
		level:       7,
	}
	module.actors[sharerEntityID] = sharerID
	module.actors[recipientEntityID] = recipientID

	if !module.CanShareQuest(sharerID, definition.ID) {
		t.Fatal("CanShareQuest() = false for a visible held quest")
	}
	eligibility, resolved := module.QuestShareEligibility(sharerID, recipientID, definition.ID)
	if !resolved || !eligibility.SameZone || eligibility.DistanceM != 3 ||
		!eligibility.Alive || !eligibility.CanStartQuest {
		t.Fatalf("QuestShareEligibility() = %+v, %t, want same-zone 3m alive and startable", eligibility, resolved)
	}

	module.logs[recipientID].instances[definition.ID] = &instance{
		questID: definition.ID,
		state:   StateAccepted,
	}
	eligibility, resolved = module.QuestShareEligibility(sharerID, recipientID, definition.ID)
	if !resolved || eligibility.CanStartQuest {
		t.Errorf("held quest eligibility = %+v, %t, want can-start false", eligibility, resolved)
	}

	delete(module.logs, recipientID)
	delete(module.actors, recipientEntityID)
	eligibility, resolved = module.QuestShareEligibility(sharerID, recipientID, definition.ID)
	if !resolved || eligibility.SameZone {
		t.Errorf("other-zone eligibility = %+v, %t, want resolved same-zone false", eligibility, resolved)
	}
}

func TestShareQuestFiltersRangeAndReportsMixedNotPossibleResults(t *testing.T) {
	sharerID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	valid := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	dead := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	ineligible := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	far := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	otherZone := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	partyAuthority := &hudPartyAuthority{
		refusal: party.AudienceAllowed,
		recipients: []uuid.UUID{
			sharerID, valid, valid, dead, ineligible, far, otherZone,
		},
	}
	eligibility := &hudShareEligibility{members: map[uuid.UUID]HUDQuestShareRecipientEligibility{
		valid:      {SameZone: true, DistanceM: 1, Alive: true, CanStartQuest: true},
		dead:       {SameZone: true, DistanceM: 2, Alive: false, CanStartQuest: true},
		ineligible: {SameZone: true, DistanceM: 3, Alive: true, CanStartQuest: false},
		far:        {SameZone: true, DistanceM: 20.01, Alive: true, CanStartQuest: true},
		otherZone:  {SameZone: false, DistanceM: 1, Alive: true, CanStartQuest: true},
	}}
	state := NewHUDQuestShareState(sharerID, "Sharer", partyAuthority, eligibility)
	state.now = func() time.Time { return time.Unix(100, 0) }
	state.newShareID = func() string { return "share.mixed" }

	result := state.ShareQuest(t.Context(), "quest.test.mixed")
	if result.Refusal != HUDQuestShareRefusalNone {
		t.Fatalf("request refusal = %q, want none", result.Refusal)
	}
	if got := len(result.Recipients); got != 3 {
		t.Fatalf("recipient results = %d, want one invite and two refusals", got)
	}
	if result.Recipients[0].RecipientCharacterID != valid || result.Recipients[0].Invite == nil {
		t.Errorf("valid result = %+v, want an invitation", result.Recipients[0])
	}
	for index, recipientID := range []uuid.UUID{dead, ineligible} {
		recipientResult := result.Recipients[index+1]
		if recipientResult.RecipientCharacterID != recipientID ||
			recipientResult.Refusal != HUDQuestShareRefusalNotPossible {
			t.Errorf("result %d = %+v, want %s NOT_POSSIBLE", index+1, recipientResult, recipientID)
		}
	}
}

func TestOnStartQuestShareUsesTenSecondExpiry(t *testing.T) {
	sharerID := uuid.MustParse("11111111-aaaa-aaaa-aaaa-111111111111")
	recipientID := uuid.MustParse("22222222-bbbb-bbbb-bbbb-222222222222")
	partyAuthority := &hudPartyAuthority{
		recipients: []uuid.UUID{recipientID}, refusal: party.AudienceAllowed,
	}
	eligibility := &hudShareEligibility{members: map[uuid.UUID]HUDQuestShareRecipientEligibility{
		recipientID: {SameZone: true, DistanceM: 5, Alive: true, CanStartQuest: true},
	}}
	state := NewHUDQuestShareState(sharerID, "Starter", partyAuthority, eligibility)
	now := time.Unix(200, 0).UTC()
	state.now = func() time.Time { return now }
	state.newShareID = func() string { return "share.on-start" }

	result := state.ShareQuestOnStart(t.Context(), "quest.test.started")
	if len(result.Recipients) != 1 || result.Recipients[0].Invite == nil {
		t.Fatalf("result = %+v, want one invitation", result)
	}
	if got, want := result.Recipients[0].Invite.ExpiresAt, now.Add(HUDQuestShareOnStartExpiry); !got.Equal(want) {
		t.Errorf("expires at %s, want %s", got, want)
	}
	if !result.Recipients[0].Invite.OnStart {
		t.Error("OnStart = false for an automatic share")
	}
	assertHUDGolden(t, "hud_share_on_start.golden.json", result)
}

func TestRetailQuestHUDTimingAndRangeConstants(t *testing.T) {
	if HUDQuestShareOnRequestExpiry != 60*time.Second {
		t.Errorf("on-request share expiry = %s, want 60s", HUDQuestShareOnRequestExpiry)
	}
	if HUDQuestShareOnStartExpiry != 10*time.Second {
		t.Errorf("on-start share expiry = %s, want 10s", HUDQuestShareOnStartExpiry)
	}
	if HUDQuestShareRangeM != 20 {
		t.Errorf("share range = %g m, want 20", HUDQuestShareRangeM)
	}
}

type hudPartyAuthority struct {
	sharerID   uuid.UUID
	recipients []uuid.UUID
	refusal    party.AudienceRefusal
}

func (authority *hudPartyAuthority) Audience(
	_ context.Context,
	sharerID uuid.UUID,
) ([]uuid.UUID, party.AudienceRefusal) {
	authority.sharerID = sharerID
	return append([]uuid.UUID(nil), authority.recipients...), authority.refusal
}

type hudShareEligibility struct {
	members     map[uuid.UUID]HUDQuestShareRecipientEligibility
	cannotShare bool
}

func (eligibility *hudShareEligibility) CanShareQuest(uuid.UUID, string) bool {
	return eligibility != nil && !eligibility.cannotShare
}

func (eligibility *hudShareEligibility) QuestShareEligibility(
	_ uuid.UUID,
	recipientCharacterID uuid.UUID,
	_ string,
) (HUDQuestShareRecipientEligibility, bool) {
	member, ok := eligibility.members[recipientCharacterID]
	return member, ok
}

type hudZone struct{}

func (hudZone) ID() string { return "zone.test.paper-harbor" }

func (hudZone) GameCommand(command func(gametypes.Tick) error) error { return command(nil) }

func (hudZone) GameAddSystem(gametypes.System) {}

func hudTestCatalog(t *testing.T, definitions ...pack.Quest) Catalog {
	t.Helper()
	catalog, err := NewCatalog(definitions, nil)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return catalog
}

func hudTestDefinition(id string) pack.Quest {
	return pack.Quest{
		ID:            id,
		ZoneID:        "zone.test.paper-harbor",
		Level:         7,
		RequiredLevel: 4,
		QuestType:     "STORY",
		StarterID:     "mob.test.quartermaster",
		FinisherID:    "mob.test.harbor-master",
		CanCancel:     true,
		NameKey:       "quest.test.tide-ledger.name",
		GoalKey:       "quest.test.tide-ledger.goal",
		StartKey:      "quest.test.tide-ledger.start",
		CheckKey:      "quest.test.tide-ledger.check",
		FinishKey:     "quest.test.tide-ledger.finish",
		Objectives: []pack.QuestObjective{
			{
				Kind:       pack.QuestObjectiveCountKill,
				Limit:      3,
				TargetIDs:  []string{"mob.test.tide-crab"},
				ShowCount:  true,
				CounterKey: "quest.test.tide-ledger.crabs",
			},
			{
				Kind:       pack.QuestObjectiveCountItem,
				Limit:      2,
				TargetIDs:  []string{"item.test.tonic", "item.test.elixir"},
				CounterKey: "quest.test.tide-ledger.supplies",
			},
		},
		Rewards: pack.QuestRewards{
			Experience: 450,
			Money:      27,
			Honor:      3,
			MandatoryItems: []pack.QuestRewardItem{
				{ItemID: "item.test.token", Count: 2},
				{ItemID: "item.test.note", Count: 1, Hidden: true},
			},
			AlternativeItems: []pack.QuestRewardItem{
				{ItemID: "item.test.sword", Count: 1},
			},
		},
	}
}

func assertHUDGolden(t *testing.T, name string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent() error = %v", err)
	}
	encoded = append(encoded, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("%s mismatch\n--- got ---\n%s--- want ---\n%s", name, encoded, want)
	}
}

func repeat[T any](value T, count int) []T {
	values := make([]T, count)
	for index := range values {
		values[index] = value
	}
	return values
}
