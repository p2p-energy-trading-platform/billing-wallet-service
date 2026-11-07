package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	ServiceName    string
	ServiceEnv     string
	LogLevel       string
	PostgresURL    string
	RedisAddress   string
	GRPCAddress    string
	HealthAddress  string
	MetricsAddress string

	// HoldSweeperEnabled releases order holds past their expires_at. Off by default
	// until OMS has agreed how it sets that value.
	HoldSweeperEnabled bool
	HoldSweepInterval  time.Duration
}

func Load() (*Config, error) {

	// Load local .env if present
	_ = godotenv.Load()

	cfg := &Config{
		ServiceName:    getEnv("SERVICE_NAME", "billing-wallet-service"),
		ServiceEnv:     getEnv("SERVICE_ENV", "development"),
		LogLevel:       getEnv("LOG_LEVEL", "info"),
		PostgresURL:    getEnv("POSTGRES_URL", ""),
		RedisAddress:   getEnv("REDIS_ADDRESS", "localhost:6379"),
		GRPCAddress:    getEnv("GRPC_ADDRESS", ":50051"),
		HealthAddress:  getEnv("HEALTH_ADDRESS", ":8080"),
		MetricsAddress: getEnv("METRICS_ADDRESS", ":9090"),
	}

	var err error
	if cfg.HoldSweeperEnabled, err = strconv.ParseBool(getEnv("HOLD_SWEEPER_ENABLED", "false")); err != nil {
		return nil, fmt.Errorf("invalid HOLD_SWEEPER_ENABLED: %w", err)
	}
	if cfg.HoldSweepInterval, err = time.ParseDuration(getEnv("HOLD_SWEEP_INTERVAL", "1m")); err != nil {
		return nil, fmt.Errorf("invalid HOLD_SWEEP_INTERVAL: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("Validate config: %w", err)
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
