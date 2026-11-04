package service

import (
	"context"
	"errors"
	"testing"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
)

type fakeWalletStore struct {
	findWallet domain.Wallet
	findErr    error
	createErr  error
	created    []uuid.UUID
}

func (f *fakeWalletStore) FindWalletByUserID(context.Context, uuid.UUID) (domain.Wallet, error) {
	return f.findWallet, f.findErr
}

func (f *fakeWalletStore) CreateWallet(_ context.Context, userID uuid.UUID) (domain.Wallet, error) {
	f.created = append(f.created, userID)
	if f.createErr != nil {
		return domain.Wallet{}, f.createErr
	}
	return domain.Wallet{UserID: userID, Status: domain.WalletActive}, nil
}

func TestGetWalletReturnsExistingWalletWithoutCreating(t *testing.T) {
	userID := uuid.New()
	store := &fakeWalletStore{findWallet: domain.Wallet{UserID: userID, Available: 500}}

	wallet, err := NewWalletUsecase(store).GetWallet(context.Background(), userID)

	if err != nil {
		t.Fatalf("GetWallet() unexpected error: %v", err)
	}
	if wallet.Available != 500 {
		t.Errorf("Available = %d, want 500", wallet.Available)
	}
	if len(store.created) != 0 {
		t.Errorf("created %d wallets, want none", len(store.created))
	}
}

func TestGetWalletCreatesWalletOnFirstUse(t *testing.T) {
	userID := uuid.New()
	store := &fakeWalletStore{findErr: domain.ErrNotFound}

	wallet, err := NewWalletUsecase(store).GetWallet(context.Background(), userID)

	if err != nil {
		t.Fatalf("GetWallet() unexpected error: %v", err)
	}
	if wallet.UserID != userID {
		t.Errorf("UserID = %v, want %v", wallet.UserID, userID)
	}
	if len(store.created) != 1 || store.created[0] != userID {
		t.Errorf("created = %v, want one wallet for %v", store.created, userID)
	}
}

func TestGetWalletDoesNotCreateWhenLookupFails(t *testing.T) {
	store := &fakeWalletStore{findErr: domain.ErrUnavailable}

	_, err := NewWalletUsecase(store).GetWallet(context.Background(), uuid.New())

	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("GetWallet() error = %v, want ErrUnavailable", err)
	}
	if len(store.created) != 0 {
		t.Errorf("created %d wallets after a failed lookup, want none", len(store.created))
	}
}

func TestGetWalletReturnsCreateFailure(t *testing.T) {
	store := &fakeWalletStore{findErr: domain.ErrNotFound, createErr: domain.ErrUnavailable}

	_, err := NewWalletUsecase(store).GetWallet(context.Background(), uuid.New())

	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("GetWallet() error = %v, want ErrUnavailable", err)
	}
}
