package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validSettleRequest() SettleRequest {
	return SettleRequest{
		TradeID:      uuid.New(),
		BuyOrderID:   uuid.New(),
		SellOrderID:  uuid.New(),
		BuyerUserID:  uuid.New(),
		SellerUserID: uuid.New(),
		QtyWh:        1000,
		PricePerKwh:  4550,
		ExecutedAt:   time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC),
	}
}

func TestSettleRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*SettleRequest)
		wantErr error
	}{
		{name: "valid", change: func(*SettleRequest) {}},
		{name: "no trade id", change: func(r *SettleRequest) { r.TradeID = uuid.Nil }, wantErr: ErrValidation},
		{name: "no buy order", change: func(r *SettleRequest) { r.BuyOrderID = uuid.Nil }, wantErr: ErrValidation},
		{name: "no sell order", change: func(r *SettleRequest) { r.SellOrderID = uuid.Nil }, wantErr: ErrValidation},
		{name: "no buyer", change: func(r *SettleRequest) { r.BuyerUserID = uuid.Nil }, wantErr: ErrValidation},
		{name: "no seller", change: func(r *SettleRequest) { r.SellerUserID = uuid.Nil }, wantErr: ErrValidation},
		{name: "self trade", change: func(r *SettleRequest) { r.SellerUserID = r.BuyerUserID }, wantErr: ErrValidation},
		{name: "zero quantity", change: func(r *SettleRequest) { r.QtyWh = 0 }, wantErr: ErrBadAmount},
		{name: "negative price", change: func(r *SettleRequest) { r.PricePerKwh = -1 }, wantErr: ErrBadAmount},
		{name: "no execution time", change: func(r *SettleRequest) { r.ExecutedAt = time.Time{} }, wantErr: ErrValidation},
		{name: "executed slightly ahead of our clock", change: func(r *SettleRequest) { r.ExecutedAt = r.ExecutedAt.Add(time.Hour + 30*time.Second) }},
		{name: "executed in the future", change: func(r *SettleRequest) { r.ExecutedAt = r.ExecutedAt.Add(time.Hour + 2*time.Minute) }, wantErr: ErrValidation},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := validSettleRequest()
			tt.change(&request)

			if err := request.Validate(validSettleRequest().ExecutedAt.Add(time.Hour)); !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// orderHold is a 1 kWh limit hold at 45.50 with a locked 1% buyer fee:
// value 4550 + fee 46 + buffer 50.
func orderHold(walletID uuid.UUID) Hold {
	orderType, qty, price, bps := OrderTypeLimit, int64(1000), int64(4550), int32(100)
	return Hold{
		WalletID: walletID, Kind: HoldKindOrder, Status: HoldActive,
		OrderType: &orderType, QtyWh: &qty, MaxPricePerKwh: &price, BuyerFeeBps: &bps,
		AmountTotal: 4646,
	}
}

func TestPlanSettlement(t *testing.T) {
	tests := []struct {
		name      string
		hold      func(Hold) Hold
		change    func(*SettleRequest)
		sellerBps int32
		want      SettlementPlan
	}{
		{
			name: "fill at the limit, not final", sellerBps: 100,
			want: SettlementPlan{
				Split:      FeeSplit{Value: 4550, BuyerFee: 46, SellerFee: 46, BuyerDebit: 4596, SellerCredit: 4504, PlatformCredit: 92},
				HoldStatus: HoldActive,
			},
		},
		{
			name: "final fill returns what is left", sellerBps: 100,
			change: func(r *SettleRequest) { r.BuyOrderFinal = true },
			want: SettlementPlan{
				Split:      FeeSplit{Value: 4550, BuyerFee: 46, SellerFee: 46, BuyerDebit: 4596, SellerCredit: 4504, PlatformCredit: 92},
				Released:   50,
				HoldStatus: HoldReleased,
			},
		},
		{
			name: "price improvement leaves more to return", sellerBps: 100,
			change: func(r *SettleRequest) { r.PricePerKwh, r.BuyOrderFinal = 4000, true },
			want: SettlementPlan{
				Split:      FeeSplit{Value: 4000, BuyerFee: 40, SellerFee: 40, BuyerDebit: 4040, SellerCredit: 3960, PlatformCredit: 80},
				Released:   606,
				HoldStatus: HoldReleased,
			},
		},
		{
			name: "buyer pays the locked rate, seller the given one", sellerBps: 500,
			want: SettlementPlan{
				Split:      FeeSplit{Value: 4550, BuyerFee: 46, SellerFee: 228, BuyerDebit: 4596, SellerCredit: 4322, PlatformCredit: 274},
				HoldStatus: HoldActive,
			},
		},
		{
			name: "final fill that uses the whole hold is consumed", sellerBps: 100,
			hold:   func(h Hold) Hold { h.AmountTotal = 4596; return h },
			change: func(r *SettleRequest) { r.BuyOrderFinal = true },
			want: SettlementPlan{
				Split:      FeeSplit{Value: 4550, BuyerFee: 46, SellerFee: 46, BuyerDebit: 4596, SellerCredit: 4504, PlatformCredit: 92},
				HoldStatus: HoldConsumed,
			},
		},
		{
			name: "partial fill draws from what is left after earlier fills", sellerBps: 100,
			hold:   func(h Hold) Hold { h.AmountConsumed = 2000; return h },
			change: func(r *SettleRequest) { r.QtyWh, r.BuyOrderFinal = 500, true },
			want: SettlementPlan{
				Split:      FeeSplit{Value: 2275, BuyerFee: 23, SellerFee: 23, BuyerDebit: 2298, SellerCredit: 2252, PlatformCredit: 46},
				Released:   348,
				HoldStatus: HoldReleased,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hold := orderHold(uuid.New())
			if tt.hold != nil {
				hold = tt.hold(hold)
			}
			request := validSettleRequest()
			if tt.change != nil {
				tt.change(&request)
			}

			got, err := hold.PlanSettlement(request, tt.sellerBps)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("plan = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPlanSettlementRejections(t *testing.T) {
	tests := []struct {
		name    string
		hold    func(Hold) Hold
		change  func(*SettleRequest)
		wantErr error
	}{
		{name: "fill above the reserved price", change: func(r *SettleRequest) { r.PricePerKwh = 4551 }, wantErr: ErrPriceExceedsReservation},
		{name: "hold cannot cover the fill", hold: func(h Hold) Hold { h.AmountConsumed = 100; return h }, change: func(r *SettleRequest) { r.QtyWh = 1000 }, wantErr: ErrHoldInsufficient},
		{name: "released hold", hold: func(h Hold) Hold { h.Status = HoldReleased; return h }, wantErr: ErrHoldNotFound},
		{name: "expired hold", hold: func(h Hold) Hold { h.Status = HoldExpired; return h }, wantErr: ErrHoldNotFound},
		{name: "consumed hold", hold: func(h Hold) Hold { h.Status = HoldConsumed; return h }, wantErr: ErrHoldNotFound},
		{name: "withdrawal hold", hold: func(h Hold) Hold { h.Kind = HoldKindWithdrawal; return h }, wantErr: ErrHoldNotFound},
		{name: "trade too small to be worth anything", change: func(r *SettleRequest) { r.QtyWh, r.PricePerKwh = 1, 1 }, wantErr: ErrBadAmount},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hold := orderHold(uuid.New())
			if tt.hold != nil {
				hold = tt.hold(hold)
			}
			request := validSettleRequest()
			if tt.change != nil {
				tt.change(&request)
			}

			if _, err := hold.PlanSettlement(request, 100); !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestSettlementLines(t *testing.T) {
	buyer, seller, revenue := uuid.New(), uuid.New(), uuid.New()

	t.Run("three lines that balance", func(t *testing.T) {
		lines := SettlementLines(buyer, seller, revenue, FeeSplit{BuyerDebit: 4596, SellerCredit: 4504, PlatformCredit: 92})

		want := []LedgerLine{{AccountID: buyer, Amount: -4596}, {AccountID: seller, Amount: 4504}, {AccountID: revenue, Amount: 92}}
		if len(lines) != len(want) {
			t.Fatalf("lines = %+v", lines)
		}
		for i := range want {
			if lines[i].AccountID != want[i].AccountID || lines[i].Amount != want[i].Amount {
				t.Errorf("line %d = %+v, want %+v", i, lines[i], want[i])
			}
		}
		if err := ValidateBalanced(lines); err != nil {
			t.Errorf("unbalanced: %v", err)
		}
	})

	// ledger_lines refuses a zero amount, so a fee that rounds to nothing has no line.
	t.Run("no revenue line when both fees round to zero", func(t *testing.T) {
		lines := SettlementLines(buyer, seller, revenue, FeeSplit{BuyerDebit: 49, SellerCredit: 49})

		if len(lines) != 2 {
			t.Fatalf("lines = %+v, want buyer and seller only", lines)
		}
		if err := ValidateBalanced(lines); err != nil {
			t.Errorf("unbalanced: %v", err)
		}
	})
}

func TestTradeSettlementMatchesRequest(t *testing.T) {
	request := validSettleRequest()
	buyerWallet, sellerWallet := uuid.New(), uuid.New()
	settled := TradeSettlement{
		TradeID: request.TradeID, BuyerWalletID: buyerWallet, SellerWalletID: sellerWallet,
		BuyOrderID: request.BuyOrderID, SellOrderID: request.SellOrderID,
		QtyWh: request.QtyWh, PricePerKwh: request.PricePerKwh,
	}

	if !settled.MatchesRequest(request, false, buyerWallet, sellerWallet) {
		t.Error("identical trade must match")
	}

	// A retry that turns this into the order's last fill would never return the
	// rest of the hold, so it is a different request.
	final := request
	final.BuyOrderFinal = true
	if settled.MatchesRequest(final, false, buyerWallet, sellerWallet) {
		t.Error("a retry that changes buy_order_final must not match")
	}

	tests := []struct {
		name         string
		change       func(*SettleRequest)
		buyer, sellr uuid.UUID
	}{
		{"different quantity", func(r *SettleRequest) { r.QtyWh++ }, buyerWallet, sellerWallet},
		{"different price", func(r *SettleRequest) { r.PricePerKwh++ }, buyerWallet, sellerWallet},
		{"different buy order", func(r *SettleRequest) { r.BuyOrderID = uuid.New() }, buyerWallet, sellerWallet},
		{"different sell order", func(r *SettleRequest) { r.SellOrderID = uuid.New() }, buyerWallet, sellerWallet},
		{"different buyer", func(*SettleRequest) {}, uuid.New(), sellerWallet},
		{"different seller", func(*SettleRequest) {}, buyerWallet, uuid.New()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := request
			tt.change(&changed)

			if settled.MatchesRequest(changed, false, tt.buyer, tt.sellr) {
				t.Error("must not match")
			}
		})
	}
}
