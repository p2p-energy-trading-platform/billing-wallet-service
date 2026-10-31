package observability

import (
	"log/slog"
	"os"
	"strings"
)

func SetupLogger(
	service string,
	environment string,
	level string,
) *slog.Logger {
	logLevel := parseLevel(level)

	options := &slog.HandlerOptions{
		Level:     logLevel,
		AddSource: environment != "production",
	}

	var handler slog.Handler

	// In local dev it shows readable log format while in production it will show json logs
	if environment == "development" || environment == "dev" || environment == "local" {
		handler = slog.NewTextHandler(os.Stdout, options)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, options)
	}

	logger := slog.New(handler).With(
		slog.String("service", service),
		slog.String("environment", environment),
	)

	slog.SetDefault(logger)

	return logger
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info":
		fallthrough
	default:
		return slog.LevelInfo
	}
}
