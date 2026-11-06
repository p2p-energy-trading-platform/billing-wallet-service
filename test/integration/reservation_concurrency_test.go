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

func TestConcurrentReservationsNeverExceedTheBalance(t *testing.T) {
	env := newReservationEnv(t)
	const affordable = 10
	balance := int64(affordable*testOrderAmount + 1)
	userID := env.fund(t, balance)

	const callers = 100
	var succeeded, insufficient atomic.Int64
	var unexpected sync.Map

	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			_, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, uuid.New()))
			switch status.Code(err) {
			case codes.OK:
				succeeded.Add(1)
			case codes.FailedPrecondition:
				insufficient.Add(1)
			default:
				unexpected.Store(err.Error(), true)
			}
		})
	}
	wg.Wait()

	unexpected.Range(func(message, _ any) bool {
		t.Errorf("unexpected error: %v", message)
		return true
	})
	if succeeded.Load() != affordable || insufficient.Load() != callers-affordable {
		t.Errorf("succeeded = %d, insufficient = %d, want %d and %d", succeeded.Load(), insufficient.Load(), affordable, callers-affordable)
	}
	env.assertBalances(t, userID, 1, affordable*testOrderAmount)
	env.assertReservedMatchesHolds(t, userID)
}

func TestConcurrentRetriesOfOneOrderLockFundsOnce(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 100_000)
	orderID := uuid.New()

	const callers = 20
	holdIDs := make([]string, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			response, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, orderID))
			errs[i] = err
			holdIDs[i] = response.GetHoldId()
		})
	}
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if holdIDs[i] != holdIDs[0] {
			t.Errorf("caller %d got hold %q, caller 0 got %q", i, holdIDs[i], holdIDs[0])
		}
	}
	env.assertBalances(t, userID, 100_000-testOrderAmount, testOrderAmount)
}

func TestConcurrentFirstReservesForANewUserAreSerialised(t *testing.T) {
	env := newReservationEnv(t)
	userID := uuid.New()

	const callers = 20
	var wg sync.WaitGroup
	var unexpected sync.Map
	for range callers {
		wg.Go(func() {
			_, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, uuid.New()))
			if status.Code(err) != codes.FailedPrecondition {
				unexpected.Store(status.Code(err).String()+": "+status.Convert(err).Message(), true)
			}
		})
	}
	wg.Wait()

	unexpected.Range(func(message, _ any) bool {
		t.Errorf("unexpected result: %v", message)
		return true
	})
}

func TestConcurrentReleasesGiveFundsBackOnce(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))

	const callers = 20
	var total atomic.Int64
	var failures atomic.Int64
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			response, err := env.orders.ReleaseReservation(context.Background(), &billingv1.ReleaseReservationRequest{
				OrderId: orderID.String(),
				Reason:  billingv1.ReleaseReason_RELEASE_REASON_CANCELLED,
			})
			if err != nil {
				failures.Add(1)
				return
			}
			total.Add(response.GetReleasedAmount())
		})
	}
	wg.Wait()

	if failures.Load() != 0 {
		t.Errorf("%d releases failed", failures.Load())
	}
	if total.Load() != testOrderAmount {
		t.Errorf("total released = %d, want %d", total.Load(), testOrderAmount)
	}
	env.assertBalances(t, userID, 10_000, 0)
}

func TestConcurrentReserveAndReleaseKeepBalancesConsistent(t *testing.T) {
	env := newReservationEnv(t)
	balance := int64(50 * testOrderAmount)
	userID := env.fund(t, balance)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			orderID := uuid.New()
			if _, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, orderID)); err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			if _, err := env.orders.ReleaseReservation(context.Background(), &billingv1.ReleaseReservationRequest{
				OrderId: orderID.String(),
				Reason:  billingv1.ReleaseReason_RELEASE_REASON_CANCELLED,
			}); err != nil {
				t.Errorf("release: %v", err)
			}
		})
	}
	wg.Wait()

	env.assertBalances(t, userID, balance, 0)
	env.assertReservedMatchesHolds(t, userID)
}
