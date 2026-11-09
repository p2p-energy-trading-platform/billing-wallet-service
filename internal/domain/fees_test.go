package domain

import (
	"errors"
	"math"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/google/uuid"
)

func TestSplitFees(t *testing.T) {
	tests := []struct {
		name      string
		value     int64
		buyerBps  int32
		sellerBps int32
		want      FeeSplit
	}{
		{
			name: "doc 05 example: 1000.00 credits at 1% each side", value: 100_000, buyerBps: 100, sellerBps: 100,
			want: FeeSplit{Value: 100_000, BuyerFee: 1000, SellerFee: 1000, BuyerDebit: 101_000, SellerCredit: 99_000, PlatformCredit: 2000},
		},
		{
			name: "rates can differ per side", value: 100_000, buyerBps: 50, sellerBps: 200,
			want: FeeSplit{Value: 100_000, BuyerFee: 500, SellerFee: 2000, BuyerDebit: 100_500, SellerCredit: 98_000, PlatformCredit: 2500},
		},
		{
			name: "seller-only model", value: 100_000, buyerBps: 0, sellerBps: 100,
			want: FeeSplit{Value: 100_000, BuyerFee: 0, SellerFee: 1000, BuyerDebit: 100_000, SellerCredit: 99_000, PlatformCredit: 1000},
		},
		{
			name: "buyer-only model", value: 100_000, buyerBps: 100, sellerBps: 0,
			want: FeeSplit{Value: 100_000, BuyerFee: 1000, SellerFee: 0, BuyerDebit: 101_000, SellerCredit: 100_000, PlatformCredit: 1000},
		},
		{
			name: "each side rounds half up on its own", value: 50, buyerBps: 100, sellerBps: 100,
			want: FeeSplit{Value: 50, BuyerFee: 1, SellerFee: 1, BuyerDebit: 51, SellerCredit: 49, PlatformCredit: 2},
		},
		{
			name: "just below half rounds to no fee", value: 49, buyerBps: 100, sellerBps: 100,
			want: FeeSplit{Value: 49, BuyerFee: 0, SellerFee: 0, BuyerDebit: 49, SellerCredit: 49, PlatformCredit: 0},
		},
		{
			name: "zero value", value: 0, buyerBps: 100, sellerBps: 100,
			want: FeeSplit{},
		},
		{
			name: "maximum rate on both sides", value: 100_000, buyerBps: MaxFeeBps, sellerBps: MaxFeeBps,
			want: FeeSplit{Value: 100_000, BuyerFee: 20_000, SellerFee: 20_000, BuyerDebit: 120_000, SellerCredit: 80_000, PlatformCredit: 40_000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SplitFees(tt.value, tt.buyerBps, tt.sellerBps)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("SplitFees() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSplitFeesRejectsBadInput(t *testing.T) {
	tests := []struct {
		name      string
		value     int64
		buyerBps  int32
		sellerBps int32
	}{
		{"negative value", -1, 100, 100},
		{"buyer rate above the maximum", 100, MaxFeeBps + 1, 100},
		{"seller rate above the maximum", 100, 100, MaxFeeBps + 1},
		{"negative buyer rate", 100, -1, 100},
		{"negative seller rate", 100, 100, -1},
		{"buyer debit would overflow", math.MaxInt64, 100, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SplitFees(tt.value, tt.buyerBps, tt.sellerBps)

			if !errors.Is(err, ErrBadAmount) {
				t.Errorf("error = %v, want ErrBadAmount", err)
			}
		})
	}
}

func TestSplitFeesLeavesNoDust(t *testing.T) {
	property := func(rawValue int64, rawBuyerBps, rawSellerBps int32) bool {
		value := bounded(rawValue, 1_000_000_000_000)
		buyerBps, sellerBps := boundedBps(rawBuyerBps), boundedBps(rawSellerBps)

		split, err := SplitFees(value, buyerBps, sellerBps)
		if err != nil {
			return false
		}
		alone, err := Fee(value, sellerBps)
		if err != nil {
			return false
		}

		return split.BuyerDebit == split.SellerCredit+split.PlatformCredit &&
			split.PlatformCredit == split.BuyerFee+split.SellerFee &&
			split.BuyerFee >= 0 && split.BuyerFee <= value &&
			split.SellerFee >= 0 && split.SellerFee <= value &&
			split.SellerCredit >= 0 &&
			split.SellerFee == alone // one side's rate never affects the other's fee
	}

	if err := quick.Check(property, nil); err != nil {
		t.Error(err)
	}
}

func validFeeRequest() SetFeeConfigRequest {
	return SetFeeConfigRequest{
		IdempotencyKey: "fee-change-1",
		BuyerFeeBps:    150,
		SellerFeeBps:   50,
		ActorID:        uuid.New(),
	}
}

func TestSetFeeConfigRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*SetFeeConfigRequest)
		wantErr error
	}{
		{name: "valid", change: func(*SetFeeConfigRequest) {}},
		{name: "zero rates are allowed", change: func(r *SetFeeConfigRequest) { r.BuyerFeeBps, r.SellerFeeBps = 0, 0 }},
		{name: "maximum rates are allowed", change: func(r *SetFeeConfigRequest) { r.BuyerFeeBps, r.SellerFeeBps = MaxFeeBps, MaxFeeBps }},
		{name: "buyer rate too high", change: func(r *SetFeeConfigRequest) { r.BuyerFeeBps = MaxFeeBps + 1 }, wantErr: ErrBadAmount},
		{name: "seller rate too high", change: func(r *SetFeeConfigRequest) { r.SellerFeeBps = MaxFeeBps + 1 }, wantErr: ErrBadAmount},
		{name: "negative buyer rate", change: func(r *SetFeeConfigRequest) { r.BuyerFeeBps = -1 }, wantErr: ErrBadAmount},
		{name: "negative seller rate", change: func(r *SetFeeConfigRequest) { r.SellerFeeBps = -1 }, wantErr: ErrBadAmount},
		{name: "missing key", change: func(r *SetFeeConfigRequest) { r.IdempotencyKey = "" }, wantErr: ErrValidation},
		{name: "key too long", change: func(r *SetFeeConfigRequest) { r.IdempotencyKey = strings.Repeat("k", 256) }, wantErr: ErrValidation},
		{name: "missing actor", change: func(r *SetFeeConfigRequest) { r.ActorID = uuid.Nil }, wantErr: ErrValidation},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := validFeeRequest()
			tt.change(&request)

			if err := request.Validate(); !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestFeeConfigMatchesRequest(t *testing.T) {
	effective := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	later := effective.Add(time.Hour)
	request := validFeeRequest()
	actor := request.ActorID
	config := FeeConfig{BuyerFeeBps: 150, SellerFeeBps: 50, EffectiveFrom: effective, CreatedBy: &actor}

	if !config.MatchesRequest(request) {
		t.Error("a retry that left effective_from unset must match")
	}

	request.EffectiveFrom = &effective
	if !config.MatchesRequest(request) {
		t.Error("identical parameters must match")
	}

	other := uuid.New()
	tests := []struct {
		name   string
		change func(*SetFeeConfigRequest)
	}{
		{"different buyer rate", func(r *SetFeeConfigRequest) { r.BuyerFeeBps = 151 }},
		{"different seller rate", func(r *SetFeeConfigRequest) { r.SellerFeeBps = 51 }},
		{"different start time", func(r *SetFeeConfigRequest) { r.EffectiveFrom = &later }},
		{"different admin", func(r *SetFeeConfigRequest) { r.ActorID = other }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := request
			tt.change(&changed)

			if config.MatchesRequest(changed) {
				t.Error("must not match")
			}
		})
	}

	if (FeeConfig{BuyerFeeBps: 150, SellerFeeBps: 50, EffectiveFrom: effective}).MatchesRequest(request) {
		t.Error("a config with no recorded admin must not match")
	}
}
