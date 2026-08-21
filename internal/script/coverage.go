package script

import "sort"

// CoverageReport is a static walk of compiled trees. It complements the live
// census: the census proves what a scenario reached, while this report proves
// that every node declared implemented has an executable interpreter path.
type CoverageReport struct {
	Implemented int
	Inert       int
	Refused     int
	Gaps        []CoverageGap
}

// CoverageGap is an implemented-tier node for which this build has no handler.
type CoverageGap struct {
	Key    string
	Family Family
	Opcode string
}

// AuditCoverage walks every nested field of every supplied root. Gaps are
// sorted by stable node key, making the result suitable for a strict build
// report without evaluating presentation or world mutations.
func AuditCoverage(roots ...*Node) CoverageReport {
	var report CoverageReport
	var walk func(*Node)
	walk = func(node *Node) {
		if node == nil {
			return
		}
		switch node.Tier {
		case TierImplemented:
			report.Implemented++
			if !implementedOpcode(node.Opcode) {
				report.Gaps = append(report.Gaps, CoverageGap{
					Key: node.Key, Family: node.Family, Opcode: node.Opcode,
				})
			}
		case TierInert:
			report.Inert++
		default:
			report.Refused++
		}
		for _, field := range node.Fields {
			walkValue(field.Value, walk)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	sort.Slice(report.Gaps, func(i, j int) bool {
		if report.Gaps[i].Key != report.Gaps[j].Key {
			return report.Gaps[i].Key < report.Gaps[j].Key
		}
		return report.Gaps[i].Opcode < report.Gaps[j].Opcode
	})
	return report
}

func walkValue(value Value, walk func(*Node)) {
	switch value.Kind {
	case ValueNode:
		walk(value.Node)
	case ValueList:
		for _, entry := range value.List {
			walkValue(entry, walk)
		}
	}
}

func implementedOpcode(opcode string) bool {
	if _, ok := m3Handlers()[opcode]; ok {
		return true
	}
	switch opcode {
	case "TriggerResource",
		"PredicateAnd", "PredicateOr", "PredicateNot", "PredicateCharacterClass",
		"PredicateCharacterRace", "PredicateIsAvatar",
		"Switch", "EffectTrigger", "HealthTrigger", "EquipTrigger", "CombatStateTrigger",
		"Guard", "ScalerAllInputDamage", "ScalerAllOutputDamage",
		"AddresseeFinderCaster", "AddresseeFinderSelf", "AddresseeFinderTarget",
		"AddresseeFinderSingleMob",
		"FullHealthCalcer", "FloatZero", "FloatData",
		"TrivialScaler", "LinearEffectScaler", "PhysicalScaler",
		"PhysicalRangedScaler", "WeaponSpeedScaler":
		return true
	default:
		return false
	}
}
