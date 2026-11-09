package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
)

type fakeReservationStore struct {
	fee         domain.FeeConfig
	feeErr      error
	feeAt       time.Time
	reservation domain.Reservation
	reserveErr  error
	reserved    []reservedCall
	released    int64
	releaseErr  error
	releases    int
}

type reservedCall struct {
	request domain.ReserveRequest
	fee     domain.FeeConfig
	amount  int64
}

func (f *fakeReservationStore) ActiveFeeConfig(_ context.Context, at time.Time) (domain.FeeConfig, error) {
	f.feeAt = at
	return f.fee, f.feeErr
}

func (f *fakeReservationStore) ReserveHold(_ context.Context, request domain.ReserveRequest, fee domain.FeeConfig, amount int64) (domain.Reservation, error) {
	f.reserved = append(f.reserved, reservedCall{request, fee, amount})
	return f.reservation, f.reserveErr
}

func (f *fakeReservationStore) ReleaseHold(context.Context, uuid.UUID, domain.ReleaseReason) (int64, error) {
	f.releases++
	return f.released, f.releaseErr
}

func validReserveRequest() domain.ReserveRequest {
	return domain.ReserveRequest{
		UserID:         uuid.New(),
		OrderID:        uuid.New(),
		OrderType:      domain.OrderTypeLimit,
		QtyWh:          1000,
		MaxPricePerKwh: 4550,
	}
}

func TestReserveCreditsSizesTheHoldFromTheActiveRate(t *testing.T) {
	fee := domain.FeeConfig{ID: uuid.New(), BuyerFeeBps: 500}
	store := &fakeReservationStore{fee: fee, reservation: domain.Reservation{AvailableAfter: 7}}
	before := time.Now()

	reservation, err := NewReservationUsecase(store, store).ReserveCredits(context.Background(), validReserveRequest())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reservation.AvailableAfter != 7 {
		t.Errorf("AvailableAfter = %d, want 7", reservation.AvailableAfter)
	}
	if len(store.reserved) != 1 {
		t.Fatalf("store called %d times, want 1", len(store.reserved))
	}
	// value 4550 + 5% fee 228 + buffer 50
	if got := store.reserved[0]; got.amount != 4828 || got.fee.ID != fee.ID {
		t.Errorf("reserved %d with config %v, want 4828 with %v", got.amount, got.fee.ID, fee.ID)
	}
	if store.feeAt.Before(before) {
		t.Errorf("rate looked up at %v, want now", store.feeAt)
	}
}

func TestReserveCreditsRejectsInvalidRequestsBeforeTouchingTheStore(t *testing.T) {
	request := validReserveRequest()
	request.OrderType = domain.OrderTypeMarket
	request.MaxPricePerKwh = 0
	store := &fakeReservationStore{}

	_, err := NewReservationUsecase(store, store).ReserveCredits(context.Background(), request)

	if !errors.Is(err, domain.ErrMarketOrderNeedsCollar) {
		t.Fatalf("error = %v, want ErrMarketOrderNeedsCollar", err)
	}
	if len(store.reserved) != 0 {
		t.Error("store must not be called")
	}
}

func TestReserveCreditsFailsClosedWithoutAFeeConfig(t *testing.T) {
	store := &fakeReservationStore{feeErr: errors.New("no active fee config")}

	_, err := NewReservationUsecase(store, store).ReserveCredits(context.Background(), validReserveRequest())

	if err == nil {
		t.Fatal("expected an error")
	}
	if len(store.reserved) != 0 {
		t.Error("must not reserve without a fee rate")
	}
}

func TestReserveCreditsRejectsAnOverflowingOrder(t *testing.T) {
	request := validReserveRequest()
	request.QtyWh = 1 << 62
	request.MaxPricePerKwh = 1 << 62
	store := &fakeReservationStore{}

	_, err := NewReservationUsecase(store, store).ReserveCredits(context.Background(), request)

	if !errors.Is(err, domain.ErrBadAmount) {
		t.Fatalf("error = %v, want ErrBadAmount", err)
	}
	if len(store.reserved) != 0 {
		t.Error("must not reserve an overflowing order")
	}
}

func TestReleaseReservationRejectsAnUnknownReason(t *testing.T) {
	store := &fakeReservationStore{}

	_, err := NewReservationUsecase(store, store).ReleaseReservation(context.Background(), uuid.New(), "")

	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
	if store.releases != 0 {
		t.Error("store must not be called")
	}
}

func TestReleaseReservationReturnsTheStoreResult(t *testing.T) {
	store := &fakeReservationStore{released: 600}

	released, err := NewReservationUsecase(store, store).ReleaseReservation(context.Background(), uuid.New(), domain.ReleaseFilled)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if released != 600 {
		t.Errorf("released = %d, want 600", released)
	}
}
