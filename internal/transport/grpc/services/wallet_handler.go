package services

import (
	"context"
	"log/slog"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// userIDMetadataKey carries the user the API Gateway authenticated.
const userIDMetadataKey = "x-gridx-user-id"

type WalletUsecase interface {
	GetWallet(ctx context.Context, userID uuid.UUID) (domain.Wallet, error)
}

type WalletHandler struct {
	billingv1.UnimplementedWalletServiceServer
	usecase WalletUsecase
	logger  *slog.Logger
}

func NewWalletHandler(usecase WalletUsecase, logger *slog.Logger) *WalletHandler {
	return &WalletHandler{usecase: usecase, logger: logger}
}

func (h *WalletHandler) GetWallet(ctx context.Context, _ *billingv1.GetWalletRequest) (*billingv1.GetWalletResponse, error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}

	wallet, err := h.usecase.GetWallet(ctx, userID)
	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	return &billingv1.GetWalletResponse{Wallet: walletToProto(wallet)}, nil
}

func callerUserID(ctx context.Context) (uuid.UUID, error) {
	md, _ := metadata.FromIncomingContext(ctx)

	values := md.Get(userIDMetadataKey)
	if len(values) != 1 {
		return uuid.Nil, status.Error(codes.Unauthenticated, "exactly one caller user id is required")
	}

	userID, err := uuid.Parse(values[0])
	if err != nil {
		return uuid.Nil, status.Error(codes.InvalidArgument, "caller user id must be a UUID")
	}

	return userID, nil
}

func walletToProto(wallet domain.Wallet) *billingv1.Wallet {
	return &billingv1.Wallet{
		WalletId:  wallet.ID.String(),
		UserId:    wallet.UserID.String(),
		Available: wallet.Available,
		Reserved:  wallet.Reserved,
		Total:     wallet.Total(),
		Status:    walletStatusToProto(wallet.Status),
		UpdatedAt: timestamppb.New(wallet.UpdatedAt),
	}
}

func walletStatusToProto(walletStatus domain.WalletStatus) billingv1.WalletStatus {
	switch walletStatus {
	case domain.WalletActive:
		return billingv1.WalletStatus_WALLET_STATUS_ACTIVE
	case domain.WalletFrozen:
		return billingv1.WalletStatus_WALLET_STATUS_FROZEN
	case domain.WalletClosed:
		return billingv1.WalletStatus_WALLET_STATUS_CLOSED
	default:
		return billingv1.WalletStatus_WALLET_STATUS_UNSPECIFIED
	}
}
