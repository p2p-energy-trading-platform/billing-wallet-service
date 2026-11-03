package grpc

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type fakeWalletUsecase struct {
	wallet domain.Wallet
	err    error
	userID uuid.UUID
	calls  int
}

func (f *fakeWalletUsecase) GetWallet(_ context.Context, userID uuid.UUID) (domain.Wallet, error) {
	f.calls++
	f.userID = userID
	return f.wallet, f.err
}

func newTestHandler(usecase WalletUsecase) *WalletHandler {
	return NewWalletHandler(usecase, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func contextWithUser(values ...string) context.Context {
	md := metadata.MD{}
	for _, value := range values {
		md.Append(userIDMetadataKey, value)
	}
	return metadata.NewIncomingContext(context.Background(), md)
}

func TestGetWalletMapsTheCallersWallet(t *testing.T) {
	userID := uuid.New()
	walletID := uuid.New()
	updatedAt := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	usecase := &fakeWalletUsecase{wallet: domain.Wallet{
		ID:        walletID,
		UserID:    userID,
		Available: 700,
		Reserved:  300,
		Status:    domain.WalletFrozen,
		UpdatedAt: updatedAt,
	}}

	response, err := newTestHandler(usecase).GetWallet(contextWithUser(userID.String()), &billingv1.GetWalletRequest{})

	if err != nil {
		t.Fatalf("GetWallet() unexpected error: %v", err)
	}
	if usecase.userID != userID {
		t.Errorf("looked up user %v, want %v", usecase.userID, userID)
	}

	wallet := response.GetWallet()
	if wallet.GetWalletId() != walletID.String() || wallet.GetUserId() != userID.String() {
		t.Errorf("ids = %q/%q, want %v/%v", wallet.GetWalletId(), wallet.GetUserId(), walletID, userID)
	}
	if wallet.GetAvailable() != 700 || wallet.GetReserved() != 300 || wallet.GetTotal() != 1000 {
		t.Errorf("balances = %d/%d/%d, want 700/300/1000", wallet.GetAvailable(), wallet.GetReserved(), wallet.GetTotal())
	}
	if wallet.GetStatus() != billingv1.WalletStatus_WALLET_STATUS_FROZEN {
		t.Errorf("status = %v, want FROZEN", wallet.GetStatus())
	}
	if !wallet.GetUpdatedAt().AsTime().Equal(updatedAt) {
		t.Errorf("updated_at = %v, want %v", wallet.GetUpdatedAt().AsTime(), updatedAt)
	}
}

func TestGetWalletMapsEveryStatus(t *testing.T) {
	tests := map[domain.WalletStatus]billingv1.WalletStatus{
		domain.WalletActive:        billingv1.WalletStatus_WALLET_STATUS_ACTIVE,
		domain.WalletFrozen:        billingv1.WalletStatus_WALLET_STATUS_FROZEN,
		domain.WalletClosed:        billingv1.WalletStatus_WALLET_STATUS_CLOSED,
		domain.WalletStatus("NEW"): billingv1.WalletStatus_WALLET_STATUS_UNSPECIFIED,
	}

	for status, want := range tests {
		t.Run(string(status), func(t *testing.T) {
			usecase := &fakeWalletUsecase{wallet: domain.Wallet{Status: status}}

			response, err := newTestHandler(usecase).GetWallet(contextWithUser(uuid.NewString()), &billingv1.GetWalletRequest{})

			if err != nil {
				t.Fatalf("GetWallet() unexpected error: %v", err)
			}
			if got := response.GetWallet().GetStatus(); got != want {
				t.Errorf("status = %v, want %v", got, want)
			}
		})
	}
}

func TestGetWalletRejectsBadIdentity(t *testing.T) {
	tests := []struct {
		name     string
		ctx      context.Context
		wantCode codes.Code
	}{
		{name: "no metadata", ctx: context.Background(), wantCode: codes.Unauthenticated},
		{name: "no user id", ctx: contextWithUser(), wantCode: codes.Unauthenticated},
		{name: "two user ids", ctx: contextWithUser(uuid.NewString(), uuid.NewString()), wantCode: codes.Unauthenticated},
		{name: "not a uuid", ctx: contextWithUser("not-a-uuid"), wantCode: codes.InvalidArgument},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usecase := &fakeWalletUsecase{}

			_, err := newTestHandler(usecase).GetWallet(tt.ctx, &billingv1.GetWalletRequest{})

			if code := status.Code(err); code != tt.wantCode {
				t.Errorf("code = %v, want %v", code, tt.wantCode)
			}
			if usecase.calls != 0 {
				t.Errorf("usecase called %d times, want 0", usecase.calls)
			}
		})
	}
}

func TestGetWalletMapsUsecaseErrors(t *testing.T) {
	usecase := &fakeWalletUsecase{err: domain.ErrUnavailable}

	_, err := newTestHandler(usecase).GetWallet(contextWithUser(uuid.NewString()), &billingv1.GetWalletRequest{})

	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", code)
	}
}
