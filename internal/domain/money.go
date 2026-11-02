package domain

import (
	"math"
	"math/bits"
)

const (
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
		return 0, NewValidationError("qty_wh", "must be positive")
	}
	if pricePerKwh <= 0 {
		return 0, NewValidationError("price_per_kwh", "must be positive")
	}

	value, ok := mulDivRoundHalfUp(uint64(qtyWh), uint64(pricePerKwh), whPerKwh)
	if !ok {
		return 0, NewValidationError("trade_value", "exceeds the maximum representable amount")
	}

	return value, nil
}

// Fee returns round_half_up(value * bps / 10_000) in minor units. Since bps is
// capped at MaxFeeBps, the result is never larger than value.
func Fee(value int64, bps int32) (int64, error) {
	if value < 0 {
		return 0, NewValidationError("value", "must not be negative")
	}
	if bps < 0 || bps > MaxFeeBps {
		return 0, NewValidationError("fee_bps", "must be between 0 and 2000")
	}

	fee, ok := mulDivRoundHalfUp(uint64(value), uint64(bps), bpsPerWhole)
	if !ok {
		return 0, NewValidationError("fee", "exceeds the maximum representable amount")
	}

	return fee, nil
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
