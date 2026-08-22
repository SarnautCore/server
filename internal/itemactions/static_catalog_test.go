package itemactions_test

import (
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/itemactions"
)

func TestStaticCatalogJoinsItemRulesToNativeActionsAndClonesRows(t *testing.T) {
	const actionID = "item-action.item.fixture-tonic"
	catalog, err := itemactions.NewStaticCatalog([]itemactions.Definition{{
		ItemID: "item.fixture-tonic", StackLimit: 20, Droppable: true,
		Use: &itemactions.UseDefinition{
			ProductActionID:  actionID,
			AllowedLocations: []itemactions.LocationKind{itemactions.LocationItemBag},
		},
	}}, []itemactions.NativeAction{{
		ActionID: actionID, CooldownGroupID: "item-action-group.fixture-healing",
		Cooldown: time.Minute, Target: itemactions.TargetSelf,
		Resource: itemactions.ActionResource{Kind: itemactions.ActionResourceActiveItem, Count: 1},
		PlanID:   "item-action-plan.fixture-tonic",
	}})
	if err != nil {
		t.Fatalf("NewStaticCatalog() error = %v", err)
	}
	definition, ok := catalog.ItemDefinition("item.fixture-tonic")
	if !ok || definition.Use.ProductActionID != actionID {
		t.Fatalf("ItemDefinition() = %+v, %v", definition, ok)
	}
	definition.Use.AllowedLocations[0] = itemactions.LocationDress
	again, _ := catalog.ItemDefinition("item.fixture-tonic")
	if again.Use.AllowedLocations[0] != itemactions.LocationItemBag {
		t.Fatal("ItemDefinition returned catalog-owned mutable memory")
	}
	action, ok := catalog.NativeAction(actionID)
	if !ok || action.Resource.Kind != itemactions.ActionResourceActiveItem || action.Resource.Count != 1 {
		t.Fatalf("NativeAction() = %+v, %v", action, ok)
	}
}

func TestStaticCatalogRejectsIncompleteOrSourceShapedRows(t *testing.T) {
	validAction := itemactions.NativeAction{
		ActionID: "item-action.item.fixture", PlanID: "item-action-plan.fixture",
	}
	for _, test := range []struct {
		name    string
		items   []itemactions.Definition
		actions []itemactions.NativeAction
	}{
		{name: "non-product action id", actions: []itemactions.NativeAction{{ActionID: "legacy.action", PlanID: "item-action-plan.fixture"}}},
		{name: "missing plan", actions: []itemactions.NativeAction{{ActionID: validAction.ActionID}}},
		{name: "bad active cost", actions: []itemactions.NativeAction{{
			ActionID: validAction.ActionID, PlanID: validAction.PlanID,
			Resource: itemactions.ActionResource{Kind: itemactions.ActionResourceActiveItem},
		}}},
		{name: "unknown item action", actions: []itemactions.NativeAction{validAction}, items: []itemactions.Definition{{
			ItemID: "item.fixture", StackLimit: 1,
			Use: &itemactions.UseDefinition{ProductActionID: "item-action.item.missing", AllowedLocations: []itemactions.LocationKind{itemactions.LocationItemBag}},
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := itemactions.NewStaticCatalog(test.items, test.actions); err == nil {
				t.Fatal("NewStaticCatalog() succeeded")
			}
		})
	}
}
