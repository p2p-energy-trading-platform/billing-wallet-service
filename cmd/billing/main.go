package main

import (
	"context"
	"log"
	"os"
	"time"

	"billing-service/internal/app"
	"billing-service/internal/config"
	"billing-service/internal/observability"
	"billing-service/internal/store/postgres"
	"billing-service/internal/store/redis"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		// Fallback to basic logger if config fails
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Initialize structured logger
	logger := observability.SetupLogger(cfg.ServiceName, cfg.ServiceEnv, cfg.LogLevel)
	logger.Info("Initializing billing service", "service", cfg.ServiceName, "env", cfg.ServiceEnv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Connect to PostgreSQL
	dbPool, err := postgres.NewPool(ctx, cfg.PostgresURL)
	if err != nil {
		logger.Error("Failed to connect to PostgreSQL", "error", err)
		os.Exit(1)
	}
	logger.Info("Successfully connected to PostgreSQL")

	// Connect to Redis
	redisClient, err := redis.NewClient(ctx, cfg.RedisAddress)
	if err != nil {
		logger.Error("Failed to connect to Redis", "error", err)
		os.Exit(1)
	}
	logger.Info("Successfully connected to Redis")

	// Instantiate App
	application := app.New(cfg, logger, dbPool, redisClient)

	// Start servers
	if err := application.Start(ctx); err != nil {
		logger.Error("Failed to start application servers", "error", err)
		os.Exit(1)
	}

	// Block until signal and gracefully shut down
	app.WaitForShutdown(logger, func(shutdownCtx context.Context) {
		application.Stop(shutdownCtx)
	})
}
