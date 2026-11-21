package config

import (
	"fmt"
	"strings"
)

func (c *Config) Validate() error {
	if c.ServiceName == "" {
		return fmt.Errorf("SERVICE_NAME cannot be empty")
	}

	if c.ServiceEnv == "" {
		return fmt.Errorf("SERVICE_ENV cannot be empty")
	}

	// Validate log level
	validLogLevels := map[string]bool{
		"debug": true,
		"info":  true,
		"warn":  true,
		"error": true,
	}

	if !validLogLevels[strings.ToLower(c.LogLevel)] {
		return fmt.Errorf("invalid LOG_LEVEL: %q (must be debug, info, warn, or error)", c.LogLevel)
	}

	if c.PostgresURL == "" {
		return fmt.Errorf("POSTGRES_URL is required")
	}

	if c.RedisAddress == "" {
		return fmt.Errorf("REDIS_ADDRESS is required")
	}

	if c.GRPCAddress == "" {
		return fmt.Errorf("GRPC_ADDRESS is required")
	}

	if c.HealthAddress == "" {
		return fmt.Errorf("HEALTH_ADDRESS is required")
	}

	if c.HoldSweepInterval <= 0 {
		return fmt.Errorf("invalid HOLD_SWEEP_INTERVAL: %s (must be positive)", c.HoldSweepInterval)
	}

	if c.DepositSweeperInterval <= 0 {
		return fmt.Errorf("invalid DEPOSIT_SWEEPER_INTERVAL: %s (must be positive)", c.DepositSweeperInterval)
	}

	if c.StripeSecretKey == "" {
		return fmt.Errorf("STRIPE_SECRET_KEY is required")
	}

	if c.StripeWebhookSecret == "" {
		return fmt.Errorf("STRIPE_WEBHOOK_SECRET is required")
	}

	return nil
}
