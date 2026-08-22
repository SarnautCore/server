package itemactions

import (
	"errors"
	"sync"
)

// EquipScriptDriver is satisfied by session.ScriptDriver. Keeping the small
// interface here avoids an itemactions-to-session dependency cycle.
type EquipScriptDriver interface {
	EquipChanged(entityID uint64, slot string, equipped bool)
}

// ScriptEquipEventSink delivers a committed equipment change through the
// interpreter's existing EventEquipChanged entry point.
type ScriptEquipEventSink struct {
	mu        sync.Mutex
	driver    EquipScriptDriver
	delivered map[equipEventKey]struct{}
}

type equipEventKey struct {
	requestID uint64
	slot      EquipmentSlot
	equipped  bool
}

func NewScriptEquipEventSink(driver EquipScriptDriver) (*ScriptEquipEventSink, error) {
	if driver == nil {
		return nil, errors.New("item actions: an equip script driver is required")
	}
	return &ScriptEquipEventSink{driver: driver, delivered: make(map[equipEventKey]struct{})}, nil
}

func (sink *ScriptEquipEventSink) EnqueueEquipChanged(event EquipChanged) {
	key := equipEventKey{requestID: event.RequestID, slot: event.Slot, equipped: event.Equipped}
	sink.mu.Lock()
	if _, duplicate := sink.delivered[key]; duplicate {
		sink.mu.Unlock()
		return
	}
	sink.delivered[key] = struct{}{}
	sink.mu.Unlock()
	sink.driver.EquipChanged(event.EntityID, event.TriggerSlot, event.Equipped)
}
