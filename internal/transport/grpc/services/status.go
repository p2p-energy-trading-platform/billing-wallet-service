package services

import (
	"log/slog"

	grpctransport "billing-wallet-service/internal/transport/grpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// toStatus logs the cause of infrastructure failures, because the status returned
// to the caller deliberately leaves it out.
func toStatus(logger *slog.Logger, err error) error {
	converted := grpctransport.ToGRPCError(err)

	switch status.Code(converted) {
	case codes.Internal, codes.Unavailable:
		logger.Error("request failed", "error", err)
	}

	return converted
}
