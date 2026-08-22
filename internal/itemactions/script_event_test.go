package itemactions_test

import (
	"testing"

	"github.com/SarnautCore/server/internal/itemactions"
)

type scriptDriver struct {
	entityID uint64
	slot     string
	equipped bool
	calls    int
}

func (driver *scriptDriver) EquipChanged(entityID uint64, slot string, equipped bool) {
	driver.entityID, driver.slot, driver.equipped = entityID, slot, equipped
	driver.calls++
}

func TestScriptEquipEventSinkUsesCommittedActorAndTriggerSlot(t *testing.T) {
	driver := &scriptDriver{}
	sink, err := itemactions.NewScriptEquipEventSink(driver)
	if err != nil {
		t.Fatal(err)
	}
	sink.EnqueueEquipChanged(itemactions.EquipChanged{
		RequestID: 9, EntityID: 71, Slot: itemactions.EquipmentMainhand,
		TriggerSlot: "MAINHAND", Equipped: true,
	})
	sink.EnqueueEquipChanged(itemactions.EquipChanged{
		RequestID: 9, EntityID: 71, Slot: itemactions.EquipmentMainhand,
		TriggerSlot: "MAINHAND", Equipped: true,
	})
	if driver.calls != 1 || driver.entityID != 71 || driver.slot != "MAINHAND" || !driver.equipped {
		t.Fatalf("script driver = %+v", driver)
	}
}
