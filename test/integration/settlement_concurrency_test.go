//go:build integration

package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestConcurrentFillsOnOneOrderAllSettle(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	seller := env.fund(t, 0)

	// 10 fills of 100 Wh: each 455 + 5 fee (4.55 rounds up) = 460 paid
	var wg sync.WaitGroup
	var failed atomic.Int64
	for range 10 {
		wg.Go(func() {
			if _, err := env.orders.SettleTrade(context.Background(), fill(buyer, seller, order, 100, testPrice, false)); err != nil {
				t.Errorf("settle: %v", err)
				failed.Add(1)
			}
		})
	}
	wg.Wait()

	if failed.Load() != 0 {
		return
	}
	env.assertBalances(t, buyer, 10_000-testOrderAmount, testOrderAmount-4600)
	env.assertBalances(t, seller, 10*(455-5), 0)
	env.assertReservedMatchesHolds(t, buyer)
	env.assertBooksBalance(t, 10_000)
}

func TestConcurrentRetriesOfOneTradeSettleOnce(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	seller := env.fund(t, 0)
	request := fill(buyer, seller, order, 1000, testPrice, true)

	const callers = 20
	ids := make([]string, callers)
	var fresh atomic.Int64
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			response, err := env.orders.SettleTrade(context.Background(), request)
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
				return
			}
			ids[i] = response.GetSettlementId()
			if !response.GetAlreadySettled() {
				fresh.Add(1)
			}
		})
	}
	wg.Wait()

	if fresh.Load() != 1 {
		t.Errorf("%d callers settled it, want exactly 1", fresh.Load())
	}
	for i := range callers {
		if ids[i] != ids[0] {
			t.Errorf("caller %d got settlement %q, caller 0 got %q", i, ids[i], ids[0])
		}
	}
	env.assertBalances(t, seller, 4504, 0)
	env.assertBooksBalance(t, 10_000)
}

// Two traders buying from each other at once lock the same two wallets in opposite
// roles. Lock ordering must stop that from deadlocking.
func TestTradersBuyingFromEachOtherDoNotDeadlock(t *testing.T) {
	env := newReservationEnv(t)
	alice, bob := env.fund(t, 50_000), env.fund(t, 50_000)
	var aliceOrders, bobOrders []uuid.UUID
	for range 10 {
		a, b := uuid.New(), uuid.New()
		env.reserve(t, limitOrder(alice, a))
		env.reserve(t, limitOrder(bob, b))
		aliceOrders, bobOrders = append(aliceOrders, a), append(bobOrders, b)
	}

	var wg sync.WaitGroup
	var unexpected sync.Map
	for i := range 10 {
		for _, request := range []*billingv1.SettleTradeRequest{
			fill(alice, bob, aliceOrders[i], 1000, testPrice, true),
			fill(bob, alice, bobOrders[i], 1000, testPrice, true),
		} {
			wg.Go(func() {
				if _, err := env.orders.SettleTrade(context.Background(), request); err != nil {
					unexpected.Store(status.Code(err).String()+": "+status.Convert(err).Message(), true)
				}
			})
		}
	}
	wg.Wait()

	unexpected.Range(func(message, _ any) bool {
		t.Errorf("unexpected error: %v", message)
		return true
	})
	// each paid 10 x 4596 and received 10 x 4504
	env.assertBalances(t, alice, 50_000-45_960+45_040, 0)
	env.assertBalances(t, bob, 50_000-45_960+45_040, 0)
	env.assertBooksBalance(t, 100_000)
}

func TestSettlementRacingAReleaseStaysConsistent(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	seller := env.fund(t, 0)

	var settleErr error
	var released int64
	var wg sync.WaitGroup
	wg.Go(func() {
		_, settleErr = env.orders.SettleTrade(context.Background(), fill(buyer, seller, order, 1000, testPrice, false))
	})
	wg.Go(func() { released = env.release(t, order) })
	wg.Wait()

	// Whichever ran first, every credit is accounted for exactly once.
	switch {
	case settleErr == nil:
		env.assertBalances(t, buyer, 10_000-4596, 0)
		env.assertBalances(t, seller, 4504, 0)
		if released != 50 {
			t.Errorf("released = %d after the fill, want 50", released)
		}
	case status.Code(settleErr) == codes.NotFound:
		env.assertBalances(t, buyer, 10_000, 0)
		env.assertBalances(t, seller, 0, 0)
	default:
		t.Fatalf("settle: %v", settleErr)
	}
	env.assertReservedMatchesHolds(t, buyer)
	env.assertBooksBalance(t, 10_000)
}
