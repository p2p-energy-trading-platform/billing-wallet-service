package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	walletUpdatedTopic      = "billing.wallet.updated"
	tradeSettledTopic       = "billing.trade.settled"
	balanceChangedEventType = "wallet.balance_changed"
	tradeSettledEventType   = "trade.settled"
)

// balanceChange says why a wallet's balance moved. Empty fields are left out.
type balanceChange struct {
	OrderID uuid.UUID
	HoldID  uuid.UUID
	TradeID uuid.UUID
	Reason  string
}

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
	OrderID   string `json:"order_id,omitempty"`
	HoldID    string `json:"hold_id,omitempty"`
	TradeID   string `json:"trade_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// tradeSettledEvent is the outbox payload for a settled trade.
type tradeSettledEvent struct {
	EventID        string    `json:"event_id"`
	EventType      string    `json:"event_type"`
	TradeID        string    `json:"trade_id"`
	SettlementID   string    `json:"settlement_id"`
	BuyOrderID     string    `json:"buy_order_id"`
	SellOrderID    string    `json:"sell_order_id"`
	BuyerUserID    string    `json:"buyer_user_id"`
	SellerUserID   string    `json:"seller_user_id"`
	QtyWh          int64     `json:"qty_wh"`
	PricePerKwh    int64     `json:"price_per_kwh"`
	TradeValue     int64     `json:"trade_value"`
	BuyerFee       int64     `json:"buyer_fee"`
	SellerFee      int64     `json:"seller_fee"`
	BuyerDebit     int64     `json:"buyer_debit"`
	SellerCredit   int64     `json:"seller_credit"`
	PlatformCredit int64     `json:"platform_credit"`
	ExecutedAt     time.Time `json:"executed_at"`
}

func optionalID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// insertBalanceChanged records the event in the same transaction as the change.
func insertBalanceChanged(ctx context.Context, tx pgx.Tx, wallet domain.Wallet, change balanceChange) error {
	return insertOutbox(ctx, tx, walletUpdatedTopic, wallet.UserID.String(), balanceChangedEvent{
		EventID:   uuid.NewString(),
		EventType: balanceChangedEventType,
		WalletID:  wallet.ID.String(),
		UserID:    wallet.UserID.String(),
		Available: wallet.Available,
		Reserved:  wallet.Reserved,
		Version:   wallet.Version,
		OrderID:   optionalID(change.OrderID),
		HoldID:    optionalID(change.HoldID),
		TradeID:   optionalID(change.TradeID),
		Reason:    change.Reason,
	})
}

func insertTradeSettled(ctx context.Context, tx pgx.Tx, event tradeSettledEvent) error {
	event.EventID = uuid.NewString()
	event.EventType = tradeSettledEventType

	return insertOutbox(ctx, tx, tradeSettledTopic, event.TradeID, event)
}

func insertOutbox(ctx context.Context, tx pgx.Tx, topic, key string, event any) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", topic, err)
	}

	_, err = tx.Exec(ctx, `INSERT INTO outbox (topic, key, payload) VALUES ($1, $2, $3)`, topic, key, payload)
	if err != nil {
		return fmt.Errorf("insert %s event: %w", topic, mapError(err))
	}

	return nil
}
