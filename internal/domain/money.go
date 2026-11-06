package domain

import (
	"math"
	"math/bits"
)

const (
	// RoundingBufferUnits is reserved on top of an order's value and fee, because
	// rounding per fill can add up to more than rounding the order as a whole. It
	// allows for 50 fills at 1 unit each, and the unused part is released when the
	// order completes.
	RoundingBufferUnits int64 = 50

	// UnitsPerCredit is the number of minor units in one credit (1 credit = 1 LKR).
	UnitsPerCredit int64 = 100

	// MaxFeeBps is the highest fee rate allowed, matching the fee_configs CHECK.
	MaxFeeBps int32 = 2000

	whPerKwh    uint64 = 1000
	bpsPerWhole uint64 = 10_000
)

// TradeValue returns round_half_up(qtyWh * pricePerKwh / 1000) in minor units.
// A very small trade can round down to a value of 0.
func TradeValue(qtyWh, pricePerKwh int64) (int64, error) {
	if qtyWh <= 0 {
		return 0, NewBadAmountError("qty_wh", "must be positive")
	}
	if pricePerKwh <= 0 {
		return 0, NewBadAmountError("price_per_kwh", "must be positive")
	}

	value, ok := mulDivRoundHalfUp(uint64(qtyWh), uint64(pricePerKwh), whPerKwh)
	if !ok {
		return 0, NewBadAmountError("trade_value", "exceeds the maximum representable amount")
	}

	return value, nil
}

// Fee returns round_half_up(value * bps / 10_000) in minor units. Since bps is
// capped at MaxFeeBps, the result is never larger than value.
func Fee(value int64, bps int32) (int64, error) {
	if value < 0 {
		return 0, NewBadAmountError("value", "must not be negative")
	}
	if bps < 0 || bps > MaxFeeBps {
		return 0, NewBadAmountError("fee_bps", "must be between 0 and 2000")
	}

	fee, ok := mulDivRoundHalfUp(uint64(value), uint64(bps), bpsPerWhole)
	if !ok {
		return 0, NewBadAmountError("fee", "exceeds the maximum representable amount")
	}

	return fee, nil
}

// ReservationAmount returns what a buy order must lock: its value at the maximum
// price, the buyer's fee on that value, and the rounding buffer.
func ReservationAmount(qtyWh, maxPricePerKwh int64, buyerFeeBps int32) (int64, error) {
	value, err := TradeValue(qtyWh, maxPricePerKwh)
	if err != nil {
		return 0, err
	}

	fee, err := Fee(value, buyerFeeBps)
	if err != nil {
		return 0, err
	}

	if fee > math.MaxInt64-value || RoundingBufferUnits > math.MaxInt64-value-fee {
		return 0, NewBadAmountError("reservation", "exceeds the maximum representable amount")
	}

	return value + fee + RoundingBufferUnits, nil
}

// mulDivRoundHalfUp computes round_half_up(a * b / divisor) using a 128-bit
// intermediate, and reports false when the result does not fit in an int64.
// It is the single place money rounding happens. a and b must be below 2^63,
// which keeps the addition below from overflowing the high word.
func mulDivRoundHalfUp(a, b, divisor uint64) (int64, bool) {
	hi, lo := bits.Mul64(a, b)
	lo, carry := bits.Add64(lo, divisor/2, 0)
	hi += carry

	if hi >= divisor {
		return 0, false
	}

	quotient, _ := bits.Div64(hi, lo, divisor)
	if quotient > math.MaxInt64 {
		return 0, false
	}

	return int64(quotient), true
}
