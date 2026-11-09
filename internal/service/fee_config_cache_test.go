package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
)

type fakeFeeRates struct {
	rows    []domain.FeeConfig // newest effective_from last
	err     error
	nextErr error
	calls   int
}

func (f *fakeFeeRates) ActiveFeeConfig(_ context.Context, at time.Time) (domain.FeeConfig, error) {
	f.calls++
	if f.err != nil {
		return domain.FeeConfig{}, f.err
	}
	for i := len(f.rows) - 1; i >= 0; i-- {
		if !f.rows[i].EffectiveFrom.After(at) {
			return f.rows[i], nil
		}
	}
	return domain.FeeConfig{}, errors.New("no active fee config")
}

func (f *fakeFeeRates) NextFeeConfigStart(_ context.Context, after time.Time) (*time.Time, error) {
	if f.nextErr != nil {
		return nil, f.nextErr
	}
	var next *time.Time
	for i := range f.rows {
		start := f.rows[i].EffectiveFrom
		if start.After(after) && (next == nil || start.Before(*next)) {
			next = &start
		}
	}
	return next, nil
}

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

var cacheStart = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func newTestCache(rates FeeRateSource) (*FeeConfigCache, *testClock) {
	clock := &testClock{now: cacheStart}
	cache := NewFeeConfigCache(rates, DefaultFeeConfigCacheTTL)
	cache.now = clock.Now
	return cache, clock
}

func row(buyerBps int32, effective time.Time) domain.FeeConfig {
	return domain.FeeConfig{ID: uuid.New(), BuyerFeeBps: buyerBps, EffectiveFrom: effective}
}

func TestCacheServesRepeatLookupsWithoutTheStore(t *testing.T) {
	rates := &fakeFeeRates{rows: []domain.FeeConfig{row(100, cacheStart.Add(-time.Hour))}}
	cache, clock := newTestCache(rates)
	ctx := context.Background()

	for range 5 {
		fee, err := cache.ActiveFeeConfig(ctx, clock.now)
		if err != nil || fee.BuyerFeeBps != 100 {
			t.Fatalf("fee = %+v, err = %v", fee, err)
		}
		clock.now = clock.now.Add(time.Second)
	}

	if rates.calls != 1 {
		t.Errorf("store called %d times, want 1", rates.calls)
	}
}

func TestCacheExpiresAfterTheTTL(t *testing.T) {
	rates := &fakeFeeRates{rows: []domain.FeeConfig{row(100, cacheStart.Add(-time.Hour))}}
	cache, clock := newTestCache(rates)
	ctx := context.Background()
	if _, err := cache.ActiveFeeConfig(ctx, clock.now); err != nil {
		t.Fatal(err)
	}

	rates.rows = append(rates.rows, row(500, cacheStart.Add(time.Second)))

	clock.now = cacheStart.Add(DefaultFeeConfigCacheTTL - time.Millisecond)
	if fee, _ := cache.ActiveFeeConfig(ctx, clock.now); fee.BuyerFeeBps != 100 {
		t.Errorf("before the TTL: rate = %d, want the cached 100", fee.BuyerFeeBps)
	}

	clock.now = cacheStart.Add(DefaultFeeConfigCacheTTL)
	if fee, _ := cache.ActiveFeeConfig(ctx, clock.now); fee.BuyerFeeBps != 500 {
		t.Errorf("at the TTL: rate = %d, want the new 500", fee.BuyerFeeBps)
	}
}

func TestCacheInvalidateForcesAReload(t *testing.T) {
	rates := &fakeFeeRates{rows: []domain.FeeConfig{row(100, cacheStart.Add(-time.Hour))}}
	cache, clock := newTestCache(rates)
	ctx := context.Background()
	if _, err := cache.ActiveFeeConfig(ctx, clock.now); err != nil {
		t.Fatal(err)
	}

	rates.rows = append(rates.rows, row(500, cacheStart))
	cache.Invalidate()

	if fee, _ := cache.ActiveFeeConfig(ctx, clock.now); fee.BuyerFeeBps != 500 {
		t.Errorf("rate = %d, want 500", fee.BuyerFeeBps)
	}
}

// A settlement asks for the rate at its executed_at, which can be before the
// cached row took effect. Serving the cached row would charge the wrong rate.
func TestCacheGoesToTheStoreForATimeBeforeTheCachedRow(t *testing.T) {
	old := row(100, cacheStart.Add(-2*time.Hour))
	current := row(500, cacheStart.Add(-time.Hour))
	rates := &fakeFeeRates{rows: []domain.FeeConfig{old, current}}
	cache, clock := newTestCache(rates)
	ctx := context.Background()
	if fee, _ := cache.ActiveFeeConfig(ctx, clock.now); fee.ID != current.ID {
		t.Fatalf("fee = %+v, want current", fee)
	}

	fee, err := cache.ActiveFeeConfig(ctx, cacheStart.Add(-90*time.Minute))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fee.ID != old.ID {
		t.Errorf("rate = %d, want the older 100", fee.BuyerFeeBps)
	}
}

func TestCacheDoesNotRememberAnOldLookup(t *testing.T) {
	old := row(100, cacheStart.Add(-2*time.Hour))
	current := row(500, cacheStart.Add(-time.Hour))
	rates := &fakeFeeRates{rows: []domain.FeeConfig{old, current}}
	cache, clock := newTestCache(rates)
	ctx := context.Background()
	if _, err := cache.ActiveFeeConfig(ctx, cacheStart.Add(-90*time.Minute)); err != nil {
		t.Fatal(err)
	}

	fee, err := cache.ActiveFeeConfig(ctx, clock.now)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fee.ID != current.ID {
		t.Errorf("rate = %d, want the current 500", fee.BuyerFeeBps)
	}
}

func TestCacheDoesNotRememberFailures(t *testing.T) {
	rates := &fakeFeeRates{err: domain.ErrUnavailable}
	cache, clock := newTestCache(rates)
	ctx := context.Background()

	if _, err := cache.ActiveFeeConfig(ctx, clock.now); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}

	rates.err = nil
	rates.rows = []domain.FeeConfig{row(100, cacheStart.Add(-time.Hour))}

	fee, err := cache.ActiveFeeConfig(ctx, clock.now)
	if err != nil || fee.BuyerFeeBps != 100 {
		t.Errorf("after recovery: fee = %+v, err = %v", fee, err)
	}
}

func TestCacheSwitchesAtAScheduledStartWithoutWaitingForTheTTL(t *testing.T) {
	current := row(100, cacheStart.Add(-time.Hour))
	scheduled := row(500, cacheStart.Add(10*time.Second))
	rates := &fakeFeeRates{rows: []domain.FeeConfig{current, scheduled}}
	cache, clock := newTestCache(rates)
	ctx := context.Background()

	if fee, _ := cache.ActiveFeeConfig(ctx, clock.now); fee.ID != current.ID {
		t.Fatalf("before the start: fee = %+v", fee)
	}

	clock.now = cacheStart.Add(9 * time.Second)
	if fee, _ := cache.ActiveFeeConfig(ctx, clock.now); fee.ID != current.ID {
		t.Errorf("just before the start: rate = %d, want 100", fee.BuyerFeeBps)
	}

	clock.now = cacheStart.Add(10 * time.Second)
	if fee, _ := cache.ActiveFeeConfig(ctx, clock.now); fee.ID != scheduled.ID {
		t.Errorf("at the start: rate = %d, want 500", fee.BuyerFeeBps)
	}
}

// A request that read the clock before a rate change must not put the old rate
// back in the cache after that change invalidated it.
func TestCacheDoesNotRememberALookupFromBeforeAnInvalidate(t *testing.T) {
	old := row(100, cacheStart.Add(-time.Hour))
	rates := &fakeFeeRates{rows: []domain.FeeConfig{old}}
	cache, clock := newTestCache(rates)
	ctx := context.Background()
	stalledAt := clock.now

	clock.now = cacheStart.Add(time.Second)
	updated := row(500, cacheStart.Add(time.Millisecond))
	rates.rows = append(rates.rows, updated)
	cache.Invalidate()

	if fee, _ := cache.ActiveFeeConfig(ctx, stalledAt); fee.ID != old.ID {
		t.Fatalf("stalled lookup: fee = %+v, want the rate at its own time", fee)
	}

	if fee, _ := cache.ActiveFeeConfig(ctx, clock.now); fee.ID != updated.ID {
		t.Errorf("next lookup: rate = %d, want the new 500", fee.BuyerFeeBps)
	}
}

func TestCacheStillAnswersWhenTheNextStartLookupFails(t *testing.T) {
	current := row(100, cacheStart.Add(-time.Hour))
	rates := &fakeFeeRates{rows: []domain.FeeConfig{current}, nextErr: domain.ErrUnavailable}
	cache, clock := newTestCache(rates)
	ctx := context.Background()

	fee, err := cache.ActiveFeeConfig(ctx, clock.now)
	if err != nil || fee.ID != current.ID {
		t.Fatalf("fee = %+v, err = %v", fee, err)
	}

	if _, err := cache.ActiveFeeConfig(ctx, clock.now); err != nil {
		t.Fatal(err)
	}
	if rates.calls != 2 {
		t.Errorf("store called %d times, want 2: without the next start it must not cache", rates.calls)
	}
}
