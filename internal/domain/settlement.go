package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// maxExecutionSkew is how far ahead of this service's clock a trade's execution
// time may be. More than that would pick up a fee rate scheduled for later.
const maxExecutionSkew = time.Minute

// BalanceReasonTradeSettled marks a balance change made by settling a trade.
const BalanceReasonTradeSettled = "TRADE_SETTLED"

// FeeRates looks up the fee config in effect at a given time.
type FeeRates interface {
	ActiveFeeConfig(ctx context.Context, at time.Time) (FeeConfig, error)
}

// SettleRequest asks to settle one matched trade.
type SettleRequest struct {
	TradeID       uuid.UUID
	BuyOrderID    uuid.UUID
	SellOrderID   uuid.UUID
	BuyerUserID   uuid.UUID
	SellerUserID  uuid.UUID
	QtyWh         int64
	PricePerKwh   int64 // execution price
	BuyOrderFinal bool  // the buy order's last fill: return what is left of its hold
	ExecutedAt    time.Time
}

func (r SettleRequest) Validate(now time.Time) error {
	for _, id := range []struct {
		field string
		value uuid.UUID
	}{
		{"trade_id", r.TradeID}, {"buy_order_id", r.BuyOrderID}, {"sell_order_id", r.SellOrderID},
		{"buyer_user_id", r.BuyerUserID}, {"seller_user_id", r.SellerUserID},
	} {
		if id.value == uuid.Nil {
			return NewValidationError(id.field, "is required")
		}
	}
	if r.BuyerUserID == r.SellerUserID {
		return NewValidationError("seller_user_id", "must differ from the buyer: self-trades are not settled")
	}
	if r.QtyWh <= 0 {
		return NewBadAmountError("qty_wh", "must be positive")
	}
	if r.PricePerKwh <= 0 {
		return NewBadAmountError("price_per_kwh", "must be positive")
	}
	if r.ExecutedAt.IsZero() {
		return NewValidationError("executed_at", "is required")
	}
	if r.ExecutedAt.After(now.Add(maxExecutionSkew)) {
		return NewValidationError("executed_at", "must not be in the future")
	}

	return nil
}

// SettlementPlan is what settling one fill does to the money and the buyer's hold.
type SettlementPlan struct {
	Split      FeeSplit
	Released   int64 // returned to the buyer because this was the order's last fill
	HoldStatus HoldStatus
}

// PlanSettlement works out one fill against this buyer hold. The buyer pays the fee
// rate locked in the hold, and the seller the rate given, which is the one in effect
// when the trade executed.
func (h Hold) PlanSettlement(request SettleRequest, sellerFeeBps int32) (SettlementPlan, error) {
	if h.Kind != HoldKindOrder || h.Status != HoldActive || h.MaxPricePerKwh == nil || h.BuyerFeeBps == nil {
		return SettlementPlan{}, ErrHoldNotFound
	}
	if request.PricePerKwh > *h.MaxPricePerKwh {
		return SettlementPlan{}, ErrPriceExceedsReservation
	}

	value, err := TradeValue(request.QtyWh, request.PricePerKwh)
	if err != nil {
		return SettlementPlan{}, err
	}
	// A ledger line can never be zero, so a trade worth nothing cannot be posted.
	if value == 0 {
		return SettlementPlan{}, NewBadAmountError("trade_value", "rounds to zero")
	}

	split, err := SplitFees(value, *h.BuyerFeeBps, sellerFeeBps)
	if err != nil {
		return SettlementPlan{}, err
	}

	remaining := h.Remaining()
	if split.BuyerDebit > remaining {
		return SettlementPlan{}, ErrHoldInsufficient
	}

	plan := SettlementPlan{Split: split, HoldStatus: HoldActive}
	if request.BuyOrderFinal {
		plan.Released = remaining - split.BuyerDebit
		plan.HoldStatus = HoldReleased
		if plan.Released == 0 {
			plan.HoldStatus = HoldConsumed
		}
	}

	return plan, nil
}

// SettlementLines are the journal lines for one fill. A line is left out when its
// amount is zero, which happens to the platform's line when both fees round to zero.
func SettlementLines(buyerAccount, sellerAccount, revenueAccount uuid.UUID, split FeeSplit) []LedgerLine {
	lines := []LedgerLine{
		{AccountID: buyerAccount, Amount: -split.BuyerDebit},
		{AccountID: sellerAccount, Amount: split.SellerCredit},
		{AccountID: revenueAccount, Amount: split.PlatformCredit},
	}

	kept := lines[:0]
	for _, line := range lines {
		if line.Amount != 0 {
			kept = append(kept, line)
		}
	}

	return kept
}

// MatchesRequest reports whether this settlement is the trade the request describes,
// which makes a retry a replay rather than a conflict. buyOrderFinal is the flag the
// trade was settled with: a retry that changes it would never return the rest of
// the hold.
func (s TradeSettlement) MatchesRequest(request SettleRequest, buyOrderFinal bool, buyerWalletID, sellerWalletID uuid.UUID) bool {
	return request.BuyOrderFinal == buyOrderFinal &&
		s.BuyerWalletID == buyerWalletID &&
		s.SellerWalletID == sellerWalletID &&
		s.BuyOrderID == request.BuyOrderID &&
		s.SellOrderID == request.SellOrderID &&
		s.QtyWh == request.QtyWh &&
		s.PricePerKwh == request.PricePerKwh
}

// SettlementResult is what SettleTrade returns. A replay returns the original
// amounts with AlreadySettled set.
type SettlementResult struct {
	SettlementID           uuid.UUID // the settlement's journal
	TradeValue             int64
	BuyerFee               int64
	SellerFee              int64
	BuyerReleasedRemainder int64
	AlreadySettled         bool
}
