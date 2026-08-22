package script_test

import (
	"context"
	"errors"
	"testing"

	"github.com/SarnautCore/server/internal/script"
)

func TestImpactAddExperienceEmitsTheAuthoredMobInputsForTheReturningCaster(t *testing.T) {
	t.Parallel()

	host := newFakeHost()
	impactNode := impact("quest-1-40/manticore/experience", "ImpactAddExperience",
		field("mobCount", integer(4)),
		field("mobLevel", integer(2)),
	)
	root := impact("quest-1-40/manticore/return", "ReturningImpact",
		field("impact", node(impactNode)),
	)
	frame := newFrame()
	frame.Addressee = ratID

	if _, err := run(host, root, frame); err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(host.commands) != 1 {
		t.Fatalf("host commands = %#v", host.commands)
	}
	command := host.commands[0]
	if command.Kind != script.CommandAddExperience || command.EntityID != playerID ||
		command.Count != 4 || command.MobLevel != 2 ||
		command.ExecutionKey != "eval-1|quest-1-40/manticore/experience" {
		t.Fatalf("experience command = %#v", command)
	}
}

func TestImpactAddExperienceRefusesMalformedMobInputsBeforeCallingHost(t *testing.T) {
	t.Parallel()

	for name, fields := range map[string][]script.Field{
		"missing count": {field("mobLevel", integer(2))},
		"zero count":    {field("mobCount", integer(0)), field("mobLevel", integer(2))},
		"missing level": {field("mobCount", integer(4))},
		"text level":    {field("mobCount", integer(4)), field("mobLevel", text("two"))},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host := newFakeHost()
			impactNode := impact("bad/experience", "ImpactAddExperience", fields...)
			evaluator := script.New(host, script.Options{Enabled: true})
			err := evaluator.Evaluate(context.Background(), impactNode, newFrame())
			var refusal *script.RefusedError
			if !errors.As(err, &refusal) {
				t.Fatalf("Evaluate() error = %v, want RefusedError", err)
			}
			if len(host.commands) != 0 {
				t.Fatalf("malformed impact emitted commands: %#v", host.commands)
			}
		})
	}
}
