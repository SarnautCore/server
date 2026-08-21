package auth

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/auth/secret"
	"github.com/SarnautCore/server/internal/charstore"
)

const testHolder = "shard-1"

func createdCharacter(t *testing.T, service *Service, accountID uuid.UUID, name string) charstore.Character {
	t.Helper()
	character, err := service.CreateCharacter(t.Context(), accountID, name, "chargen.league.warrior")
	if err != nil {
		t.Fatalf("CreateCharacter() error = %v", err)
	}
	return character
}

func TestTicketMintToRedeemRoundTrip(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	accountID, _ := registerAndLogin(t, service)
	character := createdCharacter(t, service, accountID, "Anne")

	ticket, err := service.MintTicket(t.Context(), accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	if !strings.HasPrefix(ticket.Token.Reveal(), TicketPrefix) {
		t.Errorf("ticket %q does not carry the ADR 0030 prefix", ticket.Token.ID())
	}

	admission, err := service.RedeemTicket(t.Context(), ticket.Token, testHolder)
	if err != nil {
		t.Fatalf("RedeemTicket() error = %v", err)
	}
	if admission.AccountID != accountID || admission.CharacterID != character.CharacterID {
		t.Errorf("RedeemTicket() = %+v, want the minted identity", admission)
	}
	if admission.CharacterName != "Anne" || admission.ChargenOptionID != "chargen.league.warrior" {
		t.Errorf("RedeemTicket() = %+v; the shard learns the name and option here", admission)
	}
}

// Single use is the property that makes a leaked ticket worth almost nothing.
func TestARedeemedTicketCannotBeReplayed(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	accountID, _ := registerAndLogin(t, service)
	character := createdCharacter(t, service, accountID, "Anne")

	ticket, err := service.MintTicket(t.Context(), accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	if _, err := service.RedeemTicket(t.Context(), ticket.Token, testHolder); err != nil {
		t.Fatalf("RedeemTicket() error = %v", err)
	}
	if _, err := service.RedeemTicket(t.Context(), ticket.Token, testHolder); !errors.Is(err, ErrTicketUnknown) {
		t.Errorf("RedeemTicket() replay error = %v, want ErrTicketUnknown", err)
	}
}

func TestAnExpiredTicketIsRefused(t *testing.T) {
	t.Parallel()

	service, clock := newTestService(t)
	accountID, _ := registerAndLogin(t, service)
	character := createdCharacter(t, service, accountID, "Anne")

	ticket, err := service.MintTicket(t.Context(), accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	clock.advance(TicketLifetime + time.Second)
	if _, err := service.RedeemTicket(t.Context(), ticket.Token, testHolder); !errors.Is(err, ErrTicketUnknown) {
		t.Errorf("RedeemTicket() after expiry error = %v, want ErrTicketUnknown", err)
	}
}

func TestAForgedTicketIsRefusedBeforeItReachesTheStore(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	accountID, _ := registerAndLogin(t, service)
	character := createdCharacter(t, service, accountID, "Anne")
	real, err := service.MintTicket(t.Context(), accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}

	malformed := map[string]string{
		"empty":            "",
		"no prefix":        real.Token.Reveal()[len(TicketPrefix):],
		"session prefix":   SessionTokenPrefix + real.Token.Reveal()[len(TicketPrefix):],
		"not base64":       TicketPrefix + "!!!!!!!!",
		"wrong length":     TicketPrefix + "c2hvcnQ",
		"a plausible word": "sarnaut_tk_letmein",
	}
	for name, candidate := range malformed {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := service.RedeemTicket(t.Context(), secret.New(candidate), testHolder); !errors.Is(err, ErrMalformedToken) {
				t.Errorf("RedeemTicket() error = %v, want ErrMalformedToken", err)
			}
		})
	}

	// A well-formed ticket that was never minted is unknown rather than
	// malformed, and is equally refused.
	unminted, err := mintToken(TicketPrefix)
	if err != nil {
		t.Fatalf("mintToken() error = %v", err)
	}
	if _, err := service.RedeemTicket(t.Context(), unminted, testHolder); !errors.Is(err, ErrTicketUnknown) {
		t.Errorf("RedeemTicket(unminted) error = %v, want ErrTicketUnknown", err)
	}
}

// Ownership is checked where it can be checked: by the only process holding
// credentials for auth.characters. A ticket for somebody else's character is
// never minted, which is why the shard has no such path to get wrong.
func TestMintingRefusesAnotherAccountsCharacter(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	accountA, _ := registerAndLogin(t, service)
	accountB, err := service.Register(t.Context(), secret.New("b@example.invalid"), secret.New(testPassword))
	if err != nil {
		t.Fatalf("Register(B) error = %v", err)
	}
	character := createdCharacter(t, service, accountA, "Anne")

	if _, err := service.MintTicket(t.Context(), accountB, character.CharacterID); !errors.Is(err, ErrCharacterNotFound) {
		t.Errorf("MintTicket(other account) error = %v, want ErrCharacterNotFound", err)
	}
	// An id that never existed is the same answer, so a stranger cannot probe
	// for character ids.
	if _, err := service.MintTicket(t.Context(), accountB, uuid.New()); !errors.Is(err, ErrCharacterNotFound) {
		t.Errorf("MintTicket(unknown id) error = %v, want ErrCharacterNotFound", err)
	}
}

// A character deleted under a live ticket is refused at redemption: the ticket
// outlives the roster change by up to a minute, and admitting it would spawn a
// character that no longer exists.
func TestRedemptionRefusesATicketWhoseCharacterWasDeleted(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	accountID, _ := registerAndLogin(t, service)
	character := createdCharacter(t, service, accountID, "Anne")

	ticket, err := service.MintTicket(t.Context(), accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	if err := service.DeleteCharacter(t.Context(), accountID, character.CharacterID); err != nil {
		t.Fatalf("DeleteCharacter() error = %v", err)
	}
	if _, err := service.RedeemTicket(t.Context(), ticket.Token, testHolder); !errors.Is(err, ErrCharacterNotFound) {
		t.Errorf("RedeemTicket() for a deleted character error = %v, want ErrCharacterNotFound", err)
	}
}

// Ownership is re-checked at redemption even though minting already enforces
// it. This is the defence-in-depth case: a ticket record that names account A
// and a character belonging to account B — which minting cannot produce, and
// which a compromised or corrupted store could.
func TestRedemptionRefusesARecordNamingAnotherAccountsCharacter(t *testing.T) {
	t.Parallel()

	options, _ := testOptions(t)
	keys := options.Keys
	service, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	accountA, _ := registerAndLogin(t, service)
	accountB, err := service.Register(t.Context(), secret.New("b@example.invalid"), secret.New(testPassword))
	if err != nil {
		t.Fatalf("Register(B) error = %v", err)
	}
	// The character belongs to B; the forged ticket claims it for A.
	character := createdCharacter(t, service, accountB, "Bea")

	forged, err := mintToken(TicketPrefix)
	if err != nil {
		t.Fatalf("mintToken() error = %v", err)
	}
	key, err := tokenKey(ticketKeyPrefix, TicketPrefix, forged)
	if err != nil {
		t.Fatalf("tokenKey() error = %v", err)
	}
	record, err := json.Marshal(ticketRecord{
		AccountID:       accountA,
		CharacterID:     character.CharacterID,
		CharacterName:   character.Name,
		ChargenOptionID: character.ChargenOptionID,
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := keys.Set(t.Context(), key, record, TicketLifetime); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	if _, err := service.RedeemTicket(t.Context(), forged, testHolder); !errors.Is(err, ErrNotOwned) {
		t.Errorf("RedeemTicket() error = %v, want ErrNotOwned", err)
	}
}

// One live session per character, arbitrated in Valkey. A second shard is
// refused; the same shard reconnecting is not, because it already holds the
// lock and its own registry evicts the older connection.
func TestThePlayLockRefusesASecondHolderAndAdmitsTheSameOne(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t)
	accountID, _ := registerAndLogin(t, service)
	character := createdCharacter(t, service, accountID, "Anne")

	first, err := service.MintTicket(t.Context(), accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	if _, err := service.RedeemTicket(t.Context(), first.Token, testHolder); err != nil {
		t.Fatalf("RedeemTicket() error = %v", err)
	}

	second, err := service.MintTicket(t.Context(), accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	if _, err := service.RedeemTicket(t.Context(), second.Token, "shard-2"); !errors.Is(err, ErrPlayLockHeld) {
		t.Errorf("RedeemTicket(other shard) error = %v, want ErrPlayLockHeld", err)
	}

	third, err := service.MintTicket(t.Context(), accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	if _, err := service.RedeemTicket(t.Context(), third.Token, testHolder); err != nil {
		t.Errorf("RedeemTicket(same shard) error = %v, want the reconnect to be admitted", err)
	}
}

func TestThePlayLockIsRenewedAndReleasedByItsHolderOnly(t *testing.T) {
	t.Parallel()

	service, clock := newTestService(t)
	accountID, _ := registerAndLogin(t, service)
	character := createdCharacter(t, service, accountID, "Anne")
	ticket, err := service.MintTicket(t.Context(), accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	if _, err := service.RedeemTicket(t.Context(), ticket.Token, testHolder); err != nil {
		t.Fatalf("RedeemTicket() error = %v", err)
	}

	granted, err := service.RenewPlayLock(t.Context(), character.CharacterID, "shard-2")
	if err != nil || granted {
		t.Errorf("RenewPlayLock(other holder) = %v, %v; a shard that lost the lock must not renew it back", granted, err)
	}
	granted, err = service.RenewPlayLock(t.Context(), character.CharacterID, testHolder)
	if err != nil || !granted {
		t.Errorf("RenewPlayLock(holder) = %v, %v; want granted", granted, err)
	}

	// A release from a stale holder is a no-op rather than a theft.
	if err := service.ReleasePlayLock(t.Context(), character.CharacterID, "shard-2"); err != nil {
		t.Fatalf("ReleasePlayLock(other holder) error = %v", err)
	}
	if granted, _ := service.RenewPlayLock(t.Context(), character.CharacterID, testHolder); !granted {
		t.Error("another holder's release dropped this holder's lock")
	}
	if err := service.ReleasePlayLock(t.Context(), character.CharacterID, testHolder); err != nil {
		t.Fatalf("ReleasePlayLock() error = %v", err)
	}
	if granted, _ := service.RenewPlayLock(t.Context(), character.CharacterID, testHolder); granted {
		t.Error("the lock survived its own holder's release")
	}

	// And the TTL is what frees a character whose shard died without releasing.
	next, err := service.MintTicket(t.Context(), accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	if _, err := service.RedeemTicket(t.Context(), next.Token, "shard-3"); err != nil {
		t.Fatalf("RedeemTicket() error = %v", err)
	}
	clock.advance(PlayLockLifetime + time.Second)
	after, err := service.MintTicket(t.Context(), accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}
	if _, err := service.RedeemTicket(t.Context(), after.Token, "shard-4"); err != nil {
		t.Errorf("RedeemTicket() after the lock expired error = %v; a dead shard must not lock a character out", err)
	}
}

// Only the digest is a key. A Valkey dump has to be useless.
func TestNoPlaintextTokenIsEverStored(t *testing.T) {
	t.Parallel()

	options, _ := testOptions(t)
	keys, ok := options.Keys.(*memoryKeyValue)
	if !ok {
		t.Fatalf("test fixture key-value store is %T", options.Keys)
	}
	service, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	accountID, session := registerAndLogin(t, service)
	character := createdCharacter(t, service, accountID, "Anne")
	ticket, err := service.MintTicket(t.Context(), accountID, character.CharacterID)
	if err != nil {
		t.Fatalf("MintTicket() error = %v", err)
	}

	secrets := []string{session.Token.Reveal(), ticket.Token.Reveal(), testPassword, testEmail}
	// Copy first and assert after: an assertion that held the store's mutex
	// while calling back into the service would deadlock.
	keys.mu.Lock()
	stored := make(map[string]string, len(keys.entries))
	for key, entry := range keys.entries {
		stored[key] = string(entry.value)
	}
	keys.mu.Unlock()

	for key, value := range stored {
		for _, plaintext := range secrets {
			if strings.Contains(key, plaintext) {
				t.Errorf("key %q carries a plaintext secret", key)
			}
			if strings.Contains(value, plaintext) {
				t.Errorf("the value at %q carries a plaintext secret", key)
			}
		}
	}
	// The digest is the key, so the token still resolves.
	if _, err := service.Authenticate(t.Context(), session.Token); err != nil {
		t.Errorf("Authenticate() error = %v", err)
	}
}
