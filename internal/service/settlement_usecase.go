package service

import (
	"context"
	"time"

	"billing-wallet-service/internal/domain"
)

type SettlementStore interface {
	// SettleTrade settles one fill, looking up the seller's rate only once it knows
	// the trade is not already settled. A retry of a settled trade returns the
	// original result with AlreadySettled set.
	SettleTrade(ctx context.Context, request domain.SettleRequest, fees domain.FeeRates) (domain.SettlementResult, error)
}

type SettlementUsecase struct {
	store SettlementStore
	fees  FeeRates
}

func NewSettlementUsecase(store SettlementStore, fees FeeRates) *SettlementUsecase {
	return &SettlementUsecase{store: store, fees: fees}
}

// SettleTrade settles one matched trade. The seller pays the fee rate in effect when
// the trade executed. The buyer pays the rate locked in their hold.
func (u *SettlementUsecase) SettleTrade(ctx context.Context, request domain.SettleRequest) (domain.SettlementResult, error) {
	if err := request.Validate(time.Now()); err != nil {
		return domain.SettlementResult{}, err
	}

	return u.store.SettleTrade(ctx, request, u.fees)
}
