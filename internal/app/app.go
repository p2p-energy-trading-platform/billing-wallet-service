package app

import (
	"context"
	"log/slog"
	"net"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	"billing-service/internal/config"
)

type App struct {
	cfg          *config.Config
	logger       *slog.Logger
	dbPool       *pgxpool.Pool
	redisClient  *redis.Client
	grpcServer   *grpc.Server
	healthServer *http.Server
}

func New(cfg *config.Config, logger *slog.Logger, dbPool *pgxpool.Pool, redisClient *redis.Client) *App {
	grpcServer := grpc.NewServer()

	// Setup basic HTTP health check server
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	healthServer := &http.Server{
		Addr:    cfg.HealthAddress,
		Handler: mux,
	}

	return &App{
		cfg:          cfg,
		logger:       logger,
		dbPool:       dbPool,
		redisClient:  redisClient,
		grpcServer:   grpcServer,
		healthServer: healthServer,
	}
}

func (a *App) Start(ctx context.Context) error {
	// Start gRPC listener
	grpcLis, err := net.Listen("tcp", a.cfg.GRPCAddress)
	if err != nil {
		return err
	}

	go func() {
		a.logger.Info("Starting gRPC server", "addr", a.cfg.GRPCAddress)
		if err := a.grpcServer.Serve(grpcLis); err != nil && err != grpc.ErrServerStopped {
			a.logger.Error("gRPC server stopped with error", "error", err)
		}
	}()

	// Start Health Check server
	go func() {
		a.logger.Info("Starting health check server", "addr", a.cfg.HealthAddress)
		if err := a.healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			a.logger.Error("Health server stopped with error", "error", err)
		}
	}()

	return nil
}

func (a *App) Stop(ctx context.Context) {
	a.logger.Info("Shutting down application servers...")

	// Graceful stop for gRPC
	a.grpcServer.GracefulStop()

	// Shutdown health check server
	if err := a.healthServer.Shutdown(ctx); err != nil {
		a.logger.Error("Error shutting down health server", "error", err)
	}

	// Close Redis
	if err := a.redisClient.Close(); err != nil {
		a.logger.Error("Error closing redis client", "error", err)
	}

	// Close Postgres pool
	a.dbPool.Close()

	a.logger.Info("Application shutdown complete")
}
