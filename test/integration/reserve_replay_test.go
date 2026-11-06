//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc/codes"
)

func TestReserveReplayReturnsTheOriginalHold(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()

	first := env.reserve(t, limitOrder(userID, orderID))
	second := env.reserve(t, limitOrder(userID, orderID))

	if first.GetHoldId() != second.GetHoldId() || second.GetReservedAmount() != testOrderAmount {
		t.Errorf("replay = %v, first = %v", second, first)
	}
	if second.GetAvailableAfter() != 10_000-testOrderAmount {
		t.Errorf("available_after on replay = %d", second.GetAvailableAfter())
	}
	env.assertBalances(t, userID, 10_000-testOrderAmount, testOrderAmount)
}

func TestReserveReplayOnAFrozenWalletStillSucceeds(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	first := env.reserve(t, limitOrder(userID, orderID))
	exec(t, env.db, `UPDATE wallets SET status = 'FROZEN' WHERE user_id = $1`, userID)

	second := env.reserve(t, limitOrder(userID, orderID))

	if first.GetHoldId() != second.GetHoldId() {
		t.Errorf("hold changed from %s to %s", first.GetHoldId(), second.GetHoldId())
	}
}

func TestReserveConflictsOnDifferentParameters(t *testing.T) {
	tests := []struct {
		name   string
		change func(*billingv1.ReserveCreditsRequest)
	}{
		{"quantity", func(r *billingv1.ReserveCreditsRequest) { r.QtyWh = testQtyWh + 1 }},
		{"price", func(r *billingv1.ReserveCreditsRequest) { r.MaxPricePerKwh = testPrice + 1 }},
		{"order type", func(r *billingv1.ReserveCreditsRequest) { r.OrderType = billingv1.OrderType_ORDER_TYPE_MARKET }},
	}

	env := newReservationEnv(t)
	userID := env.fund(t, 100_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := limitOrder(userID, orderID)
			tt.change(request)

			_, err := env.orders.ReserveCredits(context.Background(), request)

			assertRejected(t, err, codes.AlreadyExists, "IDEMPOTENCY_CONFLICT")
			env.assertBalances(t, userID, 100_000-testOrderAmount, testOrderAmount)
		})
	}

	t.Run("another user", func(t *testing.T) {
		other := env.fund(t, 100_000)

		_, err := env.orders.ReserveCredits(context.Background(), limitOrder(other, orderID))

		assertRejected(t, err, codes.AlreadyExists, "IDEMPOTENCY_CONFLICT")
		env.assertBalances(t, other, 100_000, 0)
	})
}

func TestFeeChangeOnlyAffectsNewReservations(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 100_000)
	openOrder := uuid.New()
	before := env.reserve(t, limitOrder(userID, openOrder))

	exec(t, env.db, `INSERT INTO fee_configs (buyer_fee_bps, seller_fee_bps, effective_from) VALUES (500, 100, now())`)

	replay := env.reserve(t, limitOrder(userID, openOrder))
	if replay.GetReservedAmount() != before.GetReservedAmount() {
		t.Errorf("replay amount changed from %d to %d", before.GetReservedAmount(), replay.GetReservedAmount())
	}

	after := env.reserve(t, limitOrder(userID, uuid.New()))
	// value 4550 + 5% fee 228 (227.5 rounds up) + buffer 50
	if after.GetReservedAmount() != 4828 {
		t.Errorf("new order reserved %d, want 4828", after.GetReservedAmount())
	}

	var lockedBps, newBps int
	ctx := context.Background()
	if err := env.db.QueryRowContext(ctx, `SELECT buyer_fee_bps FROM holds WHERE id = $1`, before.GetHoldId()).Scan(&lockedBps); err != nil {
		t.Fatalf("read locked rate: %v", err)
	}
	if err := env.db.QueryRowContext(ctx, `SELECT buyer_fee_bps FROM holds WHERE id = $1`, after.GetHoldId()).Scan(&newBps); err != nil {
		t.Fatalf("read new rate: %v", err)
	}
	if lockedBps != 100 || newBps != 500 {
		t.Errorf("rates = %d and %d, want 100 and 500", lockedBps, newBps)
	}
}
