package quests

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/party"
	"github.com/SarnautCore/server/internal/world"
)

type questShareFixture struct {
	module          *Module
	zone            *world.Zone
	party           *questShareParty
	granter         *questShareGranter
	sharerID        uuid.UUID
	recipientID     uuid.UUID
	deadID          uuid.UUID
	ineligibleID    uuid.UUID
	farID           uuid.UUID
	otherZoneID     uuid.UUID
	sharerEntityID  uint64
	recipientEntity uint64
	questID         string
	now             time.Time
}

func newQuestShareFixture(t *testing.T) *questShareFixture {
	t.Helper()
	definition := hudTestDefinition("quest.test.tide-ledger")
	zone, err := world.NewZone(world.ZoneConfig{
		ID:               definition.ZoneID,
		TickInterval:     10 * time.Millisecond,
		SnapshotInterval: 20 * time.Millisecond,
		MaxMoveSpeed:     6,
	})
	if err != nil {
		t.Fatalf("world.NewZone() error = %v", err)
	}
	granter := new(questShareGranter)
	module := New(nil, zone, hudTestCatalog(t, definition), granter)
	fixture := &questShareFixture{
		module:       module,
		zone:         zone,
		party:        new(questShareParty),
		granter:      granter,
		sharerID:     uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		recipientID:  uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		deadID:       uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		ineligibleID: uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		farID:        uuid.MustParse("55555555-5555-5555-5555-555555555555"),
		otherZoneID:  uuid.MustParse("66666666-6666-6666-6666-666666666666"),
		questID:      definition.ID,
		now:          time.Date(2026, 8, 21, 19, 0, 0, 0, time.UTC),
	}
	fixture.sharerEntityID = fixture.admit(t, fixture.sharerID, 0, true)
	fixture.recipientEntity = fixture.admit(t, fixture.recipientID, 20, false)
	deadEntity := fixture.admit(t, fixture.deadID, 6, false)
	fixture.admit(t, fixture.ineligibleID, 7, true)
	fixture.admit(t, fixture.farID, 20.01, false)
	_ = zone.GameCommand(func(tick gametypes.Tick) error {
		tick.Entity(deadEntity).Alive = false
		return nil
	})
	fixture.party.members = []uuid.UUID{
		fixture.sharerID,
		fixture.recipientID,
		fixture.recipientID,
		fixture.deadID,
		fixture.ineligibleID,
		fixture.farID,
		fixture.otherZoneID,
	}
	module.SetPartyAudience(fixture.party)
	module.shareNow = func() time.Time { return fixture.now }
	return fixture
}

func (fixture *questShareFixture) admit(
	t *testing.T,
	characterID uuid.UUID,
	x float32,
	holdsQuest bool,
) uint64 {
	t.Helper()
	entityID, _ := fixture.zone.JoinAt(world.Vec3{X: x}, 0)
	loaded := Character{Level: 7}
	if holdsQuest {
		loaded.Quests = []QuestState{{
			QuestID:    fixture.questID,
			State:      StateAccepted.String(),
			Objectives: []byte(`{"counters":[0,0],"accepted_at_tick":1}`),
		}}
	}
	if err := fixture.module.Admit(entityID, characterID, loaded); err != nil {
		t.Fatalf("Admit(%s) error = %v", characterID, err)
	}
	return entityID
}

func (fixture *questShareFixture) share(t *testing.T) HUDQuestShareResult {
	t.Helper()
	result := fixture.module.ShareQuest(
		t.Context(),
		fixture.sharerID,
		"Harbor Runner",
		fixture.questID,
	)
	if result.Refusal != HUDQuestShareRefusalNone {
		t.Fatalf("ShareQuest() refusal = %q, want none", result.Refusal)
	}
	return result
}

func TestQuestShareCreatesMixedResultsAndDedupesPending(t *testing.T) {
	fixture := newQuestShareFixture(t)
	result := fixture.share(t)

	if fixture.party.sharerID != fixture.sharerID {
		t.Errorf("party audience received sharer %s, want %s", fixture.party.sharerID, fixture.sharerID)
	}
	if got := len(result.Recipients); got != 3 {
		t.Fatalf("recipient results = %d, want one invite and two refusals", got)
	}
	invite := result.Recipients[0].Invite
	if result.Recipients[0].RecipientCharacterID != fixture.recipientID || invite == nil {
		t.Fatalf("eligible result = %+v, want recipient invitation", result.Recipients[0])
	}
	if invite.InviteID != 1 || invite.QuestID != fixture.questID ||
		invite.SharerCharacterID != fixture.sharerID ||
		invite.SharerEntityID != fixture.sharerEntityID ||
		invite.RecipientCharacterID != fixture.recipientID ||
		invite.SharerName != "Harbor Runner" || invite.OnStart {
		t.Errorf("invite = %+v, want exact authenticated routing and manual-share values", invite)
	}
	if want := fixture.now.Add(HUDQuestShareOnRequestExpiry); !invite.ExpiresAt.Equal(want) {
		t.Errorf("invite expires at %s, want %s", invite.ExpiresAt, want)
	}
	for index, recipientID := range []uuid.UUID{fixture.deadID, fixture.ineligibleID} {
		recipient := result.Recipients[index+1]
		if recipient.RecipientCharacterID != recipientID ||
			recipient.Refusal != HUDQuestShareRefusalTargetUnavailable {
			t.Errorf("recipient result %d = %+v, want %s TARGET_UNAVAILABLE", index+1, recipient, recipientID)
		}
	}
	assertHUDGolden(t, "hud_share_request.golden.json", result)

	fixture.now = fixture.now.Add(10 * time.Second)
	repeated := fixture.share(t)
	if got := repeated.Recipients[0].Invite; got == nil || got.InviteID != invite.InviteID ||
		!got.ExpiresAt.Equal(invite.ExpiresAt) || got.OnStart {
		t.Errorf("repeated invite = %+v, want unchanged original %+v", got, invite)
	}
	if pending := fixture.module.PendingQuestShareInvites(fixture.recipientID); len(pending) != 1 {
		t.Fatalf("pending invites = %+v, want one deduplicated invitation", pending)
	}
}

func TestQuestShareOnStartUsesTenSecondExpiry(t *testing.T) {
	fixture := newQuestShareFixture(t)
	fixture.party.members = []uuid.UUID{fixture.recipientID}
	result := fixture.module.ShareQuestOnStart(
		t.Context(), fixture.sharerID, "Starter", fixture.questID,
	)
	if len(result.Recipients) != 1 || result.Recipients[0].Invite == nil {
		t.Fatalf("ShareQuestOnStart() = %+v, want one invitation", result)
	}
	invite := result.Recipients[0].Invite
	if !invite.OnStart {
		t.Error("OnStart = false, want true")
	}
	if want := fixture.now.Add(HUDQuestShareOnStartExpiry); !invite.ExpiresAt.Equal(want) {
		t.Errorf("invite expires at %s, want %s", invite.ExpiresAt, want)
	}
	assertHUDGolden(t, "hud_share_on_start.golden.json", result)
}

func TestQuestShareRetailRangeAndLifetimes(t *testing.T) {
	if HUDQuestShareRangeM != 20 {
		t.Errorf("share range = %g m, want 20", HUDQuestShareRangeM)
	}
	if HUDQuestShareOnRequestExpiry != 60*time.Second {
		t.Errorf("manual share lifetime = %s, want 60s", HUDQuestShareOnRequestExpiry)
	}
	if HUDQuestShareOnStartExpiry != 10*time.Second {
		t.Errorf("on-start share lifetime = %s, want 10s", HUDQuestShareOnStartExpiry)
	}
}

func TestQuestShareExpiryForeignResponseDeclineAndReplay(t *testing.T) {
	fixture := newQuestShareFixture(t)
	invite := fixture.share(t).Recipients[0].Invite

	foreign, err := fixture.module.Respond(t.Context(), fixture.deadID, invite.InviteID, true)
	if err != nil || foreign.Refusal != HUDQuestShareRefusalInviteNotFound {
		t.Fatalf("foreign Respond() = %+v, %v, want INVITE_NOT_FOUND", foreign, err)
	}
	if got := fixture.module.PendingQuestShareInvites(fixture.recipientID); len(got) != 1 {
		t.Fatalf("foreign response consumed invitation: %+v", got)
	}

	fixture.now = invite.ExpiresAt
	expired, err := fixture.module.Respond(t.Context(), fixture.recipientID, invite.InviteID, true)
	if err != nil || expired.Refusal != HUDQuestShareRefusalInviteNotFound {
		t.Fatalf("expired Respond() = %+v, %v, want INVITE_NOT_FOUND", expired, err)
	}
	fixture.now = fixture.now.Add(time.Second)
	replacement := fixture.share(t).Recipients[0].Invite
	if replacement.InviteID == invite.InviteID {
		t.Fatalf("replacement invite id = %d, want a fresh id", replacement.InviteID)
	}

	declined, err := fixture.module.Respond(t.Context(), fixture.recipientID, replacement.InviteID, false)
	if err != nil || declined.Refusal != HUDQuestShareRefusalDeclined || declined.QuestID != fixture.questID {
		t.Fatalf("decline Respond() = %+v, %v, want quest-bound DECLINED", declined, err)
	}
	replay, err := fixture.module.Respond(t.Context(), fixture.recipientID, replacement.InviteID, false)
	if err != nil || replay.Refusal != HUDQuestShareRefusalInviteNotFound {
		t.Fatalf("decline replay = %+v, %v, want INVITE_NOT_FOUND", replay, err)
	}
}

func TestQuestShareConcurrentAcceptCommitsOnceAndRejectsReplay(t *testing.T) {
	fixture := newQuestShareFixture(t)
	invite := fixture.share(t).Recipients[0].Invite

	start := make(chan struct{})
	responses := make(chan HUDQuestShareResponseResult, 2)
	errors := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			response, err := fixture.module.Respond(
				context.Background(), fixture.recipientID, invite.InviteID, true,
			)
			responses <- response
			errors <- err
		}()
	}
	close(start)
	accepted, replayed := 0, 0
	for range 2 {
		response, err := <-responses, <-errors
		if err != nil {
			t.Fatalf("Respond() error = %v", err)
		}
		switch response.Refusal {
		case HUDQuestShareRefusalNone:
			accepted++
			if !response.QuestResult.Committed || response.QuestResult.SaveSeq != 17 {
				t.Errorf("accepted quest result = %+v, want committed save sequence 17", response.QuestResult)
			}
		case HUDQuestShareRefusalInviteNotFound:
			replayed++
		default:
			t.Errorf("Respond() refusal = %q, want none or invite-not-found", response.Refusal)
		}
	}
	if accepted != 1 || replayed != 1 {
		t.Fatalf("accepts = %d, replays = %d, want one each", accepted, replayed)
	}
	if calls := fixture.granter.callCount(); calls != 1 {
		t.Fatalf("GrantQuestReward calls = %d, want 1", calls)
	}
	grant := fixture.granter.onlyGrant(t)
	if grant.CharacterID != fixture.recipientID || grant.Quest.QuestID != fixture.questID ||
		grant.Quest.State != StateAccepted.String() {
		t.Errorf("persisted grant = %+v, want recipient's accepted shared quest", grant)
	}
	log := fixture.module.Log(fixture.recipientID)
	if len(log) != 1 || log[0].QuestID != fixture.questID || log[0].State != StateAccepted {
		t.Errorf("recipient log = %+v, want one accepted shared quest", log)
	}
}

func TestQuestShareAcceptRechecksLogCapacity(t *testing.T) {
	fixture := newQuestShareFixture(t)
	invite := fixture.share(t).Recipients[0].Invite
	_ = fixture.zone.GameCommand(func(gametypes.Tick) error {
		log := fixture.module.logs[fixture.recipientID]
		for index := range QuestLogCapacity {
			questID := "quest.test.full." + string(rune('a'+index))
			log.instances[questID] = &instance{questID: questID, state: StateAccepted}
		}
		return nil
	})

	response, err := fixture.module.Respond(t.Context(), fixture.recipientID, invite.InviteID, true)
	if err != nil || response.Refusal != HUDQuestShareRefusalLogFull {
		t.Fatalf("full-log Respond() = %+v, %v, want LOG_FULL", response, err)
	}
	if calls := fixture.granter.callCount(); calls != 0 {
		t.Errorf("GrantQuestReward calls = %d, want 0", calls)
	}
}

func TestQuestShareReturnsTypedRequestRefusals(t *testing.T) {
	fixture := newQuestShareFixture(t)
	unknown := fixture.module.ShareQuest(t.Context(), fixture.sharerID, "Sharer", "quest.missing")
	if unknown.Refusal != HUDQuestShareRefusalUnknownQuest {
		t.Errorf("unknown quest refusal = %q, want UNKNOWN_QUEST", unknown.Refusal)
	}
	fixture.module.SetPartyAudience(&questShareParty{refusal: party.AudienceNoParty})
	noParty := fixture.module.ShareQuest(t.Context(), fixture.sharerID, "Sharer", fixture.questID)
	if noParty.Refusal != HUDQuestShareRefusalNoParty {
		t.Errorf("solo refusal = %q, want NO_PARTY", noParty.Refusal)
	}
	fixture.module.SetPartyAudience(&questShareParty{
		members: []uuid.UUID{fixture.recipientID},
		refusal: party.AudienceAllowed,
	})
	notHeld := fixture.module.ShareQuest(t.Context(), fixture.recipientID, "Recipient", fixture.questID)
	if notHeld.Refusal != HUDQuestShareRefusalNotShareable {
		t.Errorf("not-held refusal = %q, want NOT_SHAREABLE", notHeld.Refusal)
	}
}

type questShareParty struct {
	mu       sync.Mutex
	sharerID uuid.UUID
	members  []uuid.UUID
	refusal  party.AudienceRefusal
}

func (authority *questShareParty) Audience(
	_ context.Context,
	sharerID uuid.UUID,
) ([]uuid.UUID, party.AudienceRefusal) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.sharerID = sharerID
	return append([]uuid.UUID(nil), authority.members...), authority.refusal
}

type questShareGranter struct {
	mu     sync.Mutex
	grants []Grant
}

func (granter *questShareGranter) GrantQuestReward(
	_ context.Context,
	grant Grant,
) (GrantResult, error) {
	granter.mu.Lock()
	defer granter.mu.Unlock()
	granter.grants = append(granter.grants, grant)
	return GrantResult{SaveSeq: 17}, nil
}

func (granter *questShareGranter) callCount() int {
	granter.mu.Lock()
	defer granter.mu.Unlock()
	return len(granter.grants)
}

func (granter *questShareGranter) onlyGrant(t *testing.T) Grant {
	t.Helper()
	granter.mu.Lock()
	defer granter.mu.Unlock()
	if len(granter.grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(granter.grants))
	}
	return granter.grants[0]
}
