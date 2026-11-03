package service

import (
	"context"
	"errors"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
)

type WalletStore interface {
	FindWalletByUserID(ctx context.Context, userID uuid.UUID) (domain.Wallet, error)
	// CreateWallet returns the user's existing wallet if another request created it first.
	CreateWallet(ctx context.Context, userID uuid.UUID) (domain.Wallet, error)
}

type WalletUsecase struct {
	store WalletStore
}

func NewWalletUsecase(store WalletStore) *WalletUsecase {
	return &WalletUsecase{store: store}
}

// GetWallet returns the user's wallet. Wallets are created lazily on first use.
func (u *WalletUsecase) GetWallet(ctx context.Context, userID uuid.UUID) (domain.Wallet, error) {
	wallet, err := u.store.FindWalletByUserID(ctx, userID)
	if errors.Is(err, domain.ErrNotFound) {
		return u.store.CreateWallet(ctx, userID)
	}

	return wallet, err
}
