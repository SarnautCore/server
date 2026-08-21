package cohort

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// Repository keeps membership after sessions disconnect and across authority
// reconstruction. Replace operations are atomic.
type Repository interface {
	GuildByMember(context.Context, uuid.UUID) (GuildRoster, error)
	RaidByMember(context.Context, uuid.UUID) (RaidRoster, error)
	ReplaceGuild(context.Context, GuildRoster) error
	ReplaceRaid(context.Context, RaidRoster) error
}

// MemoryRepository is the deterministic in-process implementation used by
// tests and single-process development shards.
type MemoryRepository struct {
	mu sync.RWMutex

	guilds        map[uuid.UUID]GuildRoster
	guildByMember map[uuid.UUID]uuid.UUID
	raids         map[uuid.UUID]RaidRoster
	raidByMember  map[uuid.UUID]uuid.UUID
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		guilds:        make(map[uuid.UUID]GuildRoster),
		guildByMember: make(map[uuid.UUID]uuid.UUID),
		raids:         make(map[uuid.UUID]RaidRoster),
		raidByMember:  make(map[uuid.UUID]uuid.UUID),
	}
}

func (repository *MemoryRepository) GuildByMember(ctx context.Context, characterID uuid.UUID) (GuildRoster, error) {
	if err := ctx.Err(); err != nil {
		return GuildRoster{}, err
	}
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	guildID, found := repository.guildByMember[characterID]
	if !found {
		return GuildRoster{}, ErrNotFound
	}
	return cloneGuild(repository.guilds[guildID]), nil
}

func (repository *MemoryRepository) RaidByMember(ctx context.Context, characterID uuid.UUID) (RaidRoster, error) {
	if err := ctx.Err(); err != nil {
		return RaidRoster{}, err
	}
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	raidID, found := repository.raidByMember[characterID]
	if !found {
		return RaidRoster{}, ErrNotFound
	}
	return cloneRaid(repository.raids[raidID]), nil
}

func (repository *MemoryRepository) ReplaceGuild(ctx context.Context, roster GuildRoster) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateGuild(roster); err != nil {
		return err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()

	repository.removeGuildLocked(roster.GuildID)
	for _, member := range roster.Members {
		if previous, found := repository.guildByMember[member.CharacterID]; found {
			repository.removeGuildMemberLocked(previous, member.CharacterID)
		}
	}
	if len(roster.Members) == 0 {
		return nil
	}
	repository.guilds[roster.GuildID] = cloneGuild(roster)
	for _, member := range roster.Members {
		repository.guildByMember[member.CharacterID] = roster.GuildID
	}
	return nil
}

func (repository *MemoryRepository) ReplaceRaid(ctx context.Context, roster RaidRoster) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateRaid(roster); err != nil {
		return err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()

	repository.removeRaidLocked(roster.RaidID)
	for _, memberID := range roster.Members {
		if previous, found := repository.raidByMember[memberID]; found {
			repository.removeRaidMemberLocked(previous, memberID)
		}
	}
	if len(roster.Members) == 0 {
		return nil
	}
	repository.raids[roster.RaidID] = cloneRaid(roster)
	for _, memberID := range roster.Members {
		repository.raidByMember[memberID] = roster.RaidID
	}
	return nil
}

func (repository *MemoryRepository) removeGuildLocked(guildID uuid.UUID) {
	for _, member := range repository.guilds[guildID].Members {
		delete(repository.guildByMember, member.CharacterID)
	}
	delete(repository.guilds, guildID)
}

func (repository *MemoryRepository) removeGuildMemberLocked(guildID, characterID uuid.UUID) {
	roster, found := repository.guilds[guildID]
	if !found {
		return
	}
	filtered := roster.Members[:0]
	for _, member := range roster.Members {
		if member.CharacterID != characterID {
			filtered = append(filtered, member)
		}
	}
	delete(repository.guildByMember, characterID)
	if len(filtered) == 0 {
		delete(repository.guilds, guildID)
		return
	}
	roster.Members = append([]GuildMember(nil), filtered...)
	repository.guilds[guildID] = roster
}

func (repository *MemoryRepository) removeRaidLocked(raidID uuid.UUID) {
	for _, memberID := range repository.raids[raidID].Members {
		delete(repository.raidByMember, memberID)
	}
	delete(repository.raids, raidID)
}

func (repository *MemoryRepository) removeRaidMemberLocked(raidID, characterID uuid.UUID) {
	roster, found := repository.raids[raidID]
	if !found {
		return
	}
	filtered := roster.Members[:0]
	for _, memberID := range roster.Members {
		if memberID != characterID {
			filtered = append(filtered, memberID)
		}
	}
	delete(repository.raidByMember, characterID)
	if len(filtered) == 0 {
		delete(repository.raids, raidID)
		return
	}
	roster.Members = append([]uuid.UUID(nil), filtered...)
	repository.raids[raidID] = roster
}

var _ Repository = (*MemoryRepository)(nil)
