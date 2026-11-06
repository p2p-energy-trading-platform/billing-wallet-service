package grpc

import (
	"fmt"
	"log/slog"
	"net"
	"time"

	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

const gracefulStopTimeout = 5 * time.Second

// Services holds the handlers this server registers next to the health service.
type Services struct {
	Wallet billingv1.WalletServiceServer
	Order  billingv1.OrderBillingServiceServer
}

type Server struct {
	address string
	server  *grpc.Server
	health  *health.Server
	logger  *slog.Logger
}

func New(address string, logger *slog.Logger, services Services) *Server {
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(4<<20),
		grpc.MaxSendMsgSize(4<<20),
		grpc.ChainUnaryInterceptor(
			UnaryServerRecovery(logger),
			UnaryServerLogger(logger),
		),
	)

	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)

	healthServer.SetServingStatus(
		"",
		grpc_health_v1.HealthCheckResponse_NOT_SERVING,
	)

	if logger == nil {
		logger = slog.Default()
	}

	billingv1.RegisterWalletServiceServer(server, services.Wallet)
	billingv1.RegisterOrderBillingServiceServer(server, services.Order)

	return &Server{
		address: address,
		server:  server,
		health:  healthServer,
		logger:  logger,
	}
}

// Listen binds the server's address, so a port conflict can fail startup before
// the blocking Serve runs.
func (server *Server) Listen() (net.Listener, error) {
	listener, err := net.Listen("tcp", server.address)

	if err != nil {
		return nil, fmt.Errorf("listen grpc: %w", err)
	}

	return listener, nil
}

func (server *Server) Serve(listener net.Listener) error {
	server.logger.Info("starting gRPC server", "address", listener.Addr().String())

	if err := server.server.Serve(listener); err != nil {
		return fmt.Errorf("serve grpc: %w", err)
	}

	return nil
}

func (server *Server) SetReady(ready bool) {
	status := grpc_health_v1.HealthCheckResponse_NOT_SERVING

	if ready {
		status = grpc_health_v1.HealthCheckResponse_SERVING
	}

	server.health.SetServingStatus("", status)
}

func (server *Server) Stop() {

	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		server.server.GracefulStop()
	}()

	timer := time.NewTimer(gracefulStopTimeout)
	defer timer.Stop()

	select {
	case <-stopped:
		server.logger.Info("gRPC server stopped gracefully")
		return
	case <-timer.C:
		server.logger.Warn("gRPC graceful shutdown timed out, forcing stop")
		server.server.Stop()
		<-stopped
	}

}
