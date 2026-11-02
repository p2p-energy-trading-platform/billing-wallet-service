package domain

import (
	"errors"
	"math"
	"testing"
	"testing/quick"
)

func TestTradeValue(t *testing.T) {
	tests := []struct {
		name          string
		qtyWh         int64
		pricePerKwh   int64
		want          int64
		wantRejection bool
	}{
		{name: "exact kWh", qtyWh: 1000, pricePerKwh: 4550, want: 4550},
		{name: "fraction of a kWh", qtyWh: 1500, pricePerKwh: 4550, want: 6825},
		{name: "below half rounds down", qtyWh: 1, pricePerKwh: 499, want: 0},
		{name: "exactly half rounds up", qtyWh: 1, pricePerKwh: 500, want: 1},
		{name: "above half rounds up", qtyWh: 1, pricePerKwh: 501, want: 1},
		{name: "intermediate product wider than int64 still resolves", qtyWh: math.MaxInt64, pricePerKwh: 1000, want: math.MaxInt64},
		{name: "result too large for int64", qtyWh: math.MaxInt64, pricePerKwh: 1001, wantRejection: true},
		{name: "both operands at max", qtyWh: math.MaxInt64, pricePerKwh: math.MaxInt64, wantRejection: true},
		{name: "zero quantity", qtyWh: 0, pricePerKwh: 100, wantRejection: true},
		{name: "negative quantity", qtyWh: -1, pricePerKwh: 100, wantRejection: true},
		{name: "zero price", qtyWh: 100, pricePerKwh: 0, wantRejection: true},
		{name: "negative price", qtyWh: 100, pricePerKwh: -1, wantRejection: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TradeValue(tt.qtyWh, tt.pricePerKwh)

			if tt.wantRejection {
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("TradeValue(%d, %d) error = %v, want ErrValidation", tt.qtyWh, tt.pricePerKwh, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("TradeValue(%d, %d) unexpected error: %v", tt.qtyWh, tt.pricePerKwh, err)
			}
			if got != tt.want {
				t.Errorf("TradeValue(%d, %d) = %d, want %d", tt.qtyWh, tt.pricePerKwh, got, tt.want)
			}
		})
	}
}

func TestFee(t *testing.T) {
	tests := []struct {
		name          string
		value         int64
		bps           int32
		want          int64
		wantRejection bool
	}{
		{name: "default rate on 1000 credits", value: 100_000, bps: 100, want: 1000},
		{name: "zero bps", value: 100_000, bps: 0, want: 0},
		{name: "zero value", value: 0, bps: 100, want: 0},
		{name: "below half rounds down", value: 49, bps: 100, want: 0},
		{name: "exactly half rounds up", value: 50, bps: 100, want: 1},
		{name: "maximum rate", value: 100_000, bps: MaxFeeBps, want: 20_000},
		{name: "largest value does not overflow", value: math.MaxInt64, bps: MaxFeeBps, want: 1844674407370955161},
		{name: "negative value", value: -1, bps: 100, wantRejection: true},
		{name: "negative bps", value: 100, bps: -1, wantRejection: true},
		{name: "bps above maximum", value: 100, bps: MaxFeeBps + 1, wantRejection: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Fee(tt.value, tt.bps)

			if tt.wantRejection {
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("Fee(%d, %d) error = %v, want ErrValidation", tt.value, tt.bps, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Fee(%d, %d) unexpected error: %v", tt.value, tt.bps, err)
			}
			if got != tt.want {
				t.Errorf("Fee(%d, %d) = %d, want %d", tt.value, tt.bps, got, tt.want)
			}
		})
	}
}

// bounded maps arbitrary generated values into [1, limit] so the properties
// exercise realistic magnitudes without every case hitting the overflow path.
func bounded(v, limit int64) int64 {
	if v == math.MinInt64 {
		v = 0
	}
	if v < 0 {
		v = -v
	}
	return v%limit + 1
}

func boundedBps(v int32) int32 {
	if v == math.MinInt32 {
		v = 0
	}
	if v < 0 {
		v = -v
	}
	return v % (MaxFeeBps + 1)
}

func TestFeeProperties(t *testing.T) {
	property := func(rawQty, rawPrice int64, rawBuyerBps, rawSellerBps int32) bool {
		value, err := TradeValue(bounded(rawQty, 1_000_000_000), bounded(rawPrice, 1_000_000))
		if err != nil {
			return false
		}
		buyerFee, err := Fee(value, boundedBps(rawBuyerBps))
		if err != nil {
			return false
		}
		sellerFee, err := Fee(value, boundedBps(rawSellerBps))
		if err != nil {
			return false
		}

		buyerDebit := value + buyerFee
		sellerCredit := value - sellerFee
		platformCredit := buyerFee + sellerFee

		return buyerFee >= 0 && sellerFee >= 0 &&
			buyerFee <= value && sellerFee <= value &&
			buyerDebit == sellerCredit+platformCredit
	}

	if err := quick.Check(property, nil); err != nil {
		t.Error(err)
	}
}

func TestReservationAtMaxPriceCoversAnyLowerFill(t *testing.T) {
	property := func(rawQty, rawMaxPrice, rawFillPrice int64, rawBps int32) bool {
		qty := bounded(rawQty, 1_000_000_000)
		maxPrice := bounded(rawMaxPrice, 1_000_000)
		fillPrice := bounded(rawFillPrice, maxPrice)
		bps := boundedBps(rawBps)

		maxValue, err := TradeValue(qty, maxPrice)
		if err != nil {
			return false
		}
		maxFee, err := Fee(maxValue, bps)
		if err != nil {
			return false
		}
		fillValue, err := TradeValue(qty, fillPrice)
		if err != nil {
			return false
		}
		fillFee, err := Fee(fillValue, bps)
		if err != nil {
			return false
		}

		return fillValue+fillFee <= maxValue+maxFee
	}

	if err := quick.Check(property, nil); err != nil {
		t.Error(err)
	}
}
