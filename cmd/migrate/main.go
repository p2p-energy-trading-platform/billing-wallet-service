package main

import (
	"billing-service/migrations"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	"github.com/pressly/goose/v3"
)

const (
	defaultCommand = "up"
)

func main() {
	logger := slog.New(
		slog.NewJSONHandler(
			os.Stdout,
			&slog.HandlerOptions{
				Level: slog.LevelInfo,
			},
		),
	)

	if err := godotenv.Load(); err != nil {
		logger.Error(
			"Failed to load environment variables",
		)

		printUsage()
	}

	slog.SetDefault(logger)

	databaseURL := os.Getenv("POSTGRES_URL")
	if databaseURL == "" {
		logger.Error("POSTGRES_URL is required")
		os.Exit(1)
	}

	command := defaultCommand

	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	if !validCommand(command) {
		logger.Error(
			"unsupported migration command",
			"command", command,
		)

		printUsage()

		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Minute,
	)
	defer cancel()

	if err := run(
		ctx,
		databaseURL,
		command,
		logger,
	); err != nil {
		logger.Error(
			"migration command failed",
			"command", command,
			"error", err,
		)

		os.Exit(1)
	}

	logger.Info(
		"migration command completed",
		"command", command,
	)
}

func run(
	ctx context.Context,
	databaseURL string,
	command string,
	logger *slog.Logger,
) error {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf(
			"parse database configuration: %w",
			err,
		)
	}

	db := stdlib.OpenDB(*config)
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("failed to close database connection", "error", err)
		}
	}()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf(
			"ping database: %w",
			err,
		)
	}

	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		db,
		migrations.FS,
		goose.WithSlog(logger),
	)
	if err != nil {
		return fmt.Errorf(
			"create migration provider: %w",
			err,
		)
	}

	switch command {
	case "up":
		_, err = provider.Up(ctx)

	case "down":
		_, err = provider.Down(ctx)

	case "status":
		err = printStatus(
			ctx,
			provider,
		)

	case "version":
		err = printVersion(
			ctx,
			provider,
		)
	}

	if err != nil {
		return fmt.Errorf(
			"execute %q migration command: %w",
			command,
			err,
		)
	}

	return nil
}

func printStatus(
	ctx context.Context,
	provider *goose.Provider,
) error {
	statuses, err := provider.Status(ctx)
	if err != nil {
		return err
	}

	for _, status := range statuses {
		slog.Info(
			"migration status",
			"version", status.Source.Version,
			"path", status.Source.Path,
			"state", status.State,
		)
	}

	return nil
}

func printVersion(
	ctx context.Context,
	provider *goose.Provider,
) error {
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return err
	}

	slog.Info(
		"current database migration version",
		"version", version,
	)

	return nil
}

func validCommand(command string) bool {
	switch command {
	case "up",
		"down",
		"status",
		"version":
		return true

	default:
		return false
	}
}

func printUsage() {
	fmt.Fprintln(
		os.Stderr,
		"usage: migrate [up|down|status|version]",
	)
}

// Ensure database/sql remains part of the migration executable's
// explicit dependency surface.
var _ *sql.DB
