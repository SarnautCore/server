// Command migrate applies the embedded goose migrations to a PostgreSQL
// database.
//
//	go run ./cmd/migrate up
//	go run ./cmd/migrate down-to 0
//	go run ./cmd/migrate status
//
// The DSN comes from -dsn, else SARNAUT_POSTGRES_DSN, else the config file
// pointed at by SARNAUT_CONFIG — the same resolution order every service uses,
// so a developer never has to hold two copies of the connection string.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/SarnautCore/server/internal/charstore"
	"github.com/SarnautCore/server/internal/config"
)

const usage = `usage: migrate [-dsn DSN] [-timeout DURATION] COMMAND [ARGS]

commands:
  up                 apply every pending migration
  down               roll back the most recently applied migration
  down-to VERSION    roll back to VERSION (0 empties the schema)
  redo               roll back and re-apply the latest migration
  status             list applied and pending migrations
  version            print the current schema version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.Usage = func() { _, _ = fmt.Fprint(os.Stderr, usage) }
	dsn := flags.String("dsn", "", "PostgreSQL DSN (defaults to SARNAUT_POSTGRES_DSN)")
	timeout := flags.Duration("timeout", time.Minute, "deadline for the whole command")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		flags.Usage()
		return errors.New("no command given")
	}

	resolved, err := resolveDSN(*dsn)
	if err != nil {
		return err
	}

	migrator, err := charstore.NewMigrator(resolved)
	if err != nil {
		return err
	}
	defer func() {
		if err := migrator.Close(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		}
	}()

	commandContext, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	if err := migrator.Ping(commandContext); err != nil {
		return err
	}
	return dispatch(commandContext, migrator, flags.Arg(0), flags.Args()[1:])
}

func dispatch(ctx context.Context, migrator *charstore.Migrator, command string, arguments []string) error {
	switch command {
	case "up":
		return migrator.Up(ctx)
	case "down":
		return migrator.Down(ctx)
	case "down-to":
		if len(arguments) != 1 {
			return errors.New("down-to needs exactly one version argument")
		}
		version, err := strconv.ParseInt(arguments[0], 10, 64)
		if err != nil {
			return fmt.Errorf("parse version %q: %w", arguments[0], err)
		}
		return migrator.DownTo(ctx, version)
	case "redo":
		if err := migrator.Down(ctx); err != nil {
			return err
		}
		return migrator.Up(ctx)
	case "status":
		return migrator.Status(ctx)
	case "version":
		version, err := migrator.Version(ctx)
		if err != nil {
			return err
		}
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

// resolveDSN prefers the flag, then falls back to the service configuration so
// that SARNAUT_POSTGRES_DSN and SARNAUT_CONFIG both work unchanged.
func resolveDSN(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}

	settings, err := config.Load("migrate")
	if err != nil {
		return "", fmt.Errorf("load configuration: %w", err)
	}
	if settings.Postgres.DSN == "" {
		return "", errors.New("no DSN: pass -dsn or set SARNAUT_POSTGRES_DSN")
	}
	return settings.Postgres.DSN, nil
}
