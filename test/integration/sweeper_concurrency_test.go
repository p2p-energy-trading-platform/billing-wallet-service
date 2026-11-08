//go:build integration

package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"billing-wallet-service/internal/store/postgres"
	"billing-wallet-service/internal/worker"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
)

// A busy wallet must not stall the sweep, so it skips what it cannot lock at once
// and picks it up on a later run.
func TestSweepSkipsAWalletThatIsLockedAndFinishesLater(t *testing.T) {
	env := newReservationEnv(t)
	busyUser := env.fund(t, 10_000)
	busyOrder := env.reserveExpired(t, busyUser)
	freeUser := env.fund(t, 10_000)
	env.reserveExpired(t, freeUser)
	sweeper := newSweeper(t, env, 100)
	ctx := context.Background()

	tx, err := env.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT id FROM wallets WHERE user_id = $1 FOR UPDATE`, busyUser); err != nil {
		t.Fatalf("lock wallet: %v", err)
	}

	sweepCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stats := sweeper.SweepExpiredHolds(sweepCtx)

	if sweepCtx.Err() != nil {
		t.Fatal("sweep waited on a locked wallet instead of skipping it")
	}
	if stats.Expired != 1 || stats.Skipped != 1 || stats.Failed != 0 {
		t.Errorf("stats = %+v, want one expired and one skipped", stats)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	stats = sweeper.SweepExpiredHolds(ctx)

	if stats.Expired != 1 {
		t.Errorf("later sweep = %+v, want the skipped hold expired", stats)
	}
	if holdStatus, _, _ := env.holdState(t, busyOrder); holdStatus != "EXPIRED" {
		t.Errorf("status = %s, want EXPIRED", holdStatus)
	}
	env.assertBalances(t, busyUser, 10_000, 0)
}

func TestReplicasSweepingTogetherReleaseEachHoldOnce(t *testing.T) {
	env := newReservationEnv(t)
	balance := int64(20 * testOrderAmount)
	userID := env.fund(t, balance)
	for range 20 {
		env.reserveExpired(t, userID)
	}

	const replicas = 5
	sweepers := make([]*worker.Reconciler, replicas)
	for i := range replicas {
		sweepers[i] = newSweeper(t, env, 100)
	}

	var released, failed atomic.Int64
	sweepAll := func() {
		var wg sync.WaitGroup
		for _, sweeper := range sweepers {
			wg.Go(func() {
				stats := sweeper.SweepExpiredHolds(context.Background())
				released.Add(stats.Released)
				failed.Add(int64(stats.Failed))
			})
		}
		wg.Wait()
	}

	// Replicas skip what another holds locked, so it can take a few rounds.
	for range 10 {
		sweepAll()
	}

	if failed.Load() != 0 {
		t.Errorf("%d failures", failed.Load())
	}
	if released.Load() != balance {
		t.Errorf("total released = %d, want %d", released.Load(), balance)
	}
	env.assertBalances(t, userID, balance, 0)
	env.assertReservedMatchesHolds(t, userID)
}

func TestSweepAndOMSReleaseRacingGiveFundsBackOnce(t *testing.T) {
	env := newReservationEnv(t)
	balance := int64(20 * testOrderAmount)
	userID := env.fund(t, balance)
	orderIDs := make([]uuid.UUID, 20)
	for i := range orderIDs {
		orderIDs[i] = env.reserveExpired(t, userID)
	}
	sweeper := newSweeper(t, env, 100)

	var released, failed atomic.Int64
	var wg sync.WaitGroup
	for _, orderID := range orderIDs {
		wg.Go(func() {
			response, err := env.orders.ReleaseReservation(context.Background(), &billingv1.ReleaseReservationRequest{
				OrderId: orderID.String(),
				Reason:  billingv1.ReleaseReason_RELEASE_REASON_CANCELLED,
			})
			if err != nil {
				failed.Add(1)
				return
			}
			released.Add(response.GetReleasedAmount())
		})
	}
	wg.Go(func() {
		for range 5 {
			stats := sweeper.SweepExpiredHolds(context.Background())
			released.Add(stats.Released)
			failed.Add(int64(stats.Failed))
		}
	})
	wg.Wait()

	if failed.Load() != 0 {
		t.Errorf("%d failures", failed.Load())
	}
	if released.Load() != balance {
		t.Errorf("total released = %d, want %d", released.Load(), balance)
	}
	env.assertBalances(t, userID, balance, 0)
	env.assertReservedMatchesHolds(t, userID)
}

func TestSweepAndNewReservationsRacingKeepBalancesConsistent(t *testing.T) {
	env := newReservationEnv(t)
	balance := int64(40 * testOrderAmount)
	userID := env.fund(t, balance)
	for range 20 {
		env.reserveExpired(t, userID)
	}
	sweeper := newSweeper(t, env, 100)

	var reserved atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, uuid.New())); err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			reserved.Add(testOrderAmount)
		})
	}
	wg.Go(func() {
		for range 5 {
			sweeper.SweepExpiredHolds(context.Background())
		}
	})
	wg.Wait()

	// Anything skipped during the race is released now.
	for range 3 {
		sweeper.SweepExpiredHolds(context.Background())
	}

	env.assertBalances(t, userID, balance-reserved.Load(), reserved.Load())
	env.assertReservedMatchesHolds(t, userID)
}

// A sweep lists holds first and locks them later. These cover a change landing in
// between, which the sweep must notice once it holds the locks.
func TestExpireHoldRechecksAfterLocking(t *testing.T) {
	tests := []struct {
		name       string
		change     func(t *testing.T, env *reservationEnv, orderID uuid.UUID)
		wantStatus string
	}{
		{"OMS released it", func(t *testing.T, env *reservationEnv, orderID uuid.UUID) {
			env.release(t, orderID)
		}, "RELEASED"},
		{"OMS extended its expiry", func(t *testing.T, env *reservationEnv, orderID uuid.UUID) {
			exec(t, env.db, `UPDATE holds SET expires_at = now() + interval '1 day' WHERE ref_id = $1`, orderID)
		}, "ACTIVE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newReservationEnv(t)
			userID := env.fund(t, 10_000)
			orderID := env.reserveExpired(t, userID)
			ctx := context.Background()

			pool, err := postgres.NewPool(ctx, env.serviceURL)
			if err != nil {
				t.Fatalf("create pool: %v", err)
			}
			t.Cleanup(pool.Close)
			repo := postgres.NewReservationRepo(pool)
			now := time.Now()

			listed, err := repo.ExpiredOrderHolds(ctx, now, 100)
			if err != nil || len(listed) != 1 || listed[0] != orderID {
				t.Fatalf("listed = %v, %v, want just %v", listed, err, orderID)
			}

			tt.change(t, env, orderID)
			available, reserved := env.balances(t, userID)

			expiry, err := repo.ExpireHold(ctx, orderID, now)

			if err != nil {
				t.Fatalf("ExpireHold: %v", err)
			}
			if expiry.Expired {
				t.Error("expired a hold that no longer qualifies")
			}
			if holdStatus, _, _ := env.holdState(t, orderID); holdStatus != tt.wantStatus {
				t.Errorf("status = %s, want %s", holdStatus, tt.wantStatus)
			}
			env.assertBalances(t, userID, available, reserved)
		})
	}
}
