// Package chataudience implements world-authoritative local chat routing.
package chataudience

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/chat"
	"github.com/SarnautCore/server/internal/gametypes"
)

const sayRadiusSquared = 10000.0

var (
	ErrMissingSource  = errors.New("chat audience: missing authority source")
	ErrInvalidAvatar  = errors.New("chat audience: invalid authenticated avatar")
	ErrAlreadyPresent = errors.New("chat audience: avatar already present")
	ErrUnknownFaction = errors.New("chat audience: unknown faction")
)

// Observation is the mutable world state read at send time.
type Observation struct {
	Position  gametypes.Vec3
	FactionID string
}

// ObserveAvatar copies one authenticated player entity from the world. It
// returns false after that entity leaves or if the entity is not an avatar.
type ObserveAvatar func() (Observation, bool)

// Avatar is the server-authored identity and topology admitted to local chat.
// ScannerID and MissionID identify the two retail visibility scopes. Observe
// is the only source of mutable position and faction data.
type Avatar struct {
	CharacterID uuid.UUID
	EntityID    uint64
	ZoneID      string
	ScannerID   string
	MissionID   string
	Observe     ObserveAvatar
}

// Factions resolves the baked faction table carried by the content pack.
type Factions interface {
	Faction(string) (gametypes.Faction, bool)
}

// Influence resolves the world visibility controller's directional
// canInfluence decision.
type Influence interface {
	CanInfluence(context.Context, uint64, uint64) (bool, error)
}

// IgnoreLists resolves directed social relationships. Say checks whether the
// recipient ignores the speaker, never the other way around.
type IgnoreLists interface {
	Ignores(context.Context, uuid.UUID, uuid.UUID) (bool, error)
}

// Authority owns authenticated avatar membership and applies every recovered
// Say recipient rule behind chat.SayAudience.
type Authority struct {
	mu        sync.RWMutex
	factions  Factions
	influence Influence
	ignores   IgnoreLists
	byID      map[uuid.UUID]*entry
	byEntity  map[uint64]*entry
}

type entry struct {
	avatar Avatar
}

// Session is one admitted avatar's membership. Scope updates and Close are
// ordered by Authority's mutex. Close is idempotent.
type Session struct {
	authority *Authority
	entry     *entry
}

var _ chat.SayAudience = (*Authority)(nil)

// New binds the three independent authorities needed for Say routing.
func New(factions Factions, influence Influence, ignores IgnoreLists) (*Authority, error) {
	if factions == nil || influence == nil || ignores == nil {
		return nil, ErrMissingSource
	}
	return &Authority{
		factions:  factions,
		influence: influence,
		ignores:   ignores,
		byID:      make(map[uuid.UUID]*entry),
		byEntity:  make(map[uint64]*entry),
	}, nil
}

// Admit adds one avatar after authentication and world admission complete.
func (authority *Authority) Admit(avatar Avatar) (*Session, error) {
	if authority == nil || !validAvatar(avatar) {
		return nil, ErrInvalidAvatar
	}
	observation, ok := avatar.Observe()
	if !ok || !validObservation(observation) {
		return nil, ErrInvalidAvatar
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.byID[avatar.CharacterID] != nil || authority.byEntity[avatar.EntityID] != nil {
		return nil, ErrAlreadyPresent
	}
	current := &entry{avatar: avatar}
	authority.byID[avatar.CharacterID] = current
	authority.byEntity[avatar.EntityID] = current
	return &Session{authority: authority, entry: current}, nil
}

// UpdateScope atomically publishes a scanner or mission transition. Identity,
// entity and zone changes require closing and admitting the new world entity.
func (session *Session) UpdateScope(scannerID, missionID string) error {
	if session == nil || session.authority == nil || scannerID == "" || missionID == "" {
		return ErrInvalidAvatar
	}
	authority := session.authority
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.byID[session.entry.avatar.CharacterID] != session.entry {
		return ErrInvalidAvatar
	}
	session.entry.avatar.ScannerID = scannerID
	session.entry.avatar.MissionID = missionID
	return nil
}

// Close removes this avatar. It is safe to call more than once.
func (session *Session) Close() {
	if session == nil || session.authority == nil {
		return
	}
	authority := session.authority
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.byID[session.entry.avatar.CharacterID] != session.entry {
		return
	}
	delete(authority.byID, session.entry.avatar.CharacterID)
	delete(authority.byEntity, session.entry.avatar.EntityID)
}

// Audience returns a deterministic snapshot of recipients authorized at this
// send. It does not hold the membership lock while calling world, social, or
// visibility sources.
func (authority *Authority) Audience(
	ctx context.Context,
	speaker chat.SaySpeaker,
) ([]chat.SayRecipient, error) {
	if authority == nil {
		return nil, ErrMissingSource
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, candidates, ok := authority.snapshot(speaker)
	if !ok || !speakerPositionValid(speaker.Position) {
		return nil, ErrInvalidAvatar
	}
	speakerWorld, ok := current.Observe()
	if !ok || !validObservation(speakerWorld) {
		return nil, ErrInvalidAvatar
	}
	speakerFaction, ok := authority.factions.Faction(speakerWorld.FactionID)
	if !ok || speakerFaction.ID == "" || speakerFaction.NameKey == "" {
		return nil, fmt.Errorf("%w: %q", ErrUnknownFaction, speakerWorld.FactionID)
	}

	recipients := make([]chat.SayRecipient, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ZoneID != current.ZoneID || candidate.ScannerID != current.ScannerID ||
			candidate.MissionID != current.MissionID {
			continue
		}
		worldState, present := candidate.Observe()
		if !present {
			continue
		}
		if !validObservation(worldState) {
			return nil, ErrInvalidAvatar
		}
		if distanceSquared(speaker.Position, worldState.Position) > sayRadiusSquared {
			continue
		}
		allowed, err := authority.influence.CanInfluence(ctx, speaker.EntityID, candidate.EntityID)
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}
		ignored, err := authority.ignores.Ignores(ctx, candidate.CharacterID, speaker.CharacterID)
		if err != nil {
			return nil, err
		}
		if ignored {
			continue
		}

		candidateFaction, ok := authority.factions.Faction(worldState.FactionID)
		if !ok || candidateFaction.ID == "" {
			return nil, fmt.Errorf("%w: %q", ErrUnknownFaction, worldState.FactionID)
		}
		recipient := chat.SayRecipient{CharacterID: candidate.CharacterID}
		if speakerFaction.StanceTowards(candidateFaction.ID) != gametypes.StanceFriendly {
			recipient.UnreadableFactionLocalizationID = speakerFaction.NameKey
		}
		recipients = append(recipients, recipient)
	}
	sort.Slice(recipients, func(left, right int) bool {
		return recipients[left].CharacterID.String() < recipients[right].CharacterID.String()
	})
	return recipients, nil
}

func (authority *Authority) snapshot(speaker chat.SaySpeaker) (Avatar, []Avatar, bool) {
	authority.mu.RLock()
	defer authority.mu.RUnlock()
	current := authority.byID[speaker.CharacterID]
	if current == nil || current.avatar.EntityID != speaker.EntityID || current.avatar.ZoneID != speaker.ZoneID ||
		authority.byEntity[speaker.EntityID] != current {
		return Avatar{}, nil, false
	}
	candidates := make([]Avatar, 0, len(authority.byID)-1)
	for characterID, candidate := range authority.byID {
		if characterID == speaker.CharacterID || candidate.avatar.EntityID == speaker.EntityID {
			continue
		}
		candidates = append(candidates, candidate.avatar)
	}
	return current.avatar, candidates, true
}

func validAvatar(avatar Avatar) bool {
	return avatar.CharacterID != uuid.Nil && avatar.EntityID != 0 && avatar.ZoneID != "" &&
		avatar.ScannerID != "" && avatar.MissionID != "" && avatar.Observe != nil
}

func validObservation(observation Observation) bool {
	return observation.Position.Finite() && observation.FactionID != ""
}

func speakerPositionValid(position chat.Position) bool {
	return gametypes.Vec3{X: position.X, Y: position.Y, Z: position.Z}.Finite()
}

func distanceSquared(from chat.Position, to gametypes.Vec3) float64 {
	x := float64(from.X) - float64(to.X)
	y := float64(from.Y) - float64(to.Y)
	z := float64(from.Z) - float64(to.Z)
	return x*x + y*y + z*z
}
