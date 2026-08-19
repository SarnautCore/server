package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SarnautCore/server/internal/config"
	"github.com/SarnautCore/server/internal/health"
	"github.com/SarnautCore/server/internal/infra"
	"github.com/SarnautCore/server/internal/observability"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "auth: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	settings, err := config.Load("auth")
	if err != nil {
		return err
	}
	logger := observability.NewLogger(settings.LogLevel).With("service", settings.ServiceName)

	shutdownTelemetry, err := observability.Setup(
		ctx,
		settings.OTel,
		settings.ServiceName,
		settings.BuildID,
	)
	if err != nil {
		return err
	}
	defer shutDownTelemetry(logger, shutdownTelemetry)

	clients, err := infra.Open(ctx, settings)
	if err != nil {
		return err
	}
	defer func() {
		if err := clients.Close(); err != nil {
			logger.Error("close infrastructure clients", "error", err)
		}
	}()

	status := new(health.Status)
	healthErrors := make(chan error, 1)
	go func() {
		healthErrors <- health.Serve(ctx, settings.HealthAddress, status)
	}()
	status.SetReady(true)
	logger.Info("auth ready", "health_address", settings.HealthAddress)

	select {
	case <-ctx.Done():
		status.SetReady(false)
		return nil
	case err := <-healthErrors:
		return err
	}
}

func shutDownTelemetry(logger *slog.Logger, shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		logger.Error("shut down telemetry", "error", err)
	}
}
