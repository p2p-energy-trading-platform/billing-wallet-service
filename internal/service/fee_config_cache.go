package service

import (
	"context"
	"sync"
	"time"

	"billing-wallet-service/internal/domain"
)

// DefaultFeeConfigCacheTTL bounds how long a replica can keep using a replaced rate.
const DefaultFeeConfigCacheTTL = 30 * time.Second

// FeeRates looks up the fee config in effect at a given time.
type FeeRates = domain.FeeRates

// FeeRateSource is what the cache reads from.
type FeeRateSource interface {
	FeeRates
	// NextFeeConfigStart returns the earliest start after the given time, if any.
	NextFeeConfigStart(ctx context.Context, after time.Time) (*time.Time, error)
}

// FeeConfigCache keeps the active fee config in process. A rate change made through
// this replica applies at once, and on other replicas within the TTL. A rate already
// scheduled when the cache loaded applies exactly at its start.
type FeeConfigCache struct {
	rates FeeRateSource
	ttl   time.Duration
	now   func() time.Time

	mu        sync.Mutex
	cached    domain.FeeConfig
	nextStart *time.Time
	fetchedAt time.Time
	valid     bool
}

func NewFeeConfigCache(rates FeeRateSource, ttl time.Duration) *FeeConfigCache {
	return &FeeConfigCache{rates: rates, ttl: ttl, now: time.Now}
}

// ActiveFeeConfig returns the config in effect at the given time. Lookups for a time
// before the cached row took effect, such as a settlement of an older trade, go to
// the store, because the cached row was not yet the active one then.
func (c *FeeConfigCache) ActiveFeeConfig(ctx context.Context, at time.Time) (domain.FeeConfig, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if c.valid && now.Sub(c.fetchedAt) < c.ttl && c.covers(at) {
		return c.cached, nil
	}

	fee, err := c.rates.ActiveFeeConfig(ctx, at)
	if err != nil {
		return domain.FeeConfig{}, err
	}

	// Only a current lookup says what is active now.
	if now.Sub(at) >= c.ttl {
		return fee, nil
	}

	nextStart, err := c.rates.NextFeeConfigStart(ctx, at)
	if err != nil {
		// The rate is still right. It just cannot be cached without knowing when it ends.
		return fee, nil
	}

	c.cached, c.nextStart, c.fetchedAt, c.valid = fee, nextStart, now, true

	return fee, nil
}

// covers reports whether the cached row was the active one at the given time.
func (c *FeeConfigCache) covers(at time.Time) bool {
	return !at.Before(c.cached.EffectiveFrom) && (c.nextStart == nil || at.Before(*c.nextStart))
}

// Invalidate makes the next lookup read the store.
func (c *FeeConfigCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.valid = false
}
