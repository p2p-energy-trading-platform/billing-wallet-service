//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc/codes"
)

func TestReleaseReturnsTheFunds(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))

	released := env.release(t, orderID)

	if released != testOrderAmount {
		t.Errorf("released = %d, want %d", released, testOrderAmount)
	}
	env.assertBalances(t, userID, 10_000, 0)
	env.assertReservedMatchesHolds(t, userID)

	var holdStatus string
	var amountReleased int64
	err := env.db.QueryRowContext(context.Background(),
		`SELECT status, amount_released FROM holds WHERE ref_id = $1`, orderID).Scan(&holdStatus, &amountReleased)
	if err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if holdStatus != "RELEASED" || amountReleased != testOrderAmount {
		t.Errorf("hold = %s with %d released", holdStatus, amountReleased)
	}
}

func TestReleaseReplayReturnsZero(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))
	env.release(t, orderID)

	if released := env.release(t, orderID); released != 0 {
		t.Errorf("second release = %d, want 0", released)
	}
	env.assertBalances(t, userID, 10_000, 0)
}

func TestReleaseAfterAPartialFillReturnsOnlyTheRemainder(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))
	env.simulateFill(t, userID, orderID, 1000)

	released := env.release(t, orderID)

	if released != testOrderAmount-1000 {
		t.Errorf("released = %d, want %d", released, testOrderAmount-1000)
	}
	env.assertBalances(t, userID, 10_000-1000, 0)
	env.assertReservedMatchesHolds(t, userID)
}

func TestReleaseAfterAFullFillConsumesTheHold(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))
	env.simulateFill(t, userID, orderID, testOrderAmount)

	if released := env.release(t, orderID); released != 0 {
		t.Errorf("released = %d, want 0", released)
	}

	var holdStatus string
	if err := env.db.QueryRowContext(context.Background(), `SELECT status FROM holds WHERE ref_id = $1`, orderID).Scan(&holdStatus); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if holdStatus != "CONSUMED" {
		t.Errorf("status = %s, want CONSUMED", holdStatus)
	}
	env.assertBalances(t, userID, 10_000-testOrderAmount, 0)
}

func TestReleaseStillWorksOnAFrozenWallet(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))
	exec(t, env.db, `UPDATE wallets SET status = 'FROZEN' WHERE user_id = $1`, userID)

	if released := env.release(t, orderID); released != testOrderAmount {
		t.Errorf("released = %d, want %d", released, testOrderAmount)
	}
	env.assertBalances(t, userID, 10_000, 0)
}

func TestReleaseRejections(t *testing.T) {
	env := newReservationEnv(t)
	ctx := context.Background()

	_, err := env.orders.ReleaseReservation(ctx, &billingv1.ReleaseReservationRequest{
		OrderId: uuid.NewString(),
		Reason:  billingv1.ReleaseReason_RELEASE_REASON_CANCELLED,
	})
	assertRejected(t, err, codes.NotFound, "HOLD_NOT_FOUND")

	_, err = env.orders.ReleaseReservation(ctx, &billingv1.ReleaseReservationRequest{
		OrderId: "nope",
		Reason:  billingv1.ReleaseReason_RELEASE_REASON_CANCELLED,
	})
	assertRejected(t, err, codes.InvalidArgument, "VALIDATION_FAILED")

	_, err = env.orders.ReleaseReservation(ctx, &billingv1.ReleaseReservationRequest{OrderId: uuid.NewString()})
	assertRejected(t, err, codes.InvalidArgument, "VALIDATION_FAILED")
}

func TestReleaseOnlyAppliesToOrderHolds(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	withdrawalID := uuid.New()
	exec(t, env.db,
		`INSERT INTO holds (wallet_id, kind, ref_id, amount_total, status)
		 SELECT id, 'WITHDRAWAL', $1, 100, 'ACTIVE' FROM wallets WHERE user_id = $2`, withdrawalID, userID)

	_, err := env.orders.ReleaseReservation(context.Background(), &billingv1.ReleaseReservationRequest{
		OrderId: withdrawalID.String(),
		Reason:  billingv1.ReleaseReason_RELEASE_REASON_CANCELLED,
	})

	assertRejected(t, err, codes.NotFound, "HOLD_NOT_FOUND")
}
