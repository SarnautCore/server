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
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	settings, err := config.Load("gateway")
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

	if err := connectToShard(ctx, settings, logger, healthErrors); err != nil {
		return err
	}
	status.SetReady(true)

	select {
	case <-ctx.Done():
		status.SetReady(false)
		return nil
	case err := <-healthErrors:
		return err
	}
}

func connectToShard(
	ctx context.Context,
	settings config.Config,
	logger *slog.Logger,
	healthErrors <-chan error,
) error {
	for {
		err := exchangeHello(ctx, settings, logger)
		if err == nil {
			return nil
		}
		logger.Warn("shard handshake failed; retrying", "error", err)

		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case err := <-healthErrors:
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

func exchangeHello(ctx context.Context, settings config.Config, logger *slog.Logger) error {
	connection, err := transport.DialQUIC(
		ctx,
		settings.QUIC.ShardAddress,
		transport.NewDevClientTLSConfig(),
	)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()

	client := session.Client{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         settings.BuildID,
	}
	hello, err := client.Handshake(ctx, connection)
	if err != nil {
		return err
	}
	logger.Info(
		"shard handshake complete",
		"protocol_version", hello.GetProtocolVersion().String(),
		"shard_build_id", hello.GetBuildId(),
		"health_address", settings.HealthAddress,
	)
	return nil
}

func shutDownTelemetry(logger *slog.Logger, shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		logger.Error("shut down telemetry", "error", err)
	}
}
