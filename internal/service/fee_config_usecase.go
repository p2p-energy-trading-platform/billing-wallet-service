package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"billing-wallet-service/internal/domain"
)

type FeeConfigStore interface {
	FindFeeConfigByKey(ctx context.Context, key string) (domain.FeeConfig, error)
	// InsertFeeConfig returns the existing row if another request used the key first.
	InsertFeeConfig(ctx context.Context, request domain.SetFeeConfigRequest) (domain.FeeConfig, error)
}

type FeeCacheInvalidator interface {
	Invalidate()
}

type FeeConfigUsecase struct {
	store FeeConfigStore
	cache FeeCacheInvalidator
}

func NewFeeConfigUsecase(store FeeConfigStore, cache FeeCacheInvalidator) *FeeConfigUsecase {
	return &FeeConfigUsecase{store: store, cache: cache}
}

// SetFeeConfig adds a new fee configuration and never edits an old one. Open buy
// orders keep the buyer rate locked in their hold.
func (u *FeeConfigUsecase) SetFeeConfig(ctx context.Context, request domain.SetFeeConfigRequest) (domain.FeeConfig, error) {
	if err := request.Validate(); err != nil {
		return domain.FeeConfig{}, err
	}

	// The replay check comes first: a retry whose start time has since passed is
	// still the same request.
	existing, err := u.store.FindFeeConfigByKey(ctx, request.IdempotencyKey)
	if err == nil {
		return u.finish(existing, request)
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return domain.FeeConfig{}, err
	}

	now := time.Now().Truncate(time.Microsecond)
	insert := request
	switch {
	case request.EffectiveFrom == nil:
		insert.EffectiveFrom = &now
	case request.EffectiveFrom.Before(now):
		// A past start would rewrite which rate applied to trades already settled.
		return domain.FeeConfig{}, domain.NewValidationError("effective_from", "must be in the future")
	}

	config, err := u.store.InsertFeeConfig(ctx, insert)
	if err != nil {
		return domain.FeeConfig{}, err
	}

	return u.finish(config, request)
}

// finish clears the cached rate on every success. A retry may be the first success
// this replica sees, if the original attempt committed but failed on the way back.
func (u *FeeConfigUsecase) finish(config domain.FeeConfig, request domain.SetFeeConfigRequest) (domain.FeeConfig, error) {
	if !config.MatchesRequest(request) {
		return domain.FeeConfig{}, fmt.Errorf("fee config key %q: %w", request.IdempotencyKey, domain.ErrIdempotencyConflict)
	}

	u.cache.Invalidate()

	return config, nil
}
