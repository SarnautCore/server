package gametypes

import "time"

const (
	StanceHostile  = "hostile"
	StanceNeutral  = "neutral"
	StanceFriendly = "friendly"
)

type AbilityEffect struct {
	Kind             string
	Element          string
	Amount           float64
	AttackPowerCoeff float64
}

type Ability struct {
	ID          string
	NameKey     string
	Target      string
	RangeM      float32
	CastTime    time.Duration
	Cooldown    time.Duration
	TriggersGCD bool
	Effects     []AbilityEffect
}

type FactionRelation struct {
	FactionID string
	Stance    string
}

type Faction struct {
	ID            string
	NameKey       string
	PlayerFaction bool
	Attackable    bool
	DefaultStance string
	Relations     []FactionRelation
}

func (faction Faction) StanceTowards(other string) string {
	for _, relation := range faction.Relations {
		if relation.FactionID == other {
			return relation.Stance
		}
	}
	return faction.DefaultStance
}

type Mob struct {
	ID           string
	NameKey      string
	FactionID    string
	MobKindID    string
	LevelMin     uint32
	LevelMax     uint32
	WalkSpeed    float32
	HPMod        float64
	AggroRadiusM float32
	LeashRadiusM float32
	AbilityIDs   []string
	LootTableID  string
}

type NPCSpawn struct {
	PlacementID string
	MobID       string
	Position    Vec3
	Heading     float32
	RespawnMin  time.Duration
	RespawnMax  time.Duration
}
