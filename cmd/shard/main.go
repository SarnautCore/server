package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/config"
	"github.com/SarnautCore/server/internal/health"
	"github.com/SarnautCore/server/internal/infra"
	"github.com/SarnautCore/server/internal/observability"
	"github.com/SarnautCore/server/internal/session"
	"github.com/SarnautCore/server/internal/transport"
	"github.com/SarnautCore/server/internal/world"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "shard: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	settings, err := config.Load("shard")
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

	tlsConfig, err := transport.NewDevServerTLSConfig()
	if err != nil {
		return err
	}
	listener, err := transport.ListenQUIC(settings.QUIC.ListenAddress, tlsConfig)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	status := new(health.Status)
	healthErrors := startHealthServer(ctx, settings.HealthAddress, status)

	worldModule := world.New(settings.World.TickInterval, logger)
	go worldModule.Run(ctx)

	server := session.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         settings.BuildID,
	}
	sessionErrors := make(chan error, 1)
	go func() {
		sessionErrors <- server.Serve(ctx, listener)
	}()

	status.SetReady(true)
	logger.Info(
		"shard ready",
		"quic_address", listener.Addr().String(),
		"health_address", settings.HealthAddress,
	)

	select {
	case <-ctx.Done():
		status.SetReady(false)
		return nil
	case err := <-healthErrors:
		return err
	case err := <-sessionErrors:
		return err
	}
}

func startHealthServer(ctx context.Context, address string, status *health.Status) <-chan error {
	errorsChannel := make(chan error, 1)
	go func() {
		errorsChannel <- health.Serve(ctx, address, status)
	}()
	return errorsChannel
}

func shutDownTelemetry(logger *slog.Logger, shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		logger.Error("shut down telemetry", "error", err)
	}
}
