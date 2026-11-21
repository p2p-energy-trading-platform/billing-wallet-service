package services

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"billing-wallet-service/internal/domain"
	"billing-wallet-service/internal/service"
	"billing-wallet-service/internal/store/postgres"

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
	CreateDepositIntent(ctx context.Context, in service.CreateDepositInput) (service.DepositIntentResult, error)
	ConfirmDeposit(ctx context.Context, headers http.Header, rawBody []byte) (postgres.DepositConfirmOutcome, error)
	GetDeposit(ctx context.Context, userID, depositID uuid.UUID) (domain.Deposit, error)
	RequestWithdrawal(ctx context.Context, userID uuid.UUID, amount int64, destination json.RawMessage) (domain.Withdrawal, error)
	CancelWithdrawal(ctx context.Context, userID, withdrawalID uuid.UUID) (domain.Withdrawal, error)
	ListWithdrawals(ctx context.Context, userID uuid.UUID, limit int) ([]domain.Withdrawal, error)
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

func (h *WalletHandler) CreateDepositIntent(
	ctx context.Context,
	req *billingv1.CreateDepositIntentRequest,
) (*billingv1.CreateDepositIntentResponse, error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	res, err := h.usecase.CreateDepositIntent(ctx, service.CreateDepositInput{
		UserID:         userID,
		IdempotencyKey: req.GetIdempotencyKey(),
		Amount:         req.GetAmount(),
		SuccessURL:     req.GetSuccessUrl(),
		CancelURL:      req.GetCancelUrl(),
	})

	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	return &billingv1.CreateDepositIntentResponse{
		Deposit: depositIntentToProto(res.Deposit, res.CheckoutURL),
	}, nil
}

func (h *WalletHandler) ConfirmDeposit(
	ctx context.Context,
	req *billingv1.ConfirmDepositRequest,
) (*billingv1.ConfirmDepositResponse, error) {
	if req == nil || len(req.GetRawBody()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "raw_body is required")
	}

	headers := make(http.Header, len(req.GetHeaders()))
	for k, v := range req.GetHeaders() {
		headers.Add(k, v)
	}

	outcome, err := h.usecase.ConfirmDeposit(ctx, headers, req.GetRawBody())
	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	var depositID string
	if outcome.Deposit.ID != uuid.Nil {
		depositID = outcome.Deposit.ID.String()
	}

	return &billingv1.ConfirmDepositResponse{
		Processed: outcome.Processed,
		DepositId: depositID,
		Status:    depositStatusToProto(outcome.Deposit.Status),
	}, nil
}

func (h *WalletHandler) GetDeposit(
	ctx context.Context,
	req *billingv1.GetDepositRequest,
) (*billingv1.GetDepositResponse, error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil || req.GetDepositId() == "" {
		return nil, status.Error(codes.InvalidArgument, "deposit_id is required")
	}

	depositID, err := uuid.Parse(req.GetDepositId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "deposit_id must be a UUID")
	}

	dep, err := h.usecase.GetDeposit(ctx, userID, depositID)
	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	return &billingv1.GetDepositResponse{
		Deposit: depositIntentToProto(dep, ""),
	}, nil
}

func (h *WalletHandler) RequestWithdrawal(
	ctx context.Context,
	req *billingv1.RequestWithdrawalRequest,
) (*billingv1.RequestWithdrawalResponse, error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	w, err := h.usecase.RequestWithdrawal(ctx, userID, req.GetAmount(), json.RawMessage(req.GetDestinationJson()))
	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	return &billingv1.RequestWithdrawalResponse{
		Withdrawal: withdrawalToProto(w),
	}, nil
}

func (h *WalletHandler) CancelWithdrawal(
	ctx context.Context,
	req *billingv1.CancelWithdrawalRequest,
) (*billingv1.CancelWithdrawalResponse, error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil || req.GetWithdrawalId() == "" {
		return nil, status.Error(codes.InvalidArgument, "withdrawal_id is required")
	}

	withdrawalID, err := uuid.Parse(req.GetWithdrawalId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "withdrawal_id must be a UUID")
	}

	w, err := h.usecase.CancelWithdrawal(ctx, userID, withdrawalID)
	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	return &billingv1.CancelWithdrawalResponse{
		Withdrawal: withdrawalToProto(w),
	}, nil
}

func (h *WalletHandler) ListWithdrawals(
	ctx context.Context,
	req *billingv1.ListWithdrawalsRequest,
) (*billingv1.ListWithdrawalsResponse, error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}

	var pageSize int
	if req != nil {
		pageSize = int(req.GetPageSize())
	}

	items, err := h.usecase.ListWithdrawals(ctx, userID, pageSize)
	if err != nil {
		return nil, toStatus(h.logger, err)
	}

	out := make([]*billingv1.Withdrawal, 0, len(items))
	for _, item := range items {
		out = append(out, withdrawalToProto(item))
	}

	return &billingv1.ListWithdrawalsResponse{
		Withdrawals: out,
	}, nil
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

func depositIntentToProto(d domain.Deposit, checkoutURL string) *billingv1.DepositIntent {
	intent := &billingv1.DepositIntent{
		DepositId:      d.ID.String(),
		Status:         depositStatusToProto(d.Status),
		CheckoutUrl:    checkoutURL,
		Amount:         d.Amount,
		ChargeCurrency: d.ChargeCurrency,
		ChargeAmount:   d.ChargeAmount,
		CreatedAt:      timestamppb.New(d.CreatedAt),
	}

	if d.ExpiresAt != nil {
		intent.ExpiresAt = timestamppb.New(*d.ExpiresAt)
	}

	return intent
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

func depositStatusToProto(depositStatus domain.DepositStatus) billingv1.DepositStatus {
	switch depositStatus {
	case domain.DepositPending:
		return billingv1.DepositStatus_DEPOSIT_STATUS_PENDING
	case domain.DepositCompleted:
		return billingv1.DepositStatus_DEPOSIT_STATUS_COMPLETED
	case domain.DepositFailed:
		return billingv1.DepositStatus_DEPOSIT_STATUS_FAILED
	case domain.DepositExpired:
		return billingv1.DepositStatus_DEPOSIT_STATUS_EXPIRED
	case domain.DepositRefunded:
		return billingv1.DepositStatus_DEPOSIT_STATUS_REFUNDED
	case domain.DepositDisputed:
		return billingv1.DepositStatus_DEPOSIT_STATUS_DISPUTED
	default:
		return billingv1.DepositStatus_DEPOSIT_STATUS_UNSPECIFIED
	}
}

func withdrawalToProto(w domain.Withdrawal) *billingv1.Withdrawal {
	return &billingv1.Withdrawal{
		WithdrawalId: w.ID.String(),
		WalletId:     w.WalletID.String(),
		Amount:       w.Amount,
		Status:       withdrawalStatusToProto(w.Status),
		CreatedAt:    timestamppb.New(w.CreatedAt),
		UpdatedAt:    timestamppb.New(w.UpdatedAt),
	}
}

func withdrawalStatusToProto(s domain.WithdrawalStatus) billingv1.WithdrawalStatus {
	switch s {
	case domain.WithdrawalPending:
		return billingv1.WithdrawalStatus_WITHDRAWAL_STATUS_PENDING
	case domain.WithdrawalApproved:
		return billingv1.WithdrawalStatus_WITHDRAWAL_STATUS_APPROVED
	case domain.WithdrawalPaid:
		return billingv1.WithdrawalStatus_WITHDRAWAL_STATUS_PAID
	case domain.WithdrawalRejected:
		return billingv1.WithdrawalStatus_WITHDRAWAL_STATUS_REJECTED
	case domain.WithdrawalCancelled:
		return billingv1.WithdrawalStatus_WITHDRAWAL_STATUS_CANCELLED
	case domain.WithdrawalFailed:
		return billingv1.WithdrawalStatus_WITHDRAWAL_STATUS_FAILED
	default:
		return billingv1.WithdrawalStatus_WITHDRAWAL_STATUS_UNSPECIFIED
	}
}
