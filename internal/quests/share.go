package quests

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/gametypes"
	"github.com/SarnautCore/server/internal/party"
)

type questShareInviteKey struct {
	InviteID  uint64
	Recipient uuid.UUID
}

type questSharePendingKey struct {
	QuestID   string
	Recipient uuid.UUID
}

// SetPartyAudience binds the authoritative connected party roster. New keeps
// its existing signature because party composition is shard-level wiring and
// may be installed after zone modules are constructed.
func (module *Module) SetPartyAudience(reader party.AudienceReader) {
	if module == nil {
		return
	}
	module.shareMu.Lock()
	module.partyAudience = reader
	module.shareMu.Unlock()
}

// ShareQuest creates manual invitations with a sixty-second lifetime. The
// session supplies the authenticated character identity and its display name;
// the client command supplies only questID.
func (module *Module) ShareQuest(
	ctx context.Context,
	sharerCharacterID uuid.UUID,
	sharerName string,
	questID string,
) HUDQuestShareResult {
	return module.shareQuest(
		ctx,
		sharerCharacterID,
		sharerName,
		questID,
		HUDQuestShareOnRequestExpiry,
		false,
	)
}

// ShareQuestOnStart creates the automatic ten-second invitations. A manual
// and automatic share use the same pending key, so neither duplicates nor
// changes the lifetime of an existing invitation.
func (module *Module) ShareQuestOnStart(
	ctx context.Context,
	sharerCharacterID uuid.UUID,
	sharerName string,
	questID string,
) HUDQuestShareResult {
	return module.shareQuest(
		ctx,
		sharerCharacterID,
		sharerName,
		questID,
		HUDQuestShareOnStartExpiry,
		true,
	)
}

func (module *Module) shareQuest(
	ctx context.Context,
	sharerCharacterID uuid.UUID,
	sharerName string,
	questID string,
	ttl time.Duration,
	onStart bool,
) HUDQuestShareResult {
	if module == nil {
		return HUDQuestShareResult{Refusal: HUDQuestShareRefusalNotShareable}
	}
	if _, found := module.catalog.Definition(questID); !found {
		return HUDQuestShareResult{Refusal: HUDQuestShareRefusalUnknownQuest}
	}

	module.shareMu.Lock()
	partyAudience := module.partyAudience
	module.shareMu.Unlock()
	if partyAudience == nil {
		return HUDQuestShareResult{Refusal: HUDQuestShareRefusalNoParty}
	}

	recipients, audienceRefusal := partyAudience.Audience(ctx, sharerCharacterID)
	switch audienceRefusal {
	case party.AudienceAllowed:
	case party.AudienceNoParty:
		return HUDQuestShareResult{Refusal: HUDQuestShareRefusalNoParty}
	default:
		return HUDQuestShareResult{Refusal: HUDQuestShareRefusalNotShareable}
	}

	sharerEntityID, eligible, shareable := module.questShareSnapshot(
		sharerCharacterID,
		recipients,
		questID,
	)
	if !shareable {
		return HUDQuestShareResult{Refusal: HUDQuestShareRefusalNotShareable}
	}

	result := HUDQuestShareResult{
		Recipients: make([]HUDQuestShareRecipientResult, 0, len(recipients)),
	}
	seen := make(map[uuid.UUID]struct{}, len(recipients))
	for _, recipientID := range recipients {
		if recipientID == sharerCharacterID {
			continue
		}
		if _, duplicate := seen[recipientID]; duplicate {
			continue
		}
		seen[recipientID] = struct{}{}

		member, resolved := eligible[recipientID]
		if resolved && (!member.SameZone || member.DistanceM > HUDQuestShareRangeM) {
			continue
		}
		if !resolved || member.DistanceM < 0 || !member.Alive || !member.CanStartQuest {
			result.Recipients = append(result.Recipients, HUDQuestShareRecipientResult{
				RecipientCharacterID: recipientID,
				Refusal:              HUDQuestShareRefusalTargetUnavailable,
			})
			continue
		}

		invite := module.createQuestShareInvite(HUDQuestShareInvite{
			QuestID:              questID,
			SharerName:           sharerName,
			SharerCharacterID:    sharerCharacterID,
			SharerEntityID:       sharerEntityID,
			RecipientCharacterID: recipientID,
			OnStart:              onStart,
		}, ttl)
		result.Recipients = append(result.Recipients, HUDQuestShareRecipientResult{
			RecipientCharacterID: recipientID,
			Invite:               &invite,
		})
	}
	return result
}

// Respond claims one invitation for its authenticated recipient. Claiming and
// removing happen under one mutex, so concurrent accepts, declines, and replay
// attempts cannot start a quest twice.
func (module *Module) Respond(
	ctx context.Context,
	recipientCharacterID uuid.UUID,
	inviteID uint64,
	accept bool,
) (HUDQuestShareResponseResult, error) {
	response := HUDQuestShareResponseResult{
		RecipientCharacterID: recipientCharacterID,
		InviteID:             inviteID,
	}
	if module == nil || recipientCharacterID == uuid.Nil || inviteID == 0 {
		response.Refusal = HUDQuestShareRefusalInviteNotFound
		return response, nil
	}

	module.shareMu.Lock()
	module.expireQuestShareInvitesLocked(module.shareNow())
	key := questShareInviteKey{InviteID: inviteID, Recipient: recipientCharacterID}
	invite, found := module.shareInvites[key]
	if found {
		module.removeQuestShareInviteLocked(key, invite)
	}
	module.shareMu.Unlock()
	if !found {
		response.Refusal = HUDQuestShareRefusalInviteNotFound
		return response, nil
	}
	response.QuestID = invite.QuestID
	if !accept {
		response.Refusal = HUDQuestShareRefusalDeclined
		return response, nil
	}

	questResult, refusal, err := module.acceptSharedQuest(
		ctx,
		recipientCharacterID,
		invite.QuestID,
	)
	response.QuestResult = questResult
	response.Refusal = refusal
	return response, err
}

// PendingQuestShareInvites returns one recipient's unexpired invitations by
// numeric id. It never exposes another character's routing UUID.
func (module *Module) PendingQuestShareInvites(recipientCharacterID uuid.UUID) []HUDQuestShareInvite {
	if module == nil || recipientCharacterID == uuid.Nil {
		return nil
	}
	module.shareMu.Lock()
	defer module.shareMu.Unlock()
	module.expireQuestShareInvitesLocked(module.shareNow())

	invites := make([]HUDQuestShareInvite, 0)
	for key, invite := range module.shareInvites {
		if key.Recipient == recipientCharacterID {
			invites = append(invites, invite)
		}
	}
	sort.Slice(invites, func(left, right int) bool {
		return invites[left].InviteID < invites[right].InviteID
	})
	return invites
}

func (module *Module) createQuestShareInvite(candidate HUDQuestShareInvite, ttl time.Duration) HUDQuestShareInvite {
	module.shareMu.Lock()
	defer module.shareMu.Unlock()
	now := module.shareNow()
	module.expireQuestShareInvitesLocked(now)
	pendingKey := questSharePendingKey{
		QuestID:   candidate.QuestID,
		Recipient: candidate.RecipientCharacterID,
	}
	if inviteKey, found := module.sharePending[pendingKey]; found {
		return module.shareInvites[inviteKey]
	}

	for {
		module.nextShareInviteID++
		if module.nextShareInviteID == 0 {
			continue
		}
		candidate.InviteID = module.nextShareInviteID
		key := questShareInviteKey{
			InviteID:  candidate.InviteID,
			Recipient: candidate.RecipientCharacterID,
		}
		if _, collision := module.shareInvites[key]; collision {
			continue
		}
		candidate.ExpiresAt = now.Add(ttl)
		module.shareInvites[key] = candidate
		module.sharePending[pendingKey] = key
		return candidate
	}
}

func (module *Module) expireQuestShareInvitesLocked(now time.Time) {
	for key, invite := range module.shareInvites {
		if !now.Before(invite.ExpiresAt) {
			module.removeQuestShareInviteLocked(key, invite)
		}
	}
}

func (module *Module) removeQuestShareInviteLocked(
	key questShareInviteKey,
	invite HUDQuestShareInvite,
) {
	delete(module.shareInvites, key)
	pendingKey := questSharePendingKey{
		QuestID:   invite.QuestID,
		Recipient: invite.RecipientCharacterID,
	}
	if module.sharePending[pendingKey] == key {
		delete(module.sharePending, pendingKey)
	}
}

func (module *Module) questShareSnapshot(
	sharerCharacterID uuid.UUID,
	recipients []uuid.UUID,
	questID string,
) (uint64, map[uuid.UUID]HUDQuestShareRecipientEligibility, bool) {
	eligible := make(map[uuid.UUID]HUDQuestShareRecipientEligibility, len(recipients))
	var (
		sharerEntityID uint64
		shareable      bool
	)
	_ = module.zone.GameCommand(func(tick gametypes.Tick) error {
		definition, found := module.catalog.Definition(questID)
		if !found {
			return nil
		}
		sharerLog, found := module.logs[sharerCharacterID]
		if !found {
			return nil
		}
		held, found := sharerLog.instances[questID]
		if !found || !hudVisible(held.state) {
			return nil
		}
		sharerEntityID, found = module.entityForCharacter(sharerCharacterID)
		if !found {
			return nil
		}
		sharer := tick.Entity(sharerEntityID)
		if sharer == nil {
			return nil
		}
		shareable = true
		for _, recipientID := range recipients {
			recipientEntityID, present := module.entityForCharacter(recipientID)
			if !present {
				eligible[recipientID] = HUDQuestShareRecipientEligibility{SameZone: false}
				continue
			}
			recipient := tick.Entity(recipientEntityID)
			recipientLog, hasLog := module.logs[recipientID]
			if recipient == nil || !hasLog {
				continue
			}
			eligible[recipientID] = HUDQuestShareRecipientEligibility{
				SameZone:      true,
				DistanceM:     gametypes.Distance(tick.Position(sharer), tick.Position(recipient)),
				Alive:         recipient.Alive,
				CanStartQuest: module.gate(recipientLog, definition) == StateOffered,
			}
		}
		return nil
	})
	return sharerEntityID, eligible, shareable
}

func (module *Module) acceptSharedQuest(
	ctx context.Context,
	characterID uuid.UUID,
	questID string,
) (Result, HUDQuestShareRefusal, error) {
	definition, found := module.catalog.Definition(questID)
	if !found {
		return Result{}, HUDQuestShareRefusalUnknownQuest, nil
	}

	var (
		created *instance
		refusal HUDQuestShareRefusal
	)
	_ = module.zone.GameCommand(func(tick gametypes.Tick) error {
		log, present := module.logs[characterID]
		if !present {
			refusal = HUDQuestShareRefusalTargetUnavailable
			return nil
		}
		entityID, present := module.entityForCharacter(characterID)
		if !present {
			refusal = HUDQuestShareRefusalTargetUnavailable
			return nil
		}
		actor := tick.Entity(entityID)
		switch {
		case actor == nil || !actor.Alive:
			refusal = HUDQuestShareRefusalTargetUnavailable
		case module.gate(log, definition) != StateOffered:
			refusal = HUDQuestShareRefusalNotShareable
		case visibleQuestCount(log) >= QuestLogCapacity:
			refusal = HUDQuestShareRefusalLogFull
		default:
			created = &instance{
				questID:        questID,
				counters:       make([]int32, len(definition.Objectives)),
				acceptedAtTick: tick.Number(),
				inFlight:       true,
			}
			created.state = progressState(definition, created.counters)
			log.instances[questID] = created
			module.trackItems(log, nil)
		}
		return nil
	})
	if refusal != HUDQuestShareRefusalNone {
		return Result{}, refusal, nil
	}
	if created == nil {
		return Result{}, HUDQuestShareRefusalInternal, fmt.Errorf("quests: shared accept created no quest instance")
	}

	row, err := created.row()
	if err != nil {
		module.rollBackAccept(characterID, questID)
		return Result{}, HUDQuestShareRefusalInternal, err
	}
	if module.granter == nil {
		module.rollBackAccept(characterID, questID)
		return Result{}, HUDQuestShareRefusalInternal, fmt.Errorf("quests: shared accept has no granter")
	}
	granted, err := module.granter.GrantQuestReward(ctx, Grant{
		CharacterID: characterID,
		Quest:       row,
	})
	if err != nil {
		module.rollBackAccept(characterID, questID)
		module.logger.Error("shared quest accept failed",
			"character_id", characterID.String(), "quest_id", questID, "error", err)
		return Result{}, HUDQuestShareRefusalInternal, err
	}

	var update Update
	_ = module.zone.GameCommand(func(gametypes.Tick) error {
		log, present := module.logs[characterID]
		if !present {
			return nil
		}
		held, present := log.instances[questID]
		if !present {
			return nil
		}
		held.inFlight = false
		module.trackItems(log, granted.Inventory)
		module.recountItems(log)
		update = module.updateFor(definition, held)
		return nil
	})
	return Result{
		Update:    update,
		Inventory: granted.Inventory,
		Currency:  granted.Currency,
		SaveSeq:   granted.SaveSeq,
		Committed: true,
	}, HUDQuestShareRefusalNone, nil
}

var _ HUDQuestShareEligibility = (*Module)(nil)
