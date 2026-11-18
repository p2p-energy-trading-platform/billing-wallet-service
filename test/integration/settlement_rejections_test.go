//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSettleRejections(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, env *reservationEnv, buyer, seller, order uuid.UUID) *billingv1.SettleTradeRequest
		wantCode   codes.Code
		wantReason string
	}{
		{"fill above the reserved price", func(_ *testing.T, _ *reservationEnv, buyer, seller, order uuid.UUID) *billingv1.SettleTradeRequest {
			return fill(buyer, seller, order, 1000, testPrice+1, false)
		}, codes.FailedPrecondition, "PRICE_EXCEEDS_RESERVATION"},
		{"more than the hold can cover", func(_ *testing.T, _ *reservationEnv, buyer, seller, order uuid.UUID) *billingv1.SettleTradeRequest {
			return fill(buyer, seller, order, 1100, testPrice, false)
		}, codes.FailedPrecondition, "HOLD_INSUFFICIENT_REMAINING"},
		{"unknown buy order", func(_ *testing.T, _ *reservationEnv, buyer, seller, _ uuid.UUID) *billingv1.SettleTradeRequest {
			return fill(buyer, seller, uuid.New(), 1000, testPrice, false)
		}, codes.NotFound, "HOLD_NOT_FOUND"},
		{"buyer with no wallet", func(_ *testing.T, _ *reservationEnv, _, seller, order uuid.UUID) *billingv1.SettleTradeRequest {
			return fill(uuid.New(), seller, order, 1000, testPrice, false)
		}, codes.NotFound, "HOLD_NOT_FOUND"},
		{"someone else's buy order", func(t *testing.T, env *reservationEnv, _, seller, order uuid.UUID) *billingv1.SettleTradeRequest {
			return fill(env.fund(t, 0), seller, order, 1000, testPrice, false)
		}, codes.NotFound, "HOLD_NOT_FOUND"},
		{"released hold", func(t *testing.T, env *reservationEnv, buyer, seller, order uuid.UUID) *billingv1.SettleTradeRequest {
			env.release(t, order)
			return fill(buyer, seller, order, 1000, testPrice, false)
		}, codes.NotFound, "HOLD_NOT_FOUND"},
		{"self trade", func(_ *testing.T, _ *reservationEnv, buyer, _, order uuid.UUID) *billingv1.SettleTradeRequest {
			return fill(buyer, buyer, order, 1000, testPrice, false)
		}, codes.InvalidArgument, "VALIDATION_FAILED"},
		{"zero quantity", func(_ *testing.T, _ *reservationEnv, buyer, seller, order uuid.UUID) *billingv1.SettleTradeRequest {
			return fill(buyer, seller, order, 0, testPrice, false)
		}, codes.InvalidArgument, "BAD_AMOUNT"},
		{"trade worth less than one unit", func(_ *testing.T, _ *reservationEnv, buyer, seller, order uuid.UUID) *billingv1.SettleTradeRequest {
			return fill(buyer, seller, order, 1, 1, false)
		}, codes.InvalidArgument, "BAD_AMOUNT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newReservationEnv(t)
			buyer, order := env.buyOrder(t, 10_000)
			seller := env.fund(t, 0)
			request := tt.setup(t, env, buyer, seller, order)
			available, reserved := env.balances(t, buyer)

			_, err := env.orders.SettleTrade(context.Background(), request)

			assertRejected(t, err, tt.wantCode, tt.wantReason)
			env.assertBalances(t, buyer, available, reserved)
			env.assertBalances(t, seller, 0, 0)
			if n := env.count(t, `SELECT count(*) FROM journals`); n != 0 {
				t.Errorf("journals = %d, want none", n)
			}
		})
	}
}

func TestSettleReplayReturnsTheOriginalResult(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	seller := env.fund(t, 0)
	request := fill(buyer, seller, order, 1000, testPrice, true)

	first := env.settle(t, request)
	second := env.settle(t, request)

	if !second.GetAlreadySettled() || first.GetAlreadySettled() {
		t.Errorf("already_settled = %v then %v", first.GetAlreadySettled(), second.GetAlreadySettled())
	}
	if second.GetSettlementId() != first.GetSettlementId() || second.GetTradeValue() != first.GetTradeValue() ||
		second.GetBuyerFee() != first.GetBuyerFee() || second.GetSellerFee() != first.GetSellerFee() ||
		second.GetBuyerReleasedRemainder() != first.GetBuyerReleasedRemainder() {
		t.Errorf("replay = %v, first = %v", second, first)
	}
	env.assertBalances(t, seller, 4504, 0)
	if n := env.count(t, `SELECT count(*) FROM journals`); n != 1 {
		t.Errorf("journals = %d, want 1", n)
	}
}

func TestSettleConflictsWhenATradeIDIsReusedDifferently(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	seller := env.fund(t, 0)
	request := fill(buyer, seller, order, 500, testPrice, false)
	env.settle(t, request)

	changed := fill(buyer, seller, order, 400, testPrice, false)
	changed.TradeId = request.GetTradeId()
	_, err := env.orders.SettleTrade(context.Background(), changed)

	assertRejected(t, err, codes.AlreadyExists, "IDEMPOTENCY_CONFLICT")
	if n := env.count(t, `SELECT count(*) FROM journals`); n != 1 {
		t.Errorf("journals = %d, want 1", n)
	}
}

func TestSettleRetryThatBecomesTheFinalFillIsAConflict(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	seller := env.fund(t, 0)
	request := fill(buyer, seller, order, 1000, testPrice, false)
	env.settle(t, request)

	request.BuyOrderFinal = true
	_, err := env.orders.SettleTrade(context.Background(), request)

	assertRejected(t, err, codes.AlreadyExists, "IDEMPOTENCY_CONFLICT")
}

// A retry carries the original executed_at, which may be before any fee config
// that exists now. It still has to return the stored result.
func TestSettleReplayWorksWithoutAnActiveRateAtItsExecutionTime(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	request := fill(buyer, env.fund(t, 0), order, 1000, testPrice, true)
	first := env.settle(t, request)

	request.ExecutedAt = timestamppb.New(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	replay := env.settle(t, request)

	if !replay.GetAlreadySettled() || replay.GetSettlementId() != first.GetSettlementId() {
		t.Errorf("replay = %v, want the original", replay)
	}
}

func TestSettlementRecordsAreAppendOnly(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	env.settle(t, fill(buyer, env.fund(t, 0), order, 1000, testPrice, true))

	for _, statement := range []string{`UPDATE ledger_lines SET amount = 1`, `DELETE FROM journals`} {
		if _, err := env.db.ExecContext(context.Background(), statement); err == nil {
			t.Errorf("%q succeeded, want it refused", statement)
		}
	}
	env.assertBooksBalance(t, 10_000)
}
