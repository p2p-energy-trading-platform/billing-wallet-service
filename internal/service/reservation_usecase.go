package service

import (
	"context"
	"fmt"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
)

type ReservationStore interface {
	ActiveFeeConfig(ctx context.Context, at time.Time) (domain.FeeConfig, error)
	// ReserveHold locks amount and snapshots the fee rate. A retry of the same order
	// returns the original hold, whatever amount and rate are passed this time.
	ReserveHold(ctx context.Context, request domain.ReserveRequest, fee domain.FeeConfig, amount int64) (domain.Reservation, error)
	ReleaseHold(ctx context.Context, orderID uuid.UUID, reason domain.ReleaseReason) (int64, error)
}

type ReservationUsecase struct {
	store ReservationStore
}

func NewReservationUsecase(store ReservationStore) *ReservationUsecase {
	return &ReservationUsecase{store: store}
}

// ReserveCredits locks what a buy order could cost at its maximum price. Billing
// computes this itself so OMS cannot drift from billing's rounding rules.
func (u *ReservationUsecase) ReserveCredits(ctx context.Context, request domain.ReserveRequest) (domain.Reservation, error) {
	now := time.Now()

	if err := request.Validate(now); err != nil {
		return domain.Reservation{}, err
	}

	fee, err := u.store.ActiveFeeConfig(ctx, now)
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("load active fee config: %w", err)
	}

	amount, err := domain.ReservationAmount(request.QtyWh, request.MaxPricePerKwh, fee.BuyerFeeBps)
	if err != nil {
		return domain.Reservation{}, err
	}

	return u.store.ReserveHold(ctx, request, fee, amount)
}

// ReleaseReservation returns whatever is left of an order's hold to the user.
func (u *ReservationUsecase) ReleaseReservation(ctx context.Context, orderID uuid.UUID, reason domain.ReleaseReason) (int64, error) {
	if err := reason.Validate(); err != nil {
		return 0, err
	}

	return u.store.ReleaseHold(ctx, orderID, reason)
}
