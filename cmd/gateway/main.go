package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	sarnautv1 "github.com/SarnautCore/server/gen/sarnaut/v1"
	"github.com/SarnautCore/server/internal/config"
	"github.com/SarnautCore/server/internal/gateway"
	"github.com/SarnautCore/server/internal/health"
	"github.com/SarnautCore/server/internal/infra"
	"github.com/SarnautCore/server/internal/observability"
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
	shutdownTelemetry, err := observability.Setup(ctx, settings.OTel, settings.ServiceName, settings.BuildID)
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
	if clients.NATS == nil {
		return errors.New("gateway requires NATS for ticket and play-lock authority")
	}
	if settings.Content.PackID == "" {
		return errors.New("gateway requires SARNAUT_CONTENT_PACK_ID")
	}
	if len(settings.Private.Secret) != 32 {
		return errors.New("gateway requires SARNAUT_PRIVATE_SHARED_SECRET_HEX")
	}
	privateTLS, err := transport.LoadPrivateClientTLSConfig(
		settings.Private.CACertificatePath, settings.Private.ServerName,
	)
	if err != nil {
		return err
	}
	publicTLS, err := transport.NewDevServerTLSConfig()
	if err != nil {
		return err
	}
	listener, err := transport.ListenQUIC(settings.QUIC.ListenAddress, publicTLS)
	if err != nil {
		return err
	}
	defer listener.Close()
	routes, err := gateway.NewRoutes(settings.Content.PackID, []gateway.Route{{
		ZoneID: settings.World.ZoneID, ShardID: settings.Private.ShardID,
		PrivateAddress: settings.QUIC.ShardAddress, PackID: settings.Content.PackID, Enabled: true,
	}})
	if err != nil {
		return err
	}
	server := &gateway.Server{
		ProtocolVersion: sarnautv1.ProtocolVersion_PROTOCOL_VERSION_1,
		BuildID:         settings.BuildID, PackID: settings.Content.PackID,
		AllowUnverifiedPack: settings.Content.AllowUnverifiedPack,
		Authority: gateway.NewNATSAuthority(
			clients.NATS, settings.Private.GatewayInstanceID, settings.Auth.RequestTimeout,
		),
		Routes: routes,
		DialShard: func(dialCtx context.Context, route gateway.Route) (transport.Connection, error) {
			return transport.DialQUIC(dialCtx, route.PrivateAddress, privateTLS)
		},
		Trust: gateway.TrustConfig{
			InstanceID: settings.Private.GatewayInstanceID,
			ShardID:    settings.Private.ShardID,
			KeyID:      settings.Private.KeyID,
			Secret:     settings.Private.Secret,
		},
		AttachTimeout: settings.Private.AttachTimeout,
		Logger:        logger,
	}
	status := new(health.Status)
	healthErrors := startHealthServer(ctx, settings.HealthAddress, status)
	sessionErrors := make(chan error, 1)
	go func() { sessionErrors <- server.Serve(ctx, listener) }()
	status.SetReady(true)
	logger.Info("gateway ready",
		"public_address", listener.Addr().String(),
		"zones", routes.ZoneIDs(),
		"pack_id", settings.Content.PackID,
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
	go func() { errorsChannel <- health.Serve(ctx, address, status) }()
	return errorsChannel
}

func shutDownTelemetry(logger *slog.Logger, shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		logger.Error("shut down telemetry", "error", err)
	}
}
