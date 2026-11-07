package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
)

type fakeHoldStore struct {
	mu        sync.Mutex
	orderIDs  []uuid.UUID
	listErr   error
	results   map[uuid.UUID]domain.HoldExpiry
	errs      map[uuid.UUID]error
	expired   []uuid.UUID
	lists     int
	listLimit int
	listNow   time.Time
	onExpire  func()
}

func (f *fakeHoldStore) ExpiredOrderHolds(_ context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	f.listLimit = limit
	f.listNow = now
	return f.orderIDs, f.listErr
}

func (f *fakeHoldStore) ExpireHold(_ context.Context, orderID uuid.UUID, _ time.Time) (domain.HoldExpiry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired = append(f.expired, orderID)
	if f.onExpire != nil {
		f.onExpire()
	}
	return f.results[orderID], f.errs[orderID]
}

func (f *fakeHoldStore) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSweepExpiresEveryListedHold(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	store := &fakeHoldStore{
		orderIDs: []uuid.UUID{a, b},
		results: map[uuid.UUID]domain.HoldExpiry{
			a: {Expired: true, Released: 600},
			b: {Expired: true, Released: 400},
		},
	}
	before := time.Now()

	stats := NewReconciler(store, time.Minute, 25, discard()).SweepExpiredHolds(context.Background())

	if stats != (SweepStats{Expired: 2, Released: 1000}) {
		t.Errorf("stats = %+v", stats)
	}
	if store.listLimit != 25 {
		t.Errorf("limit = %d, want 25", store.listLimit)
	}
	if store.listNow.Before(before) {
		t.Errorf("listed at %v, want now", store.listNow)
	}
}

func TestSweepCountsHoldsItCouldNotLockAsSkipped(t *testing.T) {
	store := &fakeHoldStore{orderIDs: []uuid.UUID{uuid.New()}}

	stats := NewReconciler(store, time.Minute, 25, discard()).SweepExpiredHolds(context.Background())

	if stats != (SweepStats{Skipped: 1}) {
		t.Errorf("stats = %+v", stats)
	}
}

func TestSweepKeepsGoingAfterOneHoldFails(t *testing.T) {
	bad, good := uuid.New(), uuid.New()
	store := &fakeHoldStore{
		orderIDs: []uuid.UUID{bad, good},
		errs:     map[uuid.UUID]error{bad: errors.New("boom")},
		results:  map[uuid.UUID]domain.HoldExpiry{good: {Expired: true, Released: 400}},
	}

	stats := NewReconciler(store, time.Minute, 25, discard()).SweepExpiredHolds(context.Background())

	if stats != (SweepStats{Expired: 1, Released: 400, Failed: 1}) {
		t.Errorf("stats = %+v", stats)
	}
}

func TestSweepSurvivesAListFailure(t *testing.T) {
	store := &fakeHoldStore{listErr: errors.New("database down")}

	stats := NewReconciler(store, time.Minute, 25, discard()).SweepExpiredHolds(context.Background())

	if stats != (SweepStats{Failed: 1}) {
		t.Errorf("stats = %+v", stats)
	}
}

func TestSweepStopsPartWayWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeHoldStore{
		orderIDs: []uuid.UUID{uuid.New(), uuid.New(), uuid.New()},
		onExpire: cancel,
	}

	NewReconciler(store, time.Minute, 25, discard()).SweepExpiredHolds(ctx)

	if len(store.expired) != 1 {
		t.Errorf("expired %d holds after cancel, want 1", len(store.expired))
	}
}

func TestRunSweepsOnEveryTickUntilCancelled(t *testing.T) {
	store := &fakeHoldStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})

	go func() {
		NewReconciler(store, 5*time.Millisecond, 25, discard()).Run(ctx)
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for store.listCount() < 3 {
		select {
		case <-deadline:
			t.Fatalf("swept %d times, want at least 3", store.listCount())
		case <-time.After(time.Millisecond):
		}
	}

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunSweepsImmediatelyInsteadOfWaitingForTheFirstTick(t *testing.T) {
	store := &fakeHoldStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go NewReconciler(store, time.Hour, 25, discard()).Run(ctx)

	deadline := time.After(5 * time.Second)
	for store.listCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("no sweep before the first tick")
		case <-time.After(time.Millisecond):
		}
	}
}
