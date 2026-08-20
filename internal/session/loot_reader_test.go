package session

import (
	"testing"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/transport"
)

// The harness stands a zone up with combat and no loot module, which is exactly
// the composition these two tests need: a zone that cannot serve the verb, and
// a carrier that may not carry it.

// TestLootTakeInAZoneWithNoLootModuleIsRefused. A verb the shard understands
// but cannot serve here is a typed error and not silence, because a client that
// gets no answer to a take has no way to tell it from a lost frame.
func TestLootTakeInAZoneWithNoLootModuleIsRefused(t *testing.T) {
	t.Parallel()
	harness := startSession(t, false)

	harness.writeReliable(t, &sarnautv1.ClientMessage{
		ClientSeq: 1,
		Payload: &sarnautv1.ClientMessage_LootTake{
			LootTake: &sarnautv1.LootTake{CorpseEntityId: 7},
		},
	})

	failure := harness.readReliable(t).GetError()
	if failure == nil || failure.GetCode() != sarnautv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE {
		t.Fatalf("server error = %v, want UNSUPPORTED_MESSAGE", failure)
	}
	if err := harness.wait(t); err == nil {
		t.Fatal("handle() error = nil, want a protocol violation")
	}
}

// TestLootTakeOnTheUnreliableChannelIsRefused keeps looting off datagrams.
//
// The take answers with what was committed to storage, and an answer that may
// be dropped is not an answer. The eligibility check runs before the module
// check, so this is refused for the carrier and not for the missing module.
func TestLootTakeOnTheUnreliableChannelIsRefused(t *testing.T) {
	t.Parallel()
	harness := startSession(t, true)

	payload, err := transport.MarshalUnreliable(&sarnautv1.ClientMessage{
		ClientSeq: 1,
		Payload: &sarnautv1.ClientMessage_LootTake{
			LootTake: &sarnautv1.LootTake{CorpseEntityId: 7},
		},
	})
	if err != nil {
		t.Fatalf("MarshalUnreliable() error = %v", err)
	}
	if err := harness.connection.SendUnreliable(payload); err != nil {
		t.Fatalf("SendUnreliable() error = %v", err)
	}

	failure := harness.readReliable(t).GetError()
	if failure == nil || failure.GetCode() != sarnautv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE {
		t.Fatalf("server error = %v, want UNSUPPORTED_MESSAGE", failure)
	}
	if detail := failure.GetDetail(); detail == "" {
		t.Error("the refusal carries no detail; a peer cannot tell which rule it broke")
	}
	if err := harness.wait(t); err == nil {
		t.Fatal("handle() error = nil, want a protocol violation")
	}
}

// TestInteractInAZoneWithNoLootModuleIsIgnored. `Interact` is the generic verb
// quest starters will also use, so a zone that cannot answer it must leave the
// session running rather than refusing a frame it understood.
func TestInteractInAZoneWithNoLootModuleIsIgnored(t *testing.T) {
	t.Parallel()
	harness := startSession(t, false)

	harness.writeReliable(t, &sarnautv1.ClientMessage{
		ClientSeq: 1,
		Payload: &sarnautv1.ClientMessage_Interact{
			Interact: &sarnautv1.Interact{TargetEntityId: 7},
		},
	})

	// The move path is untouched by the verb that just went past it, which is
	// how this test tells "ignored" from "the reader stopped".
	harness.sendMoveIntent(t, 1)
	harness.waitForAdvance(t)

	harness.logout(t)
	if err := harness.wait(t); err != nil {
		t.Fatalf("handle() error = %v, want nil after logout", err)
	}
}
