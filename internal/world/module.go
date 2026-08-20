package world

import (
	"context"
	"log/slog"
	"sync"
)

// Module runs the shard's configured zones.
type Module struct {
	zones  []*Zone
	logger *slog.Logger
}

// New builds the world module over the zones the composition root created.
func New(logger *slog.Logger, zones ...*Zone) *Module {
	return &Module{zones: zones, logger: logger}
}

// Run starts every zone and waits for shutdown.
func (module *Module) Run(ctx context.Context) {
	var group sync.WaitGroup
	for _, zone := range module.zones {
		group.Add(1)
		go func(current *Zone) {
			defer group.Done()
			module.logger.DebugContext(ctx, "zone tick loop started", "zone_id", current.ID())
			current.Run(ctx)
		}(zone)
	}
	group.Wait()
}
