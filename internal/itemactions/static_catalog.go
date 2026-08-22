package itemactions

import (
	"fmt"
	"strings"

	"github.com/SarnautCore/server/internal/inventory"
)

// StaticCatalog is the immutable runtime view of separately baked private
// item rules and native actions. It contains no source paths or raw spell refs.
type StaticCatalog struct {
	items   map[string]Definition
	actions map[string]NativeAction
}

func NewStaticCatalog(items []Definition, actions []NativeAction) (*StaticCatalog, error) {
	catalog := &StaticCatalog{
		items:   make(map[string]Definition, len(items)),
		actions: make(map[string]NativeAction, len(actions)),
	}
	for _, action := range actions {
		if err := validateNativeAction(action); err != nil {
			return nil, err
		}
		if _, duplicate := catalog.actions[action.ActionID]; duplicate {
			return nil, fmt.Errorf("item actions: duplicate native action %q", action.ActionID)
		}
		catalog.actions[action.ActionID] = action
	}
	for _, item := range items {
		if err := validateDefinition(item, catalog.actions); err != nil {
			return nil, err
		}
		if _, duplicate := catalog.items[item.ItemID]; duplicate {
			return nil, fmt.Errorf("item actions: duplicate item definition %q", item.ItemID)
		}
		catalog.items[item.ItemID] = cloneDefinition(item)
	}
	return catalog, nil
}

func (catalog *StaticCatalog) ItemDefinition(id string) (Definition, bool) {
	definition, ok := catalog.items[id]
	return cloneDefinition(definition), ok
}

func (catalog *StaticCatalog) NativeAction(id string) (NativeAction, bool) {
	action, ok := catalog.actions[id]
	return action, ok
}

func validateNativeAction(action NativeAction) error {
	switch {
	case !strings.HasPrefix(action.ActionID, "item-action.item."):
		return fmt.Errorf("item actions: action id %q is not product-native", action.ActionID)
	case action.CooldownGroupID != "" && !strings.HasPrefix(action.CooldownGroupID, "item-action-group."):
		return fmt.Errorf("item actions: cooldown group %q is not product-native", action.CooldownGroupID)
	case action.Cooldown < 0 || action.CastDuration < 0:
		return fmt.Errorf("item actions: action %q has negative cooldown", action.ActionID)
	case action.TriggersGCD && action.GlobalCooldown <= 0:
		return fmt.Errorf("item actions: action %q triggers a missing global cooldown", action.ActionID)
	case action.Target > TargetPoint:
		return fmt.Errorf("item actions: action %q has target policy %d", action.ActionID, action.Target)
	case !strings.HasPrefix(action.PlanID, "item-action-plan."):
		return fmt.Errorf("item actions: action %q has invalid native plan id %q", action.ActionID, action.PlanID)
	default:
		return validateActionResource(action)
	}
}

func validateActionResource(action NativeAction) error {
	resource := action.Resource
	switch resource.Kind {
	case ActionResourceNone:
		if resource.ItemID != "" || resource.Count != 0 {
			return fmt.Errorf("item actions: action %q has fields on a no-resource cost", action.ActionID)
		}
	case ActionResourceActiveItem:
		if resource.ItemID != "" || resource.Count <= 0 {
			return fmt.Errorf("item actions: action %q has invalid active-item cost", action.ActionID)
		}
	case ActionResourceItem:
		if !strings.HasPrefix(resource.ItemID, "item.") || resource.Count <= 0 {
			return fmt.Errorf("item actions: action %q has invalid named-item cost", action.ActionID)
		}
	default:
		return fmt.Errorf("item actions: action %q has resource kind %d", action.ActionID, resource.Kind)
	}
	return nil
}

func validateDefinition(item Definition, actions map[string]NativeAction) error {
	switch {
	case !strings.HasPrefix(item.ItemID, "item."):
		return fmt.Errorf("item actions: item id %q is not product-native", item.ItemID)
	case item.StackLimit < 1:
		return fmt.Errorf("item actions: item %q has stack limit %d", item.ItemID, item.StackLimit)
	}
	seenSlots := make(map[EquipmentSlot]struct{}, len(item.EquipSlots))
	for _, slot := range item.EquipSlots {
		if slot != EquipmentBag && !isRegularEquipmentSlot(slot) {
			return fmt.Errorf("item actions: item %q names unsupported equipment slot %d", item.ItemID, slot)
		}
		if _, duplicate := seenSlots[slot]; duplicate {
			return fmt.Errorf("item actions: item %q repeats equipment slot %d", item.ItemID, slot)
		}
		seenSlots[slot] = struct{}{}
	}
	if item.BagLayout != nil {
		if _, allowed := seenSlots[EquipmentBag]; !allowed {
			return fmt.Errorf("item actions: bag item %q does not allow the bag slot", item.ItemID)
		}
		if err := item.BagLayout.Validate(); err != nil {
			return fmt.Errorf("item actions: bag item %q: %w", item.ItemID, err)
		}
	}
	if item.Use != nil {
		action, ok := actions[item.Use.ProductActionID]
		if !ok {
			return fmt.Errorf("item actions: item %q names unknown action %q", item.ItemID, item.Use.ProductActionID)
		}
		if (action.Resource.Kind == ActionResourceActiveItem ||
			(action.Resource.Kind == ActionResourceItem && action.Resource.ItemID == item.ItemID)) &&
			action.Resource.Count > item.StackLimit {
			return fmt.Errorf("item actions: item %q action consumes %d with stack limit %d",
				item.ItemID, action.Resource.Count, item.StackLimit)
		}
		if len(item.Use.AllowedLocations) == 0 {
			return fmt.Errorf("item actions: item %q has no admitted use location", item.ItemID)
		}
		locations := make(map[LocationKind]struct{}, len(item.Use.AllowedLocations))
		for _, location := range item.Use.AllowedLocations {
			if location == LocationDeposit || location > LocationItemBag {
				return fmt.Errorf("item actions: item %q has use location %d", item.ItemID, location)
			}
			if action.Resource.Kind == ActionResourceActiveItem && location != LocationItemBag {
				return fmt.Errorf("item actions: item %q consumes the active item outside the item bag", item.ItemID)
			}
			if _, duplicate := locations[location]; duplicate {
				return fmt.Errorf("item actions: item %q repeats use location %d", item.ItemID, location)
			}
			locations[location] = struct{}{}
		}
	}
	if item.Box != nil {
		if item.Box.ConsumeKey && item.Box.KeyItemID == "" {
			return fmt.Errorf("item actions: box %q consumes an unnamed key", item.ItemID)
		}
		for _, reward := range item.Box.Rewards {
			if reward.ItemID == "" || reward.Count <= 0 {
				return fmt.Errorf("item actions: box %q has invalid reward %+v", item.ItemID, reward)
			}
		}
	}
	return nil
}

func cloneDefinition(source Definition) Definition {
	cloned := source
	cloned.EquipSlots = append([]EquipmentSlot(nil), source.EquipSlots...)
	if source.BagLayout != nil {
		layout := cloneLayout(*source.BagLayout)
		cloned.BagLayout = &layout
	}
	if source.Use != nil {
		use := *source.Use
		use.AllowedLocations = append([]LocationKind(nil), source.Use.AllowedLocations...)
		cloned.Use = &use
	}
	if source.Box != nil {
		box := *source.Box
		box.Rewards = append([]inventory.Grant(nil), source.Box.Rewards...)
		cloned.Box = &box
	}
	return cloned
}
