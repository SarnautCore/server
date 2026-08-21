package cohort

import (
	"errors"
	"sync"

	"github.com/google/uuid"
)

// PresenceReader answers only whether a character currently has an admitted
// session. It deliberately exposes no session or transport object.
type PresenceReader interface {
	Connected(uuid.UUID) bool
}

// PresenceRegistry tracks the winning session for each character. An old
// release closure cannot erase a newer reconnect because it matches its entry.
type PresenceRegistry struct {
	mu       sync.RWMutex
	sessions map[uuid.UUID]*presenceSession
}

type presenceSession struct{ marker byte }

func NewPresenceRegistry() *PresenceRegistry {
	return &PresenceRegistry{sessions: make(map[uuid.UUID]*presenceSession)}
}

// Connect marks the newly admitted session as the character's current
// presence. Its release closure is idempotent and cannot erase a later
// reconnect.
func (registry *PresenceRegistry) Connect(characterID uuid.UUID) (func(), error) {
	if characterID == uuid.Nil {
		return nil, errors.New("cohort presence: character ID is required")
	}
	entry := new(presenceSession)
	registry.mu.Lock()
	registry.sessions[characterID] = entry
	registry.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			registry.mu.Lock()
			if current, found := registry.sessions[characterID]; found && current == entry {
				delete(registry.sessions, characterID)
			}
			registry.mu.Unlock()
		})
	}, nil
}

func (registry *PresenceRegistry) Connected(characterID uuid.UUID) bool {
	registry.mu.RLock()
	_, found := registry.sessions[characterID]
	registry.mu.RUnlock()
	return found
}

var _ PresenceReader = (*PresenceRegistry)(nil)
