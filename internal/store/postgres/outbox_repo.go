package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	walletUpdatedTopic      = "billing.wallet.updated"
	balanceChangedEventType = "wallet.balance_changed"
)

// balanceChangedEvent is the outbox payload for a wallet balance change. Delivery
// is at least once, so consumers dedupe on EventID.
type balanceChangedEvent struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
	WalletID  string `json:"wallet_id"`
	UserID    string `json:"user_id"`
	Available int64  `json:"available"`
	Reserved  int64  `json:"reserved"`
	Version   int64  `json:"version"`
	OrderID   string `json:"order_id"`
	HoldID    string `json:"hold_id"`
	Reason    string `json:"reason,omitempty"`
}

// insertBalanceChanged records the event in the same transaction as the change.
func insertBalanceChanged(ctx context.Context, tx pgx.Tx, wallet domain.Wallet, hold domain.Hold, reason domain.ReleaseReason) error {
	payload, err := json.Marshal(balanceChangedEvent{
		EventID:   uuid.NewString(),
		EventType: balanceChangedEventType,
		WalletID:  wallet.ID.String(),
		UserID:    wallet.UserID.String(),
		Available: wallet.Available,
		Reserved:  wallet.Reserved,
		Version:   wallet.Version,
		OrderID:   hold.RefID.String(),
		HoldID:    hold.ID.String(),
		Reason:    string(reason),
	})
	if err != nil {
		return fmt.Errorf("encode balance event: %w", err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO outbox (topic, key, payload) VALUES ($1, $2, $3)`,
		walletUpdatedTopic, wallet.UserID.String(), payload)
	if err != nil {
		return fmt.Errorf("insert balance event: %w", mapError(err))
	}

	return nil
}
