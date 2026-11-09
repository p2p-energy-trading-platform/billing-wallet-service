package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
)

type fakeFeeConfigStore struct {
	existing    *domain.FeeConfig
	findErr     error
	insertErr   error
	inserted    []domain.SetFeeConfigRequest
	insertReply *domain.FeeConfig
}

func (f *fakeFeeConfigStore) FindFeeConfigByKey(context.Context, string) (domain.FeeConfig, error) {
	if f.findErr != nil {
		return domain.FeeConfig{}, f.findErr
	}
	if f.existing == nil {
		return domain.FeeConfig{}, domain.ErrNotFound
	}
	return *f.existing, nil
}

func (f *fakeFeeConfigStore) InsertFeeConfig(_ context.Context, request domain.SetFeeConfigRequest) (domain.FeeConfig, error) {
	f.inserted = append(f.inserted, request)
	if f.insertErr != nil {
		return domain.FeeConfig{}, f.insertErr
	}
	if f.insertReply != nil {
		return *f.insertReply, nil
	}
	actor := request.ActorID
	return domain.FeeConfig{
		ID:            uuid.New(),
		BuyerFeeBps:   request.BuyerFeeBps,
		SellerFeeBps:  request.SellerFeeBps,
		EffectiveFrom: *request.EffectiveFrom,
		CreatedBy:     &actor,
	}, nil
}

type fakeInvalidator struct{ calls int }

func (f *fakeInvalidator) Invalidate() { f.calls++ }

func feeRequest() domain.SetFeeConfigRequest {
	return domain.SetFeeConfigRequest{IdempotencyKey: "k1", BuyerFeeBps: 150, SellerFeeBps: 50, ActorID: uuid.New()}
}

func configFor(request domain.SetFeeConfigRequest, effective time.Time) *domain.FeeConfig {
	actor := request.ActorID
	return &domain.FeeConfig{
		ID:            uuid.New(),
		BuyerFeeBps:   request.BuyerFeeBps,
		SellerFeeBps:  request.SellerFeeBps,
		EffectiveFrom: effective,
		CreatedBy:     &actor,
	}
}

func TestSetFeeConfigDefaultsEffectiveFromToNow(t *testing.T) {
	store, cache := &fakeFeeConfigStore{}, &fakeInvalidator{}
	before := time.Now()

	config, err := NewFeeConfigUsecase(store, cache).SetFeeConfig(context.Background(), feeRequest())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.inserted) != 1 {
		t.Fatalf("inserted %d rows, want 1", len(store.inserted))
	}
	if got := store.inserted[0].EffectiveFrom; got == nil || got.Before(before.Truncate(time.Microsecond)) {
		t.Errorf("EffectiveFrom = %v, want now", got)
	}
	if config.BuyerFeeBps != 150 || config.SellerFeeBps != 50 {
		t.Errorf("config = %+v", config)
	}
	if cache.calls != 1 {
		t.Errorf("cache invalidated %d times, want 1", cache.calls)
	}
}

func TestSetFeeConfigKeepsAFutureStartTime(t *testing.T) {
	store := &fakeFeeConfigStore{}
	request := feeRequest()
	future := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	request.EffectiveFrom = &future

	if _, err := NewFeeConfigUsecase(store, &fakeInvalidator{}).SetFeeConfig(context.Background(), request); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !store.inserted[0].EffectiveFrom.Equal(future) {
		t.Errorf("EffectiveFrom = %v, want %v", store.inserted[0].EffectiveFrom, future)
	}
}

func TestSetFeeConfigRejectsAPastStartTime(t *testing.T) {
	store, cache := &fakeFeeConfigStore{}, &fakeInvalidator{}
	request := feeRequest()
	past := time.Now().Add(-time.Minute)
	request.EffectiveFrom = &past

	_, err := NewFeeConfigUsecase(store, cache).SetFeeConfig(context.Background(), request)

	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
	if len(store.inserted) != 0 || cache.calls != 0 {
		t.Error("a retroactive change must not be stored")
	}
}

func TestSetFeeConfigRejectsInvalidRatesBeforeTheStore(t *testing.T) {
	store := &fakeFeeConfigStore{}
	request := feeRequest()
	request.BuyerFeeBps = domain.MaxFeeBps + 1

	_, err := NewFeeConfigUsecase(store, &fakeInvalidator{}).SetFeeConfig(context.Background(), request)

	if !errors.Is(err, domain.ErrBadAmount) {
		t.Fatalf("error = %v, want ErrBadAmount", err)
	}
	if len(store.inserted) != 0 {
		t.Error("store must not be called")
	}
}

func TestSetFeeConfigReplayReturnsTheOriginal(t *testing.T) {
	request := feeRequest()
	original := configFor(request, time.Now().Add(-time.Hour))
	store, cache := &fakeFeeConfigStore{existing: original}, &fakeInvalidator{}

	config, err := NewFeeConfigUsecase(store, cache).SetFeeConfig(context.Background(), request)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.ID != original.ID {
		t.Errorf("ID = %v, want %v", config.ID, original.ID)
	}
	if len(store.inserted) != 0 {
		t.Error("a replay must not insert")
	}
}

// A retry of a request whose start time has since passed is still that same
// request, so the replay check has to come before the past-time check.
func TestSetFeeConfigReplayOfAStartTimeNowInThePast(t *testing.T) {
	request := feeRequest()
	past := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	request.EffectiveFrom = &past
	store := &fakeFeeConfigStore{existing: configFor(request, past)}

	if _, err := NewFeeConfigUsecase(store, &fakeInvalidator{}).SetFeeConfig(context.Background(), request); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSetFeeConfigConflictsWhenTheKeyWasUsedDifferently(t *testing.T) {
	request := feeRequest()
	store := &fakeFeeConfigStore{existing: configFor(request, time.Now())}
	changed := request
	changed.BuyerFeeBps = 999

	_, err := NewFeeConfigUsecase(store, &fakeInvalidator{}).SetFeeConfig(context.Background(), changed)

	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("error = %v, want ErrIdempotencyConflict", err)
	}
	if len(store.inserted) != 0 {
		t.Error("must not insert")
	}
}

// Two requests racing on one key: the insert hands back the row that won.
func TestSetFeeConfigConflictsWhenARacingRequestWon(t *testing.T) {
	request := feeRequest()
	winner := request
	winner.BuyerFeeBps = 999
	store := &fakeFeeConfigStore{insertReply: configFor(winner, time.Now())}

	_, err := NewFeeConfigUsecase(store, &fakeInvalidator{}).SetFeeConfig(context.Background(), request)

	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestSetFeeConfigReturnsStoreFailures(t *testing.T) {
	tests := []*fakeFeeConfigStore{
		{findErr: domain.ErrUnavailable},
		{insertErr: domain.ErrUnavailable},
	}

	for _, store := range tests {
		cache := &fakeInvalidator{}

		_, err := NewFeeConfigUsecase(store, cache).SetFeeConfig(context.Background(), feeRequest())

		if !errors.Is(err, domain.ErrUnavailable) {
			t.Errorf("error = %v, want ErrUnavailable", err)
		}
		if cache.calls != 0 {
			t.Error("a failed write must not invalidate the cache")
		}
	}
}

// The first attempt can commit and still fail on the way back. The retry is the
// first success this replica sees, so it has to clear the cached rate.
func TestSetFeeConfigReplayInvalidatesTheCache(t *testing.T) {
	request := feeRequest()
	store, cache := &fakeFeeConfigStore{existing: configFor(request, time.Now())}, &fakeInvalidator{}

	if _, err := NewFeeConfigUsecase(store, cache).SetFeeConfig(context.Background(), request); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cache.calls != 1 {
		t.Errorf("cache invalidated %d times, want 1", cache.calls)
	}
}
