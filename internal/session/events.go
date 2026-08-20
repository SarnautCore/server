package session

import (
	"context"
	"fmt"

	"github.com/SarnautCore/server/internal/combat"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// eventQueueDepth bounds one session's combat-event backlog.
//
// Combat events are not latest-wins the way snapshots are: a death happens
// once and there is no later frame that carries it again. The queue is
// therefore deep enough to absorb a burst rather than one deep, and an
// overflow is recorded on the session span instead of being silent.
const eventQueueDepth = 64

// eventSender writes combat events to one session's reliable stream.
//
// It exists so that combat's fan-out goroutine never blocks on a slow peer:
// one session that has stopped reading must not hold up the events of every
// other session in the zone.
type eventSender struct {
	writer *reliableWriter
	span   trace.Span
	queue  chan combat.Event
}

func newEventSender(writer *reliableWriter, span trace.Span) *eventSender {
	return &eventSender{
		writer: writer,
		span:   span,
		queue:  make(chan combat.Event, eventQueueDepth),
	}
}

// OfferCombatEvent queues one event. It is called from combat's fan-out
// goroutine and must not block, so a full queue has to lose something.
//
// It loses the same thing the module's own queue does: an ability event, never
// a death. A death displaces the oldest queued event instead, because there is
// no later frame that carries it again.
func (sender *eventSender) OfferCombatEvent(event combat.Event) {
	select {
	case sender.queue <- event:
		return
	default:
	}
	if event.Kind == combat.EventKindDeath {
		select {
		case displaced := <-sender.queue:
			sender.recordDrop(displaced)
		default:
		}
	}
	select {
	case sender.queue <- event:
	default:
		sender.recordDrop(event)
	}
}

func (sender *eventSender) recordDrop(event combat.Event) {
	sender.span.AddEvent("session.combat_event.dropped", trace.WithAttributes(
		attribute.Int("sarnaut.combat.event_kind", int(event.Kind)),
	))
}

func (sender *eventSender) run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-sender.queue:
			message, err := combatEventToProto(event)
			if err != nil {
				// A domain event with no wire projection is a mapping bug, not
				// a peer problem. It is recorded and skipped rather than
				// killing a session that did nothing wrong.
				sender.span.RecordError(err)
				continue
			}
			if err := sender.writer.write(message); err != nil {
				return fmt.Errorf("write combat event: %w", err)
			}
		}
	}
}
