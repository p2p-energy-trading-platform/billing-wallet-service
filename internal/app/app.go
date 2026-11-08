package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	"billing-wallet-service/internal/config"
	"billing-wallet-service/internal/service"
	"billing-wallet-service/internal/store/postgres"
	transportgrpc "billing-wallet-service/internal/transport/grpc"
	grpcservices "billing-wallet-service/internal/transport/grpc/services"
	"billing-wallet-service/internal/worker"
)

type App struct {
	cfg          *config.Config
	logger       *slog.Logger
	dbPool       *pgxpool.Pool
	redisClient  *redis.Client
	grpcServer   *transportgrpc.Server
	healthServer *http.Server
	reconciler   *worker.Reconciler
	stopWorkers  context.CancelFunc
	workersDone  chan struct{}
}

func New(cfg *config.Config, logger *slog.Logger, dbPool *pgxpool.Pool, redisClient *redis.Client) *App {
	walletUsecase := service.NewWalletUsecase(postgres.NewWalletRepo(dbPool))
	reservationRepo := postgres.NewReservationRepo(dbPool)
	reservationUsecase := service.NewReservationUsecase(reservationRepo)
	grpcServer := transportgrpc.New(cfg.GRPCAddress, logger, transportgrpc.Services{
		Wallet: grpcservices.NewWalletHandler(walletUsecase, logger),
		Order:  grpcservices.NewOrderHandler(reservationUsecase, logger),
	})

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
		reconciler:   worker.NewReconciler(reservationRepo, cfg.HoldSweepInterval, worker.DefaultHoldBatchSize, logger),
	}
}

func (a *App) Start(ctx context.Context) error {
	// Start gRPC listener
	grpcLis, err := a.grpcServer.Listen()
	if err != nil {
		return err
	}

	go func() {
		if err := a.grpcServer.Serve(grpcLis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			a.logger.Error("gRPC server stopped with error", "error", err)
		}
	}()
	a.grpcServer.SetReady(true)

	// Start Health Check server
	go func() {
		a.logger.Info("Starting health check server", "addr", a.cfg.HealthAddress)
		if err := a.healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			a.logger.Error("Health server stopped with error", "error", err)
		}
	}()

	a.startWorkers()

	return nil
}

// startWorkers uses its own context, because the one passed to Start only covers
// startup and times out while the service is still running.
func (a *App) startWorkers() {
	if !a.cfg.HoldSweeperEnabled {
		a.logger.Info("Hold sweeper is disabled")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.stopWorkers = cancel
	a.workersDone = make(chan struct{})

	go func() {
		defer close(a.workersDone)
		a.logger.Info("Starting hold sweeper", "interval", a.cfg.HoldSweepInterval.String())
		a.reconciler.Run(ctx)
	}()
}

func (a *App) Stop(ctx context.Context) {
	a.logger.Info("Shutting down application servers...")

	// Workers need the database, so they stop before the pool closes.
	if a.stopWorkers != nil {
		a.stopWorkers()
		<-a.workersDone
	}

	// Graceful stop for gRPC
	a.grpcServer.SetReady(false)
	a.grpcServer.Stop()

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
