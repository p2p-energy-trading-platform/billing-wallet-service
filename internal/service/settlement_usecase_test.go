package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
)

type fakeSettlementStore struct {
	result    domain.SettlementResult
	err       error
	calls     int
	request   domain.SettleRequest
	sellerFee domain.FeeConfig
	replay    bool // the trade is already settled, so no rate is needed
}

func (f *fakeSettlementStore) SettleTrade(ctx context.Context, request domain.SettleRequest, fees FeeRates) (domain.SettlementResult, error) {
	f.calls++
	f.request = request
	if f.replay {
		return f.result, f.err
	}
	sellerFee, err := fees.ActiveFeeConfig(ctx, request.ExecutedAt)
	if err != nil {
		return domain.SettlementResult{}, err
	}
	f.sellerFee = sellerFee
	return f.result, f.err
}

type recordingFeeRates struct {
	fee domain.FeeConfig
	err error
	at  time.Time
}

func (r *recordingFeeRates) ActiveFeeConfig(_ context.Context, at time.Time) (domain.FeeConfig, error) {
	r.at = at
	return r.fee, r.err
}

func settleRequest() domain.SettleRequest {
	return domain.SettleRequest{
		TradeID: uuid.New(), BuyOrderID: uuid.New(), SellOrderID: uuid.New(),
		BuyerUserID: uuid.New(), SellerUserID: uuid.New(),
		QtyWh: 1000, PricePerKwh: 4550, ExecutedAt: time.Now().Add(-time.Minute),
	}
}

func TestSettleTradeUsesTheSellerRateAtExecutionTime(t *testing.T) {
	fee := domain.FeeConfig{ID: uuid.New(), SellerFeeBps: 250}
	store, rates := &fakeSettlementStore{result: domain.SettlementResult{TradeValue: 4550}}, &recordingFeeRates{fee: fee}
	request := settleRequest()

	result, err := NewSettlementUsecase(store, rates).SettleTrade(context.Background(), request)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rates.at.Equal(request.ExecutedAt) {
		t.Errorf("rate looked up at %v, want executed_at %v", rates.at, request.ExecutedAt)
	}
	if store.sellerFee.ID != fee.ID || store.request.TradeID != request.TradeID {
		t.Errorf("store got %+v with %+v", store.request, store.sellerFee)
	}
	if result.TradeValue != 4550 {
		t.Errorf("result = %+v", result)
	}
}

func TestSettleTradeRejectsInvalidRequestsBeforeAnyLookup(t *testing.T) {
	request := settleRequest()
	request.SellerUserID = request.BuyerUserID
	store, rates := &fakeSettlementStore{}, &recordingFeeRates{}

	_, err := NewSettlementUsecase(store, rates).SettleTrade(context.Background(), request)

	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
	if store.calls != 0 || !rates.at.IsZero() {
		t.Error("nothing may be looked up for an invalid request")
	}
}

func TestSettleTradeFailsClosedWithoutASellerRate(t *testing.T) {
	store := &fakeSettlementStore{}

	_, err := NewSettlementUsecase(store, &recordingFeeRates{err: errors.New("no active fee config")}).SettleTrade(context.Background(), settleRequest())

	if err == nil {
		t.Fatal("expected an error")
	}
}

// A retry of a settled trade needs no rate, so a failing rate lookup must not stop
// it from returning the original result.
func TestSettleTradeReplayDoesNotNeedASellerRate(t *testing.T) {
	store := &fakeSettlementStore{replay: true, result: domain.SettlementResult{AlreadySettled: true}}
	rates := &recordingFeeRates{err: domain.ErrUnavailable}

	result, err := NewSettlementUsecase(store, rates).SettleTrade(context.Background(), settleRequest())

	if err != nil || !result.AlreadySettled {
		t.Fatalf("result = %+v, err = %v, want the stored result", result, err)
	}
	if !rates.at.IsZero() {
		t.Error("the rate must not be looked up for a replay")
	}
}

func TestSettleTradeRejectsAFutureExecutionTime(t *testing.T) {
	request := settleRequest()
	request.ExecutedAt = time.Now().Add(time.Hour)
	store := &fakeSettlementStore{}

	_, err := NewSettlementUsecase(store, &recordingFeeRates{}).SettleTrade(context.Background(), request)

	if !errors.Is(err, domain.ErrValidation) || store.calls != 0 {
		t.Errorf("err = %v, store calls = %d, want a validation error before the store", err, store.calls)
	}
}
