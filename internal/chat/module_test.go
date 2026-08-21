package chat_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SarnautCore/server/internal/chat"
)

func TestWhisperUsesAuthenticatedPresenceAndDistinguishesMissingFromOffline(t *testing.T) {
	t.Parallel()

	aliceID := uuid.MustParse("019200f0-0000-7000-8000-00000000c001")
	bobID := uuid.MustParse("019200f0-0000-7000-8000-00000000c002")
	odetteID := uuid.MustParse("019200f0-0000-7000-8000-00000000c003")
	clock := newTestClock(time.Date(2026, time.August, 21, 18, 0, 0, 0, time.UTC))
	directory := testDirectory{
		"alice":  {CharacterID: aliceID, Name: "Alice"},
		"bob":    {CharacterID: bobID, Name: "Bob"},
		"odette": {CharacterID: odetteID, Name: "Odette"},
	}
	module := chat.New(chat.Options{Clock: clock.Now, Directory: directory})

	aliceSink := newRecordingSink()
	alice := join(t, module, presence(aliceID, 101, "Alice", "zone-a", 1), aliceSink)
	defer alice.Close()
	bobSink := newRecordingSink()
	bob := join(t, module, presence(bobID, 202, "Bob", "zone-b", 2), bobSink)
	defer bob.Close()

	missing := alice.Send(t.Context(), whisper(1, "Nobody", "first"))
	assertRejected(t, missing, chat.RejectionTargetNotFound)
	offline := alice.Send(t.Context(), whisper(2, "Odette", "second"))
	assertRejected(t, offline, chat.RejectionTargetOffline)

	text := "raw  e\u0301  U0001f680"
	accepted := alice.Send(t.Context(), whisper(3, "bOb", text))
	if !accepted.Accepted {
		t.Fatalf("Send() = %+v, want accepted whisper", accepted)
	}

	if deliveries := aliceSink.all(); len(deliveries) != 0 {
		t.Fatalf("sender deliveries = %+v, want client-local echo only", deliveries)
	}
	bobDelivery := onlyDelivery(t, bobSink)
	if bobDelivery.MessageID == 0 {
		t.Fatal("remote whisper message id = 0, want server id")
	}
	if bobDelivery.WhisperPeerName != "Alice" {
		t.Errorf("recipient delivery = %+v, want non-echo and peer Alice", bobDelivery)
	}
	for _, delivery := range []chat.Delivery{bobDelivery} {
		if delivery.Channel != chat.ChannelWhisper || delivery.SenderCharacterID != aliceID ||
			delivery.SenderEntityID != 101 || delivery.SenderName != "Alice" || !delivery.SenderAlive {
			t.Errorf("server-authored sender = %+v, want authenticated Alice", delivery)
		}
		if delivery.Body.Kind != chat.BodyUserText || delivery.Body.UserText != text {
			t.Errorf("body = %+v, want byte-for-codepoint preserved user text %q", delivery.Body, text)
		}
	}
}

func TestTextUsesUTF16UnitsAndOnlyAcceptedSendsConsumeTheThrottle(t *testing.T) {
	t.Parallel()

	aliceID := uuid.MustParse("019200f0-0000-7000-8000-00000000c011")
	clock := newTestClock(time.Date(2026, time.August, 21, 18, 30, 0, 0, time.UTC))
	directory := testDirectory{"alice": {CharacterID: aliceID, Name: "Alice"}}
	module := chat.New(chat.Options{Clock: clock.Now, Directory: directory})
	sink := newRecordingSink()
	session := join(t, module, presence(aliceID, 111, "Alice", "zone-a", 0), sink)
	defer session.Close()

	assertRejected(t, session.Send(t.Context(), whisper(1, "Alice", "")), chat.RejectionEmpty)
	assertRejected(t, session.Send(t.Context(), whisper(2, "Alice", strings.Repeat("a", 301))), chat.RejectionTooLong)
	assertRejected(t, session.Send(t.Context(), whisper(3, "Alice", strings.Repeat("\U0001f680", 151))), chat.RejectionTooLong)
	assertRejected(t, session.Send(t.Context(), whisper(4, "Alice", string([]byte{0xed, 0xa0, 0x80}))), chat.RejectionInternalError)
	assertRejected(t, session.Send(t.Context(), whisper(5, "Nobody", "does not consume")), chat.RejectionTargetNotFound)

	exactly300 := strings.Repeat("\U0001f680", 150)
	if result := session.Send(t.Context(), whisper(6, "Alice", exactly300)); !result.Accepted {
		t.Fatalf("300 UTF-16-unit send = %+v, want accepted", result)
	}
	limited := session.Send(t.Context(), whisper(7, "Alice", "next"))
	assertRejected(t, limited, chat.RejectionRateLimited)
	if limited.RetryAfter != time.Second {
		t.Errorf("retry_after = %s, want 1s", limited.RetryAfter)
	}

	clock.Advance(999 * time.Millisecond)
	limited = session.Send(t.Context(), whisper(8, "Alice", "next"))
	assertRejected(t, limited, chat.RejectionRateLimited)
	if limited.RetryAfter != time.Millisecond {
		t.Errorf("retry_after = %s, want 1ms", limited.RetryAfter)
	}
	clock.Advance(time.Millisecond)
	if result := session.Send(t.Context(), whisper(9, "Alice", "  \t  ")); !result.Accepted {
		t.Fatalf("whitespace-only send = %+v, want accepted without trimming", result)
	}

	deliveries := sink.all()
	if len(deliveries) != 2 || deliveries[0].Body.UserText != exactly300 || deliveries[1].Body.UserText != "  \t  " {
		t.Fatalf("self-whisper remote deliveries = %+v, want the retail self-target edge preserved", deliveries)
	}
}

func TestZoneAndPaidWorldFanoutUseExactServerScopes(t *testing.T) {
	t.Parallel()

	aliceID := uuid.MustParse("019200f0-0000-7000-8000-00000000c021")
	bobID := uuid.MustParse("019200f0-0000-7000-8000-00000000c022")
	carolID := uuid.MustParse("019200f0-0000-7000-8000-00000000c023")
	clock := newTestClock(time.Date(2026, time.August, 21, 19, 0, 0, 0, time.UTC))
	module := chat.New(chat.Options{Clock: clock.Now})
	aliceSink, bobSink, carolSink := newRecordingSink(), newRecordingSink(), newRecordingSink()
	alice := join(t, module, presence(aliceID, 121, "Alice", "zone-a", 0), aliceSink)
	defer alice.Close()
	bob := join(t, module, presence(bobID, 122, "Bob", "zone-a", 0), bobSink)
	defer bob.Close()
	carol := join(t, module, presence(carolID, 123, "Carol", "zone-b", 0), carolSink)
	defer carol.Close()

	if result := alice.Send(t.Context(), send(20, chat.ChannelZone, "zone")); !result.Accepted {
		t.Fatalf("zone Send() = %+v, want accepted", result)
	}
	if len(aliceSink.all()) != 0 || len(bobSink.all()) != 1 || len(carolSink.all()) != 0 {
		t.Fatalf("zone fanout counts = %d/%d/%d, want 0/1/0 without public sender echo",
			len(aliceSink.all()), len(bobSink.all()), len(carolSink.all()))
	}
	clock.Advance(time.Second)
	assertRejected(t, alice.Send(t.Context(), send(21, chat.ChannelWorld, "world")), chat.RejectionUnsupportedChannel)

	paid := &testCurrencySpender{allow: true}
	paidModule := chat.New(chat.Options{Clock: clock.Now, CurrencySpender: paid})
	paidAliceSink, paidBobSink, paidCarolSink := newRecordingSink(), newRecordingSink(), newRecordingSink()
	paidAlice := join(t, paidModule, presence(aliceID, 221, "Alice", "zone-a", 0), paidAliceSink)
	defer paidAlice.Close()
	paidBob := join(t, paidModule, presence(bobID, 222, "Bob", "zone-a", 0), paidBobSink)
	defer paidBob.Close()
	paidCarol := join(t, paidModule, presence(carolID, 223, "Carol", "zone-b", 0), paidCarolSink)
	defer paidCarol.Close()

	if result := paidAlice.Send(t.Context(), send(22, chat.ChannelWorld, "world")); !result.Accepted {
		t.Fatalf("authorized world Send() = %+v, want accepted", result)
	}
	if len(paidAliceSink.all()) != 0 || len(paidBobSink.all()) != 1 || len(paidCarolSink.all()) != 1 {
		t.Fatalf("world fanout counts = %d/%d/%d, want 0/1/1 without public sender echo",
			len(paidAliceSink.all()), len(paidBobSink.all()), len(paidCarolSink.all()))
	}
	if paid.calls != 1 || paid.lastCurrency.ResourceID != chat.WorldChatCurrencyResourceID ||
		paid.lastCurrency.SysName != chat.WorldChatCurrencySysName || paid.lastAmount != 1 || paid.lastCharacter != aliceID {
		t.Errorf("paid authorization = %+v, want one World debit for authenticated Alice", paid)
	}
}

func TestMembershipFanoutUsesAuthoritySnapshotsAndIdentityStaysImmutable(t *testing.T) {
	t.Parallel()

	clock := newTestClock(time.Date(2026, time.August, 21, 19, 30, 0, 0, time.UTC))
	aliceID := uuid.MustParse("019200f0-0000-7000-8000-00000000c031")
	bobID := uuid.MustParse("019200f0-0000-7000-8000-00000000c032")
	carolID := uuid.MustParse("019200f0-0000-7000-8000-00000000c033")
	daveID := uuid.MustParse("019200f0-0000-7000-8000-00000000c034")
	group := &testAudience{audience: chat.Audience{RecipientCharacterIDs: []uuid.UUID{bobID, bobID, aliceID}}}
	guild := &testGuildAudience{audience: chat.Audience{RecipientCharacterIDs: []uuid.UUID{bobID}}}
	module := chat.New(chat.Options{Clock: clock.Now, GroupAudience: group, GuildAudience: guild})

	alicePresence := presence(aliceID, 131, "Alice", "zone-a", 0)
	bobPresence := presence(bobID, 132, "Bob", "zone-a", 0)
	carolPresence := presence(carolID, 133, "Carol", "zone-a", 0)
	davePresence := presence(daveID, 134, "Dave", "zone-a", 0)

	aliceSink, bobSink := newRecordingSink(), newRecordingSink()
	carolSink, daveSink := newRecordingSink(), newRecordingSink()
	alice := join(t, module, alicePresence, aliceSink)
	defer alice.Close()
	bob := join(t, module, bobPresence, bobSink)
	defer bob.Close()
	carol := join(t, module, carolPresence, carolSink)
	defer carol.Close()
	dave := join(t, module, davePresence, daveSink)
	defer dave.Close()

	if result := alice.Send(t.Context(), send(30, chat.ChannelGroup, "group")); !result.Accepted {
		t.Fatalf("group Send() = %+v, want accepted", result)
	}
	if len(aliceSink.all()) != 0 || len(bobSink.all()) != 1 || len(carolSink.all()) != 0 || len(daveSink.all()) != 0 {
		t.Fatalf("group fanout counts = %d/%d/%d/%d, want 0/1/0/0 without server echo",
			len(aliceSink.all()), len(bobSink.all()), len(carolSink.all()), len(daveSink.all()))
	}

	group.set(chat.Audience{RecipientCharacterIDs: []uuid.UUID{carolID}})
	clock.Advance(time.Second)
	if result := alice.Send(t.Context(), send(31, chat.ChannelGroup, "updated group")); !result.Accepted {
		t.Fatalf("updated group Send() = %+v, want accepted", result)
	}
	if len(aliceSink.all()) != 0 || len(bobSink.all()) != 1 || len(carolSink.all()) != 1 {
		t.Fatalf("updated group counts = %d/%d/%d, want 0/1/1", len(aliceSink.all()), len(bobSink.all()), len(carolSink.all()))
	}

	clock.Advance(time.Second)
	if result := alice.Send(t.Context(), send(32, chat.ChannelGuildOfficer, "officers")); !result.Accepted {
		t.Fatalf("officer Send() = %+v, want accepted", result)
	}
	if len(aliceSink.all()) != 0 || len(bobSink.all()) != 2 || len(carolSink.all()) != 1 {
		t.Fatalf("officer fanout counts = %d/%d/%d, want 0/2/1 without server echo",
			len(aliceSink.all()), len(bobSink.all()), len(carolSink.all()))
	}
	guild.set(chat.Audience{Refusal: chat.AudienceNotAuthorized})
	assertRejected(t, carol.Send(t.Context(), send(33, chat.ChannelGuildOfficer, "not an officer")), chat.RejectionNotAuthorized)
	group.set(chat.Audience{Refusal: chat.AudienceNotMember})
	assertRejected(t, dave.Send(t.Context(), send(34, chat.ChannelGroup, "not grouped")), chat.RejectionNotMember)

	forged := alicePresence
	forged.CharacterID = bobID
	assertRejected(t, alice.UpdatePresence(forged), chat.RejectionNotAuthorized)
	forged = alicePresence
	forged.Name = "Mallory"
	assertRejected(t, alice.UpdatePresence(forged), chat.RejectionNotAuthorized)
	alice.Close()
	alice.Close()
	assertRejected(t, alice.Send(t.Context(), send(35, chat.ChannelZone, "closed")), chat.RejectionInternalError)
}

func TestSayFailsClosedWithoutWorldAuthorityAndUsesItsRecipientViews(t *testing.T) {
	t.Parallel()

	aliceID := uuid.MustParse("019200f0-0000-7000-8000-00000000c041")
	closedModule := chat.New(chat.Options{})
	closed := join(t, closedModule, presence(aliceID, 141, "Alice", "zone-a", 0), newRecordingSink())
	defer closed.Close()
	assertRejected(t, closed.Send(t.Context(), send(40, chat.ChannelSay, "no world authority")), chat.RejectionUnsupportedChannel)

	bobID := uuid.MustParse("019200f0-0000-7000-8000-00000000c042")
	carolID := uuid.MustParse("019200f0-0000-7000-8000-00000000c043")
	daveID := uuid.MustParse("019200f0-0000-7000-8000-00000000c044")
	say := &testSayAudience{recipients: []chat.SayRecipient{
		{CharacterID: bobID},
		{CharacterID: carolID, UnreadableFactionLocalizationID: "faction.empire.name"},
	}}
	module := chat.New(chat.Options{SayAudience: say})
	aliceSink, bobSink := newRecordingSink(), newRecordingSink()
	carolSink, daveSink := newRecordingSink(), newRecordingSink()
	alice := join(t, module, presenceAt(aliceID, 241, "Alice", "zone-a", chat.Position{}, true), aliceSink)
	defer alice.Close()
	bob := join(t, module, presenceAt(bobID, 242, "Bob", "zone-a", chat.Position{X: 6, Y: 8}, true), bobSink)
	defer bob.Close()
	carol := join(t, module, presenceAt(carolID, 243, "Carol", "zone-a", chat.Position{X: 100}, true), carolSink)
	defer carol.Close()
	dave := join(t, module, presenceAt(daveID, 244, "Dave", "zone-b", chat.Position{}, true), daveSink)
	defer dave.Close()

	if result := alice.Send(t.Context(), send(41, chat.ChannelSay, "local")); !result.Accepted {
		t.Fatalf("configured Say = %+v, want accepted", result)
	}
	if len(aliceSink.all()) != 0 || len(bobSink.all()) != 1 || len(carolSink.all()) != 1 || len(daveSink.all()) != 0 {
		t.Fatalf("Say fanout counts = %d/%d/%d/%d, want authority-selected 0/1/1/0 without public sender echo",
			len(aliceSink.all()), len(bobSink.all()), len(carolSink.all()), len(daveSink.all()))
	}
	if got := onlyDelivery(t, carolSink).Body; got.Kind != chat.BodyUnreadableFaction || got.FactionNameLocalizationID != "faction.empire.name" {
		t.Fatalf("nonfriend Say body = %+v, want unreadable faction body", got)
	}
	if say.lastSpeaker.CharacterID != aliceID || say.lastSpeaker.EntityID != 241 || say.lastSpeaker.Position != (chat.Position{}) {
		t.Fatalf("Say authority input = %+v, want authenticated Alice at current position", say.lastSpeaker)
	}

	dead := presenceAt(aliceID, 241, "Alice", "zone-a", chat.Position{}, false)
	if result := alice.UpdatePresence(dead); !result.Accepted {
		t.Fatalf("dead UpdatePresence() = %+v, want accepted state update", result)
	}
	alice.Close()
	deadAlice := join(t, module, dead, aliceSink)
	defer deadAlice.Close()
	assertRejected(t, deadAlice.Send(t.Context(), send(42, chat.ChannelSay, "dead")), chat.RejectionDead)
}

func TestConcurrentSendsAcceptAtMostOnePerInterval(t *testing.T) {
	t.Parallel()

	characterID := uuid.MustParse("019200f0-0000-7000-8000-00000000c051")
	clock := newTestClock(time.Date(2026, time.August, 21, 20, 0, 0, 0, time.UTC))
	module := chat.New(chat.Options{Clock: clock.Now})
	sink := newRecordingSink()
	session := join(t, module, presence(characterID, 151, "Alice", "zone-a", 0), sink)
	defer session.Close()

	const attempts = 64
	results := make(chan chat.Result, attempts)
	var group sync.WaitGroup
	for index := 0; index < attempts; index++ {
		group.Add(1)
		go func(requestID uint64) {
			defer group.Done()
			results <- session.Send(t.Context(), send(requestID, chat.ChannelZone, "same interval"))
		}(uint64(index + 1))
	}
	group.Wait()
	close(results)

	accepted, limited := 0, 0
	for result := range results {
		switch {
		case result.Accepted:
			accepted++
		case result.Rejection == chat.RejectionRateLimited:
			limited++
		default:
			t.Fatalf("concurrent Send() = %+v, want accepted or rate limited", result)
		}
	}
	if accepted != 1 || limited != attempts-1 || len(sink.all()) != 0 {
		t.Fatalf("accepted/limited/delivered = %d/%d/%d, want 1/%d/0 without public echo",
			accepted, limited, len(sink.all()), attempts-1)
	}
}

func TestConcurrentSendUpdateAndCloseRemainRaceFree(t *testing.T) {
	t.Parallel()

	characterID := uuid.MustParse("019200f0-0000-7000-8000-00000000c052")
	clock := newTestClock(time.Date(2026, time.August, 21, 20, 30, 0, 0, time.UTC))
	module := chat.New(chat.Options{Clock: clock.Now})
	base := presence(characterID, 152, "Alice", "zone-a", 0)
	session := join(t, module, base, newRecordingSink())

	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(3)
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < 128; index++ {
			_ = session.Send(t.Context(), send(uint64(index+1), chat.ChannelZone, "race"))
		}
	}()
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < 128; index++ {
			updated := base
			if index%2 == 0 {
				updated.ZoneID = "zone-b"
			}
			_ = session.UpdatePresence(updated)
		}
	}()
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < 128; index++ {
			session.Close()
		}
	}()
	close(start)
	group.Wait()
	session.Close()
}

func TestFanoutSnapshotsRecipientIdentityBeforeConcurrentPresenceUpdates(t *testing.T) {
	t.Parallel()

	clock := newTestClock(time.Date(2026, time.August, 21, 20, 45, 0, 0, time.UTC))
	module := chat.New(chat.Options{Clock: clock.Now})
	senderID := uuid.MustParse("019200f0-0000-7000-8000-00000000c053")
	recipientID := uuid.MustParse("019200f0-0000-7000-8000-00000000c054")
	sender := join(t, module, presence(senderID, 153, "Alice", "zone-a", 0), newRecordingSink())
	defer sender.Close()
	base := presence(recipientID, 154, "Bob", "zone-a", 0)
	recipient := join(t, module, base, newRecordingSink())
	defer recipient.Close()

	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < 256; index++ {
			clock.Advance(time.Second)
			_ = sender.Send(t.Context(), send(uint64(index+1), chat.ChannelZone, "recipient race"))
		}
	}()
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < 256; index++ {
			updated := base
			if index%2 == 0 {
				updated.ZoneID = "zone-b"
			}
			_ = recipient.UpdatePresence(updated)
		}
	}()
	close(start)
	group.Wait()
}

type testCurrencySpender struct {
	allow         bool
	calls         int
	lastCurrency  chat.AlternativeCurrency
	lastAmount    uint64
	lastCharacter uuid.UUID
}

func (paid *testCurrencySpender) Spend(_ context.Context, characterID uuid.UUID, currency chat.AlternativeCurrency, amount uint64) (bool, error) {
	paid.calls++
	paid.lastCharacter = characterID
	paid.lastCurrency = currency
	paid.lastAmount = amount
	return paid.allow, nil
}

type testAudience struct {
	mu       sync.Mutex
	audience chat.Audience
}

func (authority *testAudience) Audience(_ context.Context, _ uuid.UUID) (chat.Audience, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	result := authority.audience
	result.RecipientCharacterIDs = append([]uuid.UUID(nil), result.RecipientCharacterIDs...)
	return result, nil
}

func (authority *testAudience) set(audience chat.Audience) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.audience = audience
}

type testGuildAudience struct {
	testAudience
}

func (authority *testGuildAudience) Audience(_ context.Context, _ uuid.UUID, _ bool) (chat.Audience, error) {
	return authority.testAudience.Audience(context.Background(), uuid.Nil)
}

type testSayAudience struct {
	recipients  []chat.SayRecipient
	lastSpeaker chat.SaySpeaker
}

func (authority *testSayAudience) Audience(_ context.Context, speaker chat.SaySpeaker) ([]chat.SayRecipient, error) {
	authority.lastSpeaker = speaker
	return append([]chat.SayRecipient(nil), authority.recipients...), nil
}

type testDirectory map[string]chat.Character

func (directory testDirectory) ResolveCharacterName(_ context.Context, name string) (chat.Character, bool, error) {
	character, ok := directory[strings.ToLower(name)]
	return character, ok, nil
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(now time.Time) *testClock { return &testClock{now: now} }

func (clock *testClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *testClock) Advance(elapsed time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(elapsed)
}

type recordingSink struct {
	mu         sync.Mutex
	deliveries []chat.Delivery
}

func newRecordingSink() *recordingSink { return new(recordingSink) }

func (sink *recordingSink) OfferChat(delivery chat.Delivery) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.deliveries = append(sink.deliveries, delivery)
}

func (sink *recordingSink) all() []chat.Delivery {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]chat.Delivery(nil), sink.deliveries...)
}

func join(t *testing.T, module *chat.Module, value chat.Presence, sink chat.Sink) *chat.Session {
	t.Helper()
	session, err := module.Join(value, sink)
	if err != nil {
		t.Fatalf("Join() error = %v", err)
	}
	return session
}

func presence(characterID uuid.UUID, entityID uint64, name, zoneID string, x float32) chat.Presence {
	return presenceAt(characterID, entityID, name, zoneID, chat.Position{X: x}, true)
}

func presenceAt(
	characterID uuid.UUID,
	entityID uint64,
	name, zoneID string,
	position chat.Position,
	alive bool,
) chat.Presence {
	return chat.Presence{
		CharacterID: characterID,
		EntityID:    entityID,
		Name:        name,
		ZoneID:      zoneID,
		Observe: func() (chat.Observation, bool) {
			return chat.Observation{Position: position, Alive: alive}, true
		},
	}
}

func whisper(requestID uint64, target, text string) chat.Request {
	return chat.Request{
		RequestID: requestID,
		Channel:   chat.ChannelWhisper,
		Text:      text,
		Target:    chat.Target{Kind: chat.TargetWhisperCharacter, Value: target},
	}
}

func send(requestID uint64, channel chat.Channel, text string) chat.Request {
	return chat.Request{RequestID: requestID, Channel: channel, Text: text}
}

func assertRejected(t *testing.T, result chat.Result, want chat.Rejection) {
	t.Helper()
	if result.Accepted || result.Rejection != want {
		t.Fatalf("result = %+v, want rejection %v", result, want)
	}
}

func onlyDelivery(t *testing.T, sink *recordingSink) chat.Delivery {
	t.Helper()
	deliveries := sink.all()
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v, want exactly one", deliveries)
	}
	return deliveries[0]
}
