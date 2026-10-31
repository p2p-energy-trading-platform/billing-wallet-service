package config

import (
	"fmt"
	"os"

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
