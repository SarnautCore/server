// Package world contains the shard world module skeleton.
package world

import (
	"context"
	"log/slog"
	"time"
)

// Module owns the world tick loop.
type Module struct {
	interval time.Duration
	logger   *slog.Logger
}

func New(interval time.Duration, logger *slog.Logger) *Module {
	return &Module{interval: interval, logger: logger}
}

// Run ticks until the process context ends.
func (module *Module) Run(ctx context.Context) {
	ticker := time.NewTicker(module.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			module.logger.DebugContext(ctx, "world tick")
		}
	}
}
