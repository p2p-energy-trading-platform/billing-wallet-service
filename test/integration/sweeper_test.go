//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"

	"billing-wallet-service/internal/worker"

	"github.com/google/uuid"
)

func TestSweepReleasesAnExpiredHold(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := env.reserveExpired(t, userID)

	stats := newSweeper(t, env, 100).SweepExpiredHolds(context.Background())

	if stats != (worker.SweepStats{Expired: 1, Released: testOrderAmount}) {
		t.Errorf("stats = %+v", stats)
	}
	env.assertBalances(t, userID, 10_000, 0)
	env.assertReservedMatchesHolds(t, userID)

	holdStatus, consumed, released := env.holdState(t, orderID)
	if holdStatus != "EXPIRED" || consumed != 0 || released != testOrderAmount {
		t.Errorf("hold = %s, consumed %d, released %d", holdStatus, consumed, released)
	}
}

func TestSweepPublishesABalanceEventWithTheExpiredReason(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := env.reserveExpired(t, userID)

	newSweeper(t, env, 100).SweepExpiredHolds(context.Background())

	var payload []byte
	err := env.db.QueryRowContext(context.Background(),
		`SELECT payload FROM outbox WHERE key = $1 ORDER BY id DESC LIMIT 1`, userID.String()).Scan(&payload)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}

	var event struct {
		Reason    string `json:"reason"`
		OrderID   string `json:"order_id"`
		Available int64  `json:"available"`
		Reserved  int64  `json:"reserved"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if event.Reason != "EXPIRED" || event.OrderID != orderID.String() || event.Available != 10_000 || event.Reserved != 0 {
		t.Errorf("event = %+v", event)
	}
}

func TestSweepLeavesHoldsThatMustNotExpire(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 50_000)

	noExpiry := uuid.New()
	env.reserve(t, limitOrder(userID, noExpiry))

	notYet := uuid.New()
	env.reserve(t, limitOrder(userID, notYet))
	exec(t, env.db, `UPDATE holds SET expires_at = now() + interval '1 hour' WHERE ref_id = $1`, notYet)

	released := env.reserveExpired(t, userID)
	env.release(t, released)

	withdrawal := uuid.New()
	exec(t, env.db,
		`INSERT INTO holds (wallet_id, kind, ref_id, amount_total, status, expires_at)
		 SELECT id, 'WITHDRAWAL', $1, 100, 'ACTIVE', now() - interval '1 hour' FROM wallets WHERE user_id = $2`,
		withdrawal, userID)
	exec(t, env.db, `UPDATE wallets SET available = available - 100, reserved = reserved + 100 WHERE user_id = $1`, userID)

	available, reserved := env.balances(t, userID)

	stats := newSweeper(t, env, 100).SweepExpiredHolds(context.Background())

	if stats != (worker.SweepStats{}) {
		t.Errorf("stats = %+v, want nothing swept", stats)
	}
	env.assertBalances(t, userID, available, reserved)

	for name, orderID := range map[string]uuid.UUID{"no expiry": noExpiry, "not yet": notYet} {
		if holdStatus, _, _ := env.holdState(t, orderID); holdStatus != "ACTIVE" {
			t.Errorf("%s: status = %s, want ACTIVE", name, holdStatus)
		}
	}
	var withdrawalStatus string
	err := env.db.QueryRowContext(context.Background(), `SELECT status FROM holds WHERE ref_id = $1`, withdrawal).Scan(&withdrawalStatus)
	if err != nil {
		t.Fatalf("read withdrawal hold: %v", err)
	}
	if withdrawalStatus != "ACTIVE" {
		t.Errorf("withdrawal hold status = %s, want ACTIVE", withdrawalStatus)
	}
}

func TestSweepReturnsOnlyTheRemainderOfAPartlyFilledHold(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := env.reserveExpired(t, userID)
	env.simulateFill(t, userID, orderID, 1000)

	stats := newSweeper(t, env, 100).SweepExpiredHolds(context.Background())

	if stats.Released != testOrderAmount-1000 {
		t.Errorf("released = %d, want %d", stats.Released, testOrderAmount-1000)
	}
	env.assertBalances(t, userID, 10_000-1000, 0)
	env.assertReservedMatchesHolds(t, userID)
	if holdStatus, _, _ := env.holdState(t, orderID); holdStatus != "EXPIRED" {
		t.Errorf("status = %s, want EXPIRED", holdStatus)
	}
}

func TestSweepMarksAFullyFilledHoldConsumed(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := env.reserveExpired(t, userID)
	env.simulateFill(t, userID, orderID, testOrderAmount)

	stats := newSweeper(t, env, 100).SweepExpiredHolds(context.Background())

	if stats.Expired != 1 || stats.Released != 0 {
		t.Errorf("stats = %+v", stats)
	}
	env.assertBalances(t, userID, 10_000-testOrderAmount, 0)
	if holdStatus, _, _ := env.holdState(t, orderID); holdStatus != "CONSUMED" {
		t.Errorf("status = %s, want CONSUMED", holdStatus)
	}

	var events int
	err := env.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM outbox WHERE key = $1`, userID.String()).Scan(&events)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("events = %d, want only the reservation's: the sweep moved no money", events)
	}
}

func TestSweepReturnsFundsOnAFrozenWallet(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	env.reserveExpired(t, userID)
	exec(t, env.db, `UPDATE wallets SET status = 'FROZEN' WHERE user_id = $1`, userID)

	newSweeper(t, env, 100).SweepExpiredHolds(context.Background())

	env.assertBalances(t, userID, 10_000, 0)
}

func TestSweepingAgainChangesNothing(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	env.reserveExpired(t, userID)
	sweeper := newSweeper(t, env, 100)
	sweeper.SweepExpiredHolds(context.Background())

	stats := sweeper.SweepExpiredHolds(context.Background())

	if stats != (worker.SweepStats{}) {
		t.Errorf("second sweep = %+v, want nothing", stats)
	}
	env.assertBalances(t, userID, 10_000, 0)
}

func TestSweepHonoursTheBatchSizeAndFinishesOnLaterRuns(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 50_000)
	for range 5 {
		env.reserveExpired(t, userID)
	}
	sweeper := newSweeper(t, env, 2)
	ctx := context.Background()

	if stats := sweeper.SweepExpiredHolds(ctx); stats.Expired != 2 {
		t.Errorf("first sweep expired %d, want 2", stats.Expired)
	}
	if stats := sweeper.SweepExpiredHolds(ctx); stats.Expired != 2 {
		t.Errorf("second sweep expired %d, want 2", stats.Expired)
	}
	if stats := sweeper.SweepExpiredHolds(ctx); stats.Expired != 1 {
		t.Errorf("third sweep expired %d, want 1", stats.Expired)
	}
	env.assertBalances(t, userID, 50_000, 0)
}

func TestAfterExpiryTheOrderAPIStaysConsistent(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := env.reserveExpired(t, userID)
	newSweeper(t, env, 100).SweepExpiredHolds(context.Background())

	if released := env.release(t, orderID); released != 0 {
		t.Errorf("release after expiry = %d, want 0", released)
	}
	replay := env.reserve(t, limitOrder(userID, orderID))
	if replay.GetReservedAmount() != testOrderAmount {
		t.Errorf("replay amount = %d", replay.GetReservedAmount())
	}
	env.assertBalances(t, userID, 10_000, 0)

	// The returned funds can be spent again by a new order.
	env.reserve(t, limitOrder(userID, uuid.New()))
	env.assertBalances(t, userID, 10_000-testOrderAmount, testOrderAmount)
}
