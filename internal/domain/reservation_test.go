package domain

import (
	"errors"
	"math"
	"testing"
	"testing/quick"
	"time"

	"github.com/google/uuid"
)

func TestReservationAmount(t *testing.T) {
	tests := []struct {
		name        string
		qtyWh       int64
		maxPrice    int64
		bps         int32
		want        int64
		wantBadArgs bool
	}{
		// value 4550 + 1% fee 46 (45.5 rounds up) + buffer 50
		{name: "one kWh at 45.50", qtyWh: 1000, maxPrice: 4550, bps: 100, want: 4646},
		{name: "zero fee still carries the buffer", qtyWh: 1000, maxPrice: 4550, bps: 0, want: 4600},
		{name: "value that rounds to zero is only the buffer", qtyWh: 1, maxPrice: 1, bps: 100, want: RoundingBufferUnits},
		{name: "zero quantity", qtyWh: 0, maxPrice: 4550, bps: 100, wantBadArgs: true},
		{name: "value overflows", qtyWh: math.MaxInt64, maxPrice: 1001, bps: 100, wantBadArgs: true},
		{name: "value fits but value plus fee overflows", qtyWh: math.MaxInt64, maxPrice: 1000, bps: 100, wantBadArgs: true},
		{name: "value plus fee fits but buffer overflows", qtyWh: math.MaxInt64, maxPrice: 1000, bps: 0, wantBadArgs: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReservationAmount(tt.qtyWh, tt.maxPrice, tt.bps)

			if tt.wantBadArgs {
				if !errors.Is(err, ErrBadAmount) {
					t.Fatalf("error = %v, want ErrBadAmount", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ReservationAmount() = %d, want %d", got, tt.want)
			}
		})
	}
}

// A reservation must cover any single fill at or below the reserved price,
// including that fill's fee.
func TestReservationCoversAnyFillAtOrBelowMaxPrice(t *testing.T) {
	property := func(rawQty, rawMaxPrice, rawFillPrice int64, rawBps int32) bool {
		qty := bounded(rawQty, 1_000_000_000)
		maxPrice := bounded(rawMaxPrice, 1_000_000)
		fillPrice := bounded(rawFillPrice, maxPrice)
		bps := boundedBps(rawBps)

		amount, err := ReservationAmount(qty, maxPrice, bps)
		if err != nil {
			return false
		}
		value, err := TradeValue(qty, fillPrice)
		if err != nil {
			return false
		}
		fee, err := Fee(value, bps)
		if err != nil {
			return false
		}

		return value+fee <= amount
	}

	if err := quick.Check(property, nil); err != nil {
		t.Error(err)
	}
}

func TestMoneyErrorsAreBadAmountAndValidation(t *testing.T) {
	_, tradeErr := TradeValue(0, 100)
	_, feeErr := Fee(-1, 100)

	for _, err := range []error{tradeErr, feeErr} {
		if !errors.Is(err, ErrBadAmount) {
			t.Errorf("%v is not ErrBadAmount", err)
		}
		if !errors.Is(err, ErrValidation) {
			t.Errorf("%v is not ErrValidation", err)
		}
	}
}

func TestPlainValidationErrorIsNotBadAmount(t *testing.T) {
	if errors.Is(NewValidationError("order_id", "must be a UUID"), ErrBadAmount) {
		t.Error("a non-money validation error must not be ErrBadAmount")
	}
}

func validRequest() ReserveRequest {
	return ReserveRequest{
		UserID:         uuid.New(),
		OrderID:        uuid.New(),
		OrderType:      OrderTypeLimit,
		QtyWh:          1000,
		MaxPricePerKwh: 4550,
	}
}

func TestReserveRequestValidate(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	past := now.Add(-time.Second)
	future := now.Add(time.Hour)

	tests := []struct {
		name    string
		change  func(*ReserveRequest)
		wantErr error
	}{
		{name: "valid limit", change: func(*ReserveRequest) {}},
		{name: "valid market with collar", change: func(r *ReserveRequest) { r.OrderType = OrderTypeMarket }},
		{name: "valid future expiry", change: func(r *ReserveRequest) { r.ExpiresAt = &future }},
		{name: "market without collar", change: func(r *ReserveRequest) {
			r.OrderType = OrderTypeMarket
			r.MaxPricePerKwh = 0
		}, wantErr: ErrMarketOrderNeedsCollar},
		{name: "market with negative collar", change: func(r *ReserveRequest) {
			r.OrderType = OrderTypeMarket
			r.MaxPricePerKwh = -1
		}, wantErr: ErrBadAmount},
		{name: "limit without price", change: func(r *ReserveRequest) { r.MaxPricePerKwh = 0 }, wantErr: ErrBadAmount},
		{name: "zero quantity", change: func(r *ReserveRequest) { r.QtyWh = 0 }, wantErr: ErrBadAmount},
		{name: "negative quantity", change: func(r *ReserveRequest) { r.QtyWh = -1 }, wantErr: ErrBadAmount},
		{name: "unknown order type", change: func(r *ReserveRequest) { r.OrderType = "NOPE" }, wantErr: ErrValidation},
		{name: "expiry in the past", change: func(r *ReserveRequest) { r.ExpiresAt = &past }, wantErr: ErrValidation},
		{name: "expiry exactly now", change: func(r *ReserveRequest) { r.ExpiresAt = &now }, wantErr: ErrValidation},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := validRequest()
			tt.change(&request)

			err := request.Validate(now)

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestHoldMatchesReservation(t *testing.T) {
	walletID := uuid.New()
	request := validRequest()
	orderType := request.OrderType
	qty := request.QtyWh
	price := request.MaxPricePerKwh
	otherType := OrderTypeMarket
	otherQty := qty + 1
	otherPrice := price + 1
	hold := Hold{WalletID: walletID, OrderType: &orderType, QtyWh: &qty, MaxPricePerKwh: &price}

	if !hold.MatchesReservation(walletID, request) {
		t.Error("identical parameters must match")
	}

	expiry := time.Now()
	request.ExpiresAt = &expiry
	if !hold.MatchesReservation(walletID, request) {
		t.Error("expiry is a safety net, not part of the money, so it must not cause a conflict")
	}

	tests := []struct {
		name     string
		hold     Hold
		walletID uuid.UUID
	}{
		{name: "different user's wallet", hold: hold, walletID: uuid.New()},
		{name: "different type", hold: Hold{WalletID: walletID, OrderType: &otherType, QtyWh: &qty, MaxPricePerKwh: &price}, walletID: walletID},
		{name: "different quantity", hold: Hold{WalletID: walletID, OrderType: &orderType, QtyWh: &otherQty, MaxPricePerKwh: &price}, walletID: walletID},
		{name: "different price", hold: Hold{WalletID: walletID, OrderType: &orderType, QtyWh: &qty, MaxPricePerKwh: &otherPrice}, walletID: walletID},
		{name: "not an order hold", hold: Hold{WalletID: walletID}, walletID: walletID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.hold.MatchesReservation(tt.walletID, request) {
				t.Error("must not match")
			}
		})
	}
}

func TestHoldReleaseOutcome(t *testing.T) {
	tests := []struct {
		name         string
		hold         Hold
		wantLeftover int64
		wantStatus   HoldStatus
	}{
		{name: "untouched", hold: Hold{AmountTotal: 1000, Status: HoldActive}, wantLeftover: 1000, wantStatus: HoldReleased},
		{name: "partly filled", hold: Hold{AmountTotal: 1000, AmountConsumed: 400, Status: HoldActive}, wantLeftover: 600, wantStatus: HoldReleased},
		{name: "fully consumed", hold: Hold{AmountTotal: 1000, AmountConsumed: 1000, Status: HoldActive}, wantLeftover: 0, wantStatus: HoldConsumed},
		{name: "already released", hold: Hold{AmountTotal: 1000, AmountReleased: 1000, Status: HoldReleased}, wantLeftover: 0, wantStatus: HoldReleased},
		{name: "already consumed", hold: Hold{AmountTotal: 1000, AmountConsumed: 1000, Status: HoldConsumed}, wantLeftover: 0, wantStatus: HoldConsumed},
		{name: "already expired", hold: Hold{AmountTotal: 1000, AmountReleased: 1000, Status: HoldExpired}, wantLeftover: 0, wantStatus: HoldExpired},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leftover, status := tt.hold.ReleaseOutcome()

			if leftover != tt.wantLeftover || status != tt.wantStatus {
				t.Errorf("ReleaseOutcome() = %d, %s, want %d, %s", leftover, status, tt.wantLeftover, tt.wantStatus)
			}
		})
	}
}

func TestReleaseReasonValidate(t *testing.T) {
	for _, reason := range []ReleaseReason{ReleaseCancelled, ReleaseExpired, ReleaseFilled, ReleaseRejected} {
		if err := reason.Validate(); err != nil {
			t.Errorf("%s: unexpected error %v", reason, err)
		}
	}

	for _, reason := range []ReleaseReason{"", "NOPE"} {
		if err := reason.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%q: error = %v, want ErrValidation", reason, err)
		}
	}
}
