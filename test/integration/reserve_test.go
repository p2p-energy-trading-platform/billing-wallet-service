//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestReserveLocksFundsAndRecordsTheHold(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)

	request := limitOrder(userID, orderID)
	request.ExpiresAt = timestamppb.New(expiresAt)
	response := env.reserve(t, request)

	if response.GetReservedAmount() != testOrderAmount {
		t.Errorf("reserved_amount = %d, want %d", response.GetReservedAmount(), testOrderAmount)
	}
	if response.GetAvailableAfter() != 10_000-testOrderAmount {
		t.Errorf("available_after = %d, want %d", response.GetAvailableAfter(), 10_000-testOrderAmount)
	}
	env.assertBalances(t, userID, 10_000-testOrderAmount, testOrderAmount)
	env.assertReservedMatchesHolds(t, userID)

	var (
		kind, holdStatus, orderType string
		qty, price, total           int64
		bps                         int
		configID, activeConfigID    uuid.UUID
		storedExpiry                time.Time
	)
	ctx := context.Background()
	err := env.db.QueryRowContext(ctx,
		`SELECT kind, status, order_type, qty_wh, max_price_per_kwh, amount_total, buyer_fee_bps, buyer_fee_config_id, expires_at
		 FROM holds WHERE id = $1 AND ref_id = $2`, response.GetHoldId(), orderID).
		Scan(&kind, &holdStatus, &orderType, &qty, &price, &total, &bps, &configID, &storedExpiry)
	if err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if kind != "ORDER" || holdStatus != "ACTIVE" || orderType != "LIMIT" {
		t.Errorf("hold = %s/%s/%s, want ORDER/ACTIVE/LIMIT", kind, holdStatus, orderType)
	}
	if qty != testQtyWh || price != testPrice || total != testOrderAmount || bps != 100 {
		t.Errorf("hold values = %d/%d/%d/%d", qty, price, total, bps)
	}
	if !storedExpiry.Equal(expiresAt) {
		t.Errorf("expires_at = %v, want %v", storedExpiry, expiresAt)
	}
	if err := env.db.QueryRowContext(ctx, `SELECT id FROM fee_configs LIMIT 1`).Scan(&activeConfigID); err != nil {
		t.Fatalf("read fee config: %v", err)
	}
	if configID != activeConfigID {
		t.Errorf("buyer_fee_config_id = %v, want %v", configID, activeConfigID)
	}
}

func TestReserveMovesNoMoneyInTheLedger(t *testing.T) {
	env := newReservationEnv(t)

	env.reserve(t, limitOrder(env.fund(t, 10_000), uuid.New()))

	var journals, lines int
	ctx := context.Background()
	if err := env.db.QueryRowContext(ctx, `SELECT count(*) FROM journals`).Scan(&journals); err != nil {
		t.Fatalf("count journals: %v", err)
	}
	if err := env.db.QueryRowContext(ctx, `SELECT count(*) FROM ledger_lines`).Scan(&lines); err != nil {
		t.Fatalf("count lines: %v", err)
	}
	if journals != 0 || lines != 0 {
		t.Errorf("journals = %d, lines = %d, want none: a hold only encumbers the user's own money", journals, lines)
	}
}

func TestReserveAndReleaseEachPublishABalanceEvent(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()

	env.reserve(t, limitOrder(userID, orderID))
	env.release(t, orderID)
	env.release(t, orderID) // moved nothing, so it publishes nothing

	rows, err := env.db.QueryContext(context.Background(),
		`SELECT topic, key, payload FROM outbox WHERE published_at IS NULL ORDER BY id`)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()

	type event struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
		UserID    string `json:"user_id"`
		Available int64  `json:"available"`
		Reserved  int64  `json:"reserved"`
		OrderID   string `json:"order_id"`
		Reason    string `json:"reason"`
	}
	var events []event
	for rows.Next() {
		var topic, key string
		var payload []byte
		if err := rows.Scan(&topic, &key, &payload); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		if topic != "billing.wallet.updated" || key != userID.String() {
			t.Errorf("topic/key = %q/%q", topic, key)
		}
		var decoded event
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		events = append(events, decoded)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox rows: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].EventID == "" || events[0].EventID == events[1].EventID {
		t.Errorf("event ids must be set and unique: %q, %q", events[0].EventID, events[1].EventID)
	}
	if events[0].EventType != "wallet.balance_changed" || events[0].Reserved != testOrderAmount || events[0].Available != 10_000-testOrderAmount {
		t.Errorf("reserve event = %+v", events[0])
	}
	if events[1].Reserved != 0 || events[1].Available != 10_000 || events[1].Reason != "CANCELLED" || events[1].OrderID != orderID.String() {
		t.Errorf("release event = %+v", events[1])
	}
}

func TestMarketOrderReservesAtTheCollar(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)

	request := limitOrder(userID, uuid.New())
	request.OrderType = billingv1.OrderType_ORDER_TYPE_MARKET
	response := env.reserve(t, request)

	if response.GetReservedAmount() != testOrderAmount {
		t.Errorf("reserved_amount = %d, want %d", response.GetReservedAmount(), testOrderAmount)
	}

	var orderType string
	var collar int64
	err := env.db.QueryRowContext(context.Background(),
		`SELECT order_type, max_price_per_kwh FROM holds WHERE id = $1`, response.GetHoldId()).Scan(&orderType, &collar)
	if err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if orderType != "MARKET" || collar != testPrice {
		t.Errorf("hold = %s at %d, want MARKET at %d", orderType, collar, testPrice)
	}
}

func TestReserveRejections(t *testing.T) {
	tests := []struct {
		name       string
		change     func(*billingv1.ReserveCreditsRequest)
		wantCode   codes.Code
		wantReason string
	}{
		{"market without collar", func(r *billingv1.ReserveCreditsRequest) {
			r.OrderType = billingv1.OrderType_ORDER_TYPE_MARKET
			r.MaxPricePerKwh = 0
		}, codes.InvalidArgument, "MARKET_ORDER_NEEDS_COLLAR"},
		{"zero quantity", func(r *billingv1.ReserveCreditsRequest) { r.QtyWh = 0 }, codes.InvalidArgument, "BAD_AMOUNT"},
		{"limit without price", func(r *billingv1.ReserveCreditsRequest) { r.MaxPricePerKwh = 0 }, codes.InvalidArgument, "BAD_AMOUNT"},
		{"amount overflow", func(r *billingv1.ReserveCreditsRequest) {
			r.QtyWh = math.MaxInt64
			r.MaxPricePerKwh = math.MaxInt64
		}, codes.InvalidArgument, "BAD_AMOUNT"},
		{"no order type", func(r *billingv1.ReserveCreditsRequest) {
			r.OrderType = billingv1.OrderType_ORDER_TYPE_UNSPECIFIED
		}, codes.InvalidArgument, "VALIDATION_FAILED"},
		{"bad user id", func(r *billingv1.ReserveCreditsRequest) { r.UserId = "nope" }, codes.InvalidArgument, "VALIDATION_FAILED"},
		{"bad order id", func(r *billingv1.ReserveCreditsRequest) { r.OrderId = "nope" }, codes.InvalidArgument, "VALIDATION_FAILED"},
		{"expired already", func(r *billingv1.ReserveCreditsRequest) {
			r.ExpiresAt = timestamppb.New(time.Now().Add(-time.Hour))
		}, codes.InvalidArgument, "VALIDATION_FAILED"},
	}

	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := limitOrder(userID, uuid.New())
			tt.change(request)

			_, err := env.orders.ReserveCredits(context.Background(), request)

			assertRejected(t, err, tt.wantCode, tt.wantReason)
			env.assertBalances(t, userID, 10_000, 0)
		})
	}
}

func TestReserveRequiresEnoughAvailableFunds(t *testing.T) {
	env := newReservationEnv(t)

	t.Run("one unit short", func(t *testing.T) {
		userID := env.fund(t, testOrderAmount-1)

		_, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, uuid.New()))

		assertRejected(t, err, codes.FailedPrecondition, "INSUFFICIENT_FUNDS")
		env.assertBalances(t, userID, testOrderAmount-1, 0)
	})

	t.Run("exactly enough", func(t *testing.T) {
		userID := env.fund(t, testOrderAmount)

		env.reserve(t, limitOrder(userID, uuid.New()))

		env.assertBalances(t, userID, 0, testOrderAmount)
	})

	t.Run("funds already reserved do not count", func(t *testing.T) {
		userID := env.fund(t, testOrderAmount+1)
		env.reserve(t, limitOrder(userID, uuid.New()))

		_, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, uuid.New()))

		assertRejected(t, err, codes.FailedPrecondition, "INSUFFICIENT_FUNDS")
		env.assertBalances(t, userID, 1, testOrderAmount)
	})

	t.Run("a user with no wallet has no funds", func(t *testing.T) {
		_, err := env.orders.ReserveCredits(context.Background(), limitOrder(uuid.New(), uuid.New()))

		assertRejected(t, err, codes.FailedPrecondition, "INSUFFICIENT_FUNDS")
	})
}

func TestReserveRejectsAWalletThatIsNotActive(t *testing.T) {
	env := newReservationEnv(t)

	for _, walletStatus := range []string{"FROZEN", "CLOSED"} {
		t.Run(walletStatus, func(t *testing.T) {
			userID := env.fund(t, 10_000)
			exec(t, env.db, `UPDATE wallets SET status = $1 WHERE user_id = $2`, walletStatus, userID)

			_, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, uuid.New()))

			assertRejected(t, err, codes.FailedPrecondition, "WALLET_FROZEN")
			env.assertBalances(t, userID, 10_000, 0)
		})
	}
}

func TestReserveIgnoresAFeeConfigNotYetEffective(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 100_000)
	exec(t, env.db, `INSERT INTO fee_configs (buyer_fee_bps, seller_fee_bps, effective_from) VALUES (500, 100, now() + interval '1 day')`)

	response := env.reserve(t, limitOrder(userID, uuid.New()))

	if response.GetReservedAmount() != testOrderAmount {
		t.Errorf("reserved %d, want %d at the current rate", response.GetReservedAmount(), testOrderAmount)
	}
}
