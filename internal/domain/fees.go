package domain

import (
	"math"
	"time"

	"github.com/google/uuid"
)

const maxIdempotencyKeyLength = 255

// FeeSplit is what one trade costs each side under the balanced split model.
type FeeSplit struct {
	Value          int64
	BuyerFee       int64
	SellerFee      int64
	BuyerDebit     int64
	SellerCredit   int64
	PlatformCredit int64
}

// SplitFees computes each side's fee independently from the same value. That
// guarantees BuyerDebit == SellerCredit + PlatformCredit, so rounding leaves no
// dust behind.
func SplitFees(value int64, buyerFeeBps, sellerFeeBps int32) (FeeSplit, error) {
	buyerFee, err := Fee(value, buyerFeeBps)
	if err != nil {
		return FeeSplit{}, err
	}

	sellerFee, err := Fee(value, sellerFeeBps)
	if err != nil {
		return FeeSplit{}, err
	}
	sellerFee = min(sellerFee, value)

	if buyerFee > math.MaxInt64-value {
		return FeeSplit{}, NewBadAmountError("buyer_debit", "exceeds the maximum representable amount")
	}

	return FeeSplit{
		Value:          value,
		BuyerFee:       buyerFee,
		SellerFee:      sellerFee,
		BuyerDebit:     value + buyerFee,
		SellerCredit:   value - sellerFee,
		PlatformCredit: buyerFee + sellerFee,
	}, nil
}

// SetFeeConfigRequest asks to add a new fee configuration.
type SetFeeConfigRequest struct {
	IdempotencyKey string
	BuyerFeeBps    int32
	SellerFeeBps   int32
	EffectiveFrom  *time.Time // nil means now
	ActorID        uuid.UUID
}

func (r SetFeeConfigRequest) Validate() error {
	if r.IdempotencyKey == "" || len(r.IdempotencyKey) > maxIdempotencyKeyLength {
		return NewValidationError("idempotency_key", "must be between 1 and 255 characters")
	}
	if r.BuyerFeeBps < 0 || r.BuyerFeeBps > MaxFeeBps {
		return NewBadAmountError("buyer_fee_bps", "must be between 0 and 2000")
	}
	if r.SellerFeeBps < 0 || r.SellerFeeBps > MaxFeeBps {
		return NewBadAmountError("seller_fee_bps", "must be between 0 and 2000")
	}
	if r.ActorID == uuid.Nil {
		return NewValidationError("actor", "is required")
	}

	return nil
}

// MatchesRequest reports whether this config is what the request would create,
// which makes a retried request a replay rather than a conflict. A request with no
// start time matches any, because the original one defaulted to the time it ran.
func (c FeeConfig) MatchesRequest(request SetFeeConfigRequest) bool {
	if c.CreatedBy == nil || *c.CreatedBy != request.ActorID {
		return false
	}
	if request.EffectiveFrom != nil && !request.EffectiveFrom.Equal(c.EffectiveFrom) {
		return false
	}

	return c.BuyerFeeBps == request.BuyerFeeBps && c.SellerFeeBps == request.SellerFeeBps
}
