// Command auth is the account service: registration, login, the character
// roster, and the two credentials of ADR 0030.
//
// It is the only process that holds credentials for the `auth` schema. The
// shard learns an account and a character from a ticket redemption over NATS
// and from nothing else.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SarnautCore/server/internal/auth"
	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/config"
	"github.com/SarnautCore/server/internal/health"
	"github.com/SarnautCore/server/internal/infra"
	"github.com/SarnautCore/server/internal/observability"
	"github.com/SarnautCore/server/internal/pack"
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

	// The chargen table is the option list. There is no built-in one, so a
	// misconfigured pack path fails here rather than serving a creation form
	// with nothing in it (ADR 0032).
	if settings.Content.PackPath == "" {
		return errors.New(
			"no content pack configured: auth reads its character-creation options from the " +
				"pack's chargen table (ADR 0032). Set SARNAUT_CONTENT_PACK to a pack directory",
		)
	}
	content, err := pack.Load(settings.Content.PackPath, pack.Options{AllowExtra: settings.Content.AllowExtra})
	if err != nil {
		return fmt.Errorf("load content pack: %w", err)
	}
	catalogue, err := auth.NewCatalogue(content.ChargenOptions())
	if err != nil {
		return err
	}

	clients, err := infra.Open(ctx, settings)
	if err != nil {
		return err
	}
	defer func() {
		if err := clients.Close(); err != nil {
			logger.Error("close infrastructure clients", "error", err)
		}
	}()

	// PostgreSQL and Valkey are both required (ADR 0030, ADR 0031): accounts
	// have nowhere else to live, and sessions, tickets and play locks have no
	// fallback. Starting without them would mean discovering it at the first
	// login instead of at boot.
	pool, err := clients.RequirePostgres()
	if err != nil {
		return err
	}
	if clients.Valkey == nil {
		return errors.New(
			"no Valkey configured: sessions, tickets and play locks have no fallback " +
				"(ADR 0030 §2). Set SARNAUT_VALKEY_ADDRESS",
		)
	}
	if clients.NATS == nil {
		return errors.New(
			"no NATS configured: the shard redeems tickets over NATS request/reply " +
				"(ADR 0030 §3). Set SARNAUT_NATS_URL",
		)
	}

	// Migrations are applied by `cmd/migrate`, not at service start: two
	// services racing to migrate the same database is a problem goose's
	// advisory lock solves and an operator should not have to think about.
	repository, err := charstore.NewPostgres(pool)
	if err != nil {
		return err
	}

	service, err := auth.New(auth.Options{
		Repository:    repository,
		Keys:          auth.NewValkeyKeyValue(clients.Valkey),
		Catalogue:     catalogue,
		NameBlocklist: settings.Auth.NameBlocklist,
		Logger:        logger,
	})
	if err != nil {
		return err
	}

	responder := auth.NewResponder(service, clients.NATS, logger)
	drain, err := responder.Subscribe(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := drain(); err != nil {
			logger.Error("drain auth subscriptions", "error", err)
		}
	}()

	apiErrors := serveAPI(ctx, settings.Auth.ListenAddress, service.Handler(), logger)

	status := new(health.Status)
	healthErrors := make(chan error, 1)
	go func() {
		healthErrors <- health.Serve(ctx, settings.HealthAddress, status)
	}()
	status.SetReady(true)
	logger.Info("auth ready",
		"health_address", settings.HealthAddress,
		"api_address", settings.Auth.ListenAddress,
		"pack_id", content.ID(),
		"chargen_options", len(catalogue.Playable()),
	)

	select {
	case <-ctx.Done():
		status.SetReady(false)
		return nil
	case err := <-healthErrors:
		return err
	case err := <-apiErrors:
		return err
	}
}

// serveAPI runs the account HTTP API on its own listener and shuts it down with
// the process context. TLS for this listener follows ADR 0017's dev
// self-signed story and is not configured here yet; in M2 it binds loopback.
func serveAPI(ctx context.Context, address string, handler http.Handler, logger *slog.Logger) <-chan error {
	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errorsChannel := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errorsChannel <- err
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("shut down auth api", "error", err)
		}
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
