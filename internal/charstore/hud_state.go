package charstore

import (
	"fmt"
	"math"
	"strings"
)

// EquipmentSlot is the retail DressSlot ordinal used by the 1.1 equipment UI.
// The regular UI has twenty addressable slots. Bag is persisted separately at
// its retail ordinal, 20.
type EquipmentSlot int16

const (
	EquipmentHelm           EquipmentSlot = 0
	EquipmentArmor          EquipmentSlot = 1
	EquipmentPants          EquipmentSlot = 2
	EquipmentBoots          EquipmentSlot = 3
	EquipmentMantle         EquipmentSlot = 4
	EquipmentGloves         EquipmentSlot = 5
	EquipmentBracers        EquipmentSlot = 6
	EquipmentBelt           EquipmentSlot = 7
	EquipmentRing1          EquipmentSlot = 8
	EquipmentRing2          EquipmentSlot = 9
	EquipmentEarrings       EquipmentSlot = 10
	EquipmentNecklace       EquipmentSlot = 11
	EquipmentCloak          EquipmentSlot = 12
	EquipmentShirt          EquipmentSlot = 13
	EquipmentMainhand       EquipmentSlot = 14
	EquipmentOffhand        EquipmentSlot = 15
	EquipmentRanged         EquipmentSlot = 16
	EquipmentTabard         EquipmentSlot = 18
	EquipmentTrinket        EquipmentSlot = 19
	EquipmentBag            EquipmentSlot = 20
	EquipmentDeathInsurance EquipmentSlot = 21
)

// RegularEquipmentSlots is the closed, retail-ordered set the equipment panel
// addresses. Ammo 17 and bag 20 are not regular panel slots.
var RegularEquipmentSlots = [...]EquipmentSlot{
	EquipmentHelm,
	EquipmentArmor,
	EquipmentPants,
	EquipmentBoots,
	EquipmentMantle,
	EquipmentGloves,
	EquipmentBracers,
	EquipmentBelt,
	EquipmentRing1,
	EquipmentRing2,
	EquipmentEarrings,
	EquipmentNecklace,
	EquipmentCloak,
	EquipmentShirt,
	EquipmentMainhand,
	EquipmentOffhand,
	EquipmentRanged,
	EquipmentTabard,
	EquipmentTrinket,
	EquipmentDeathInsurance,
}

// ProductID returns SarnautCore's stable equipment name. It does not expose an
// original-data sysName.
func (slot EquipmentSlot) ProductID() (string, bool) {
	name, ok := map[EquipmentSlot]string{
		EquipmentHelm:           "helm",
		EquipmentArmor:          "armor",
		EquipmentPants:          "pants",
		EquipmentBoots:          "boots",
		EquipmentMantle:         "mantle",
		EquipmentGloves:         "gloves",
		EquipmentBracers:        "bracers",
		EquipmentBelt:           "belt",
		EquipmentRing1:          "ring-1",
		EquipmentRing2:          "ring-2",
		EquipmentEarrings:       "earrings",
		EquipmentNecklace:       "necklace",
		EquipmentCloak:          "cloak",
		EquipmentShirt:          "shirt",
		EquipmentMainhand:       "mainhand",
		EquipmentOffhand:        "offhand",
		EquipmentRanged:         "ranged",
		EquipmentTabard:         "tabard",
		EquipmentTrinket:        "trinket",
		EquipmentBag:            "bag",
		EquipmentDeathInsurance: "death-insurance",
	}[slot]
	return name, ok
}

func isRegularEquipmentSlot(slot EquipmentSlot) bool {
	for _, candidate := range RegularEquipmentSlots {
		if candidate == slot {
			return true
		}
	}
	return false
}

// ItemInstance is the mutable part of an item. Static presentation and rules
// are resolved from ItemID in the compiled catalogue.
type ItemInstance struct {
	InstanceID         uint64
	ItemID             string
	Quantity           int32
	CounterValue       int32
	Bound              bool
	Cursed             bool
	QuestOperator      bool
	RemoveTime         *int64
	RuneResourceID     *string
	RuneSlotResourceID *string
}

// EquipmentItem is one item in a regular equipment slot. The equipped bag is
// CharacterHUDState.Bag, because retail gives it its own slot and behavior.
type EquipmentItem struct {
	Slot EquipmentSlot
	ItemInstance
}

const (
	MaxBagPartitions = 5
	MaxBagCapacity   = 60
	ActionSlotCount  = 36
)

// BagPartition is one ordered, contiguous range of bag slots. Capacity is the
// range length. A slot's absolute index is the sum of preceding capacities plus
// its local index.
type BagPartition struct {
	Ordinal  int16
	Capacity int32
}

// ProductBagLayout is a native product identity and its exact ordered ranges.
// It carries no original resource name.
type ProductBagLayout struct {
	LayoutID   string
	Partitions []BagPartition
}

// Capacity returns the total number of addressable bag slots.
func (layout ProductBagLayout) Capacity() int32 {
	var total int32
	for _, partition := range layout.Partitions {
		total += partition.Capacity
	}
	return total
}

// ValidateProductBagLayout checks the repository and migration limits at the
// content-wiring boundary, so a bad product layout stops shard boot.
func ValidateProductBagLayout(layout ProductBagLayout) error {
	return validateBagLayout(layout)
}

// StatOrdinal is the retail InnateStats ordinal. Character saves hold exactly
// one row per ordinal in this order.
type StatOrdinal int16

const (
	StatStrength StatOrdinal = iota
	StatMight
	StatDexterity
	StatAgility
	StatStamina
	StatPrecision
	StatHardiness
	StatIntellect
	StatIntuition
	StatSpirit
	StatWill
	StatResolve
	StatWisdom
	StatLethality
	StatCount
)

// ProductID returns SarnautCore's stable stat name, never a source sysName.
func (ordinal StatOrdinal) ProductID() (string, bool) {
	if ordinal < 0 || ordinal >= StatCount {
		return "", false
	}
	return [...]string{
		"strength",
		"might",
		"dexterity",
		"agility",
		"stamina",
		"precision",
		"hardiness",
		"intellect",
		"intuition",
		"spirit",
		"will",
		"resolve",
		"wisdom",
		"lethality",
	}[ordinal], true
}

// StatOrdinalByProductID resolves only native product names.
func StatOrdinalByProductID(id string) (StatOrdinal, bool) {
	wanted := strings.ToLower(strings.TrimSpace(id))
	for ordinal := StatOrdinal(0); ordinal < StatCount; ordinal++ {
		if id, _ := ordinal.ProductID(); id == wanted {
			return ordinal, true
		}
	}
	return 0, false
}

// CharacterStat is one stable row. The 1.1 codec carries three independent raw
// float32 values. Nil means that mechanics has not authored or computed that
// component; the row still exists, so absence never becomes an invented zero.
type CharacterStat struct {
	Ordinal        StatOrdinal
	Base           *float32
	Result         *float32
	ResultLongTerm *float32
}

// OrderedStats is exactly the retail fourteen-row stat vector.
type OrderedStats [StatCount]CharacterStat

// EmptyOrderedStats returns all fourteen ordinals with absent authored values.
func EmptyOrderedStats() OrderedStats {
	var stats OrderedStats
	for ordinal := StatOrdinal(0); ordinal < StatCount; ordinal++ {
		stats[ordinal].Ordinal = ordinal
	}
	return stats
}

// ActionSlot is one of the HUD's thirty-six zero-based authored slots. A nil
// AbilityID means the slot is intentionally empty.
type ActionSlot struct {
	Ordinal   int16
	AbilityID *string
}

// OrderedActionSlots is exactly the retail action-bar capacity.
type OrderedActionSlots [ActionSlotCount]ActionSlot

// EmptyOrderedActionSlots returns all slot ordinals with no invented ability.
func EmptyOrderedActionSlots() OrderedActionSlots {
	var slots OrderedActionSlots
	for ordinal := range slots {
		slots[ordinal].Ordinal = int16(ordinal)
	}
	return slots
}

// CharacterHUDState is the persisted equipment, bag identity and stat vector.
// Snapshot carries it as a pointer so old checkpoint producers that do not own
// HUD mutations preserve the stored value rather than clearing it.
type CharacterHUDState struct {
	Equipment []EquipmentItem
	Bag       *ItemInstance
	BagLayout ProductBagLayout
	Stats     OrderedStats
	Actions   OrderedActionSlots
}

func validateHUDState(hud CharacterHUDState) error {
	if err := validateBagLayout(hud.BagLayout); err != nil {
		return err
	}
	seen := make(map[EquipmentSlot]struct{}, len(hud.Equipment))
	seenInstances := make(map[uint64]struct{}, len(hud.Equipment)+1)
	for _, equipped := range hud.Equipment {
		if !isRegularEquipmentSlot(equipped.Slot) {
			return fmt.Errorf("%w: equipment slot %d is not a regular retail slot", ErrConstraintViolated, equipped.Slot)
		}
		if _, duplicate := seen[equipped.Slot]; duplicate {
			return fmt.Errorf("%w: equipment slot %d occurs twice", ErrConstraintViolated, equipped.Slot)
		}
		seen[equipped.Slot] = struct{}{}
		if err := validateItemInstance(equipped.ItemInstance); err != nil {
			return fmt.Errorf("equipment slot %d: %w", equipped.Slot, err)
		}
		if _, duplicate := seenInstances[equipped.InstanceID]; duplicate {
			return fmt.Errorf("%w: equipment instance id %d occurs twice", ErrConstraintViolated, equipped.InstanceID)
		}
		seenInstances[equipped.InstanceID] = struct{}{}
	}
	if hud.Bag != nil {
		if err := validateItemInstance(*hud.Bag); err != nil {
			return fmt.Errorf("bag slot %d: %w", EquipmentBag, err)
		}
		if _, duplicate := seenInstances[hud.Bag.InstanceID]; duplicate {
			return fmt.Errorf("%w: equipment instance id %d occurs twice", ErrConstraintViolated, hud.Bag.InstanceID)
		}
	}
	for index, stat := range hud.Stats {
		if stat.Ordinal != StatOrdinal(index) {
			return fmt.Errorf("%w: stat row %d carries ordinal %d", ErrConstraintViolated, index, stat.Ordinal)
		}
		for name, value := range map[string]*float32{
			"base": stat.Base, "result": stat.Result, "result_long_term": stat.ResultLongTerm,
		} {
			if value != nil && (math.IsNaN(float64(*value)) || math.IsInf(float64(*value), 0)) {
				return fmt.Errorf("%w: stat ordinal %d has a non-finite %s", ErrConstraintViolated, stat.Ordinal, name)
			}
		}
	}
	for index, action := range hud.Actions {
		if action.Ordinal != int16(index) {
			return fmt.Errorf("%w: action row %d carries ordinal %d", ErrConstraintViolated, index, action.Ordinal)
		}
		if action.AbilityID != nil && strings.TrimSpace(*action.AbilityID) == "" {
			return fmt.Errorf("%w: action slot %d has an empty ability id", ErrConstraintViolated, action.Ordinal)
		}
	}
	return nil
}

func validateSnapshotItemIdentities(inventory []InventoryItem, hud *CharacterHUDState) error {
	seen := make(map[uint64]string, len(inventory))
	register := func(instanceID uint64, location string) error {
		if instanceID == 0 {
			return fmt.Errorf("%w: %s has item instance id 0", ErrConstraintViolated, location)
		}
		if first, duplicate := seen[instanceID]; duplicate {
			return fmt.Errorf("%w: item instance id %d occurs in both %s and %s",
				ErrConstraintViolated, instanceID, first, location)
		}
		seen[instanceID] = location
		return nil
	}
	for _, item := range inventory {
		if err := register(item.InstanceID, fmt.Sprintf("inventory slot %d", item.Slot)); err != nil {
			return err
		}
	}
	if hud == nil {
		return nil
	}
	for _, item := range hud.Equipment {
		if err := register(item.InstanceID, fmt.Sprintf("equipment slot %d", item.Slot)); err != nil {
			return err
		}
	}
	if hud.Bag != nil {
		if err := register(hud.Bag.InstanceID, fmt.Sprintf("bag slot %d", EquipmentBag)); err != nil {
			return err
		}
	}
	return nil
}

func validateBagLayout(layout ProductBagLayout) error {
	switch {
	case strings.TrimSpace(layout.LayoutID) == "":
		return fmt.Errorf("%w: bag layout has no product id", ErrConstraintViolated)
	case len(layout.Partitions) == 0:
		return fmt.Errorf("%w: bag layout has no partitions", ErrConstraintViolated)
	case len(layout.Partitions) > MaxBagPartitions:
		return fmt.Errorf("%w: bag layout has %d partitions, maximum is %d", ErrConstraintViolated, len(layout.Partitions), MaxBagPartitions)
	}
	var capacity int32
	for index, partition := range layout.Partitions {
		if partition.Ordinal != int16(index) {
			return fmt.Errorf("%w: bag partition %d carries ordinal %d", ErrConstraintViolated, index, partition.Ordinal)
		}
		if partition.Capacity <= 0 {
			return fmt.Errorf("%w: bag partition %d capacity is %d", ErrConstraintViolated, partition.Ordinal, partition.Capacity)
		}
		capacity += partition.Capacity
	}
	if capacity > MaxBagCapacity {
		return fmt.Errorf("%w: bag capacity is %d, maximum is %d", ErrConstraintViolated, capacity, MaxBagCapacity)
	}
	return nil
}

func validateItemInstance(item ItemInstance) error {
	if item.InstanceID == 0 {
		return fmt.Errorf("%w: item instance has id 0", ErrConstraintViolated)
	}
	return validateMutableItem(item)
}

func validateMutableItem(item ItemInstance) error {
	switch {
	case strings.TrimSpace(item.ItemID) == "":
		return fmt.Errorf("%w: item has no product id", ErrConstraintViolated)
	case item.Quantity <= 0:
		return errQuantity(item.Quantity)
	case item.RuneResourceID != nil && strings.TrimSpace(*item.RuneResourceID) == "":
		return fmt.Errorf("%w: rune resource id is empty", ErrConstraintViolated)
	case item.RuneSlotResourceID != nil && strings.TrimSpace(*item.RuneSlotResourceID) == "":
		return fmt.Errorf("%w: rune slot resource id is empty", ErrConstraintViolated)
	}
	return nil
}

func cloneHUDState(source CharacterHUDState) CharacterHUDState {
	cloned := source
	cloned.Equipment = make([]EquipmentItem, len(source.Equipment))
	for index, item := range source.Equipment {
		cloned.Equipment[index] = item
		cloned.Equipment[index].ItemInstance = cloneItemInstance(item.ItemInstance)
	}
	if source.Bag != nil {
		bag := cloneItemInstance(*source.Bag)
		cloned.Bag = &bag
	}
	cloned.BagLayout.Partitions = append([]BagPartition(nil), source.BagLayout.Partitions...)
	for index, stat := range source.Stats {
		cloned.Stats[index] = stat
		cloned.Stats[index].Base = cloneFloat32(stat.Base)
		cloned.Stats[index].Result = cloneFloat32(stat.Result)
		cloned.Stats[index].ResultLongTerm = cloneFloat32(stat.ResultLongTerm)
	}
	for index, action := range source.Actions {
		cloned.Actions[index] = action
		if action.AbilityID != nil {
			value := *action.AbilityID
			cloned.Actions[index].AbilityID = &value
		}
	}
	return cloned
}

func cloneItemInstance(source ItemInstance) ItemInstance {
	cloned := source
	if source.RemoveTime != nil {
		value := *source.RemoveTime
		cloned.RemoveTime = &value
	}
	if source.RuneResourceID != nil {
		value := *source.RuneResourceID
		cloned.RuneResourceID = &value
	}
	if source.RuneSlotResourceID != nil {
		value := *source.RuneSlotResourceID
		cloned.RuneSlotResourceID = &value
	}
	return cloned
}

func cloneFloat32(source *float32) *float32 {
	if source == nil {
		return nil
	}
	value := *source
	return &value
}
