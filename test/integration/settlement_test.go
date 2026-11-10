//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSettleMovesMoneyFromBuyerToSellerAndPlatform(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	seller := uuid.New() // no wallet yet: settlement creates one

	request := fill(buyer, seller, order, 1000, testPrice, false)
	response := env.settle(t, request)

	// value 4550, 1% fee each side 46: buyer pays 4596, seller gets 4504, platform 92
	if response.GetTradeValue() != 4550 || response.GetBuyerFee() != 46 || response.GetSellerFee() != 46 ||
		response.GetBuyerReleasedRemainder() != 0 || response.GetAlreadySettled() {
		t.Errorf("response = %v", response)
	}
	env.assertBalances(t, buyer, 10_000-testOrderAmount, testOrderAmount-4596)
	env.assertBalances(t, seller, 4504, 0)
	env.assertReservedMatchesHolds(t, buyer)
	if revenue := env.platformRevenue(t); revenue != 92 {
		t.Errorf("platform revenue = %d, want 92", revenue)
	}
	env.assertBooksBalance(t, 10_000)

	holdStatus, consumed, released := env.holdState(t, order)
	if holdStatus != "ACTIVE" || consumed != 4596 || released != 0 {
		t.Errorf("hold = %s, consumed %d, released %d", holdStatus, consumed, released)
	}

	var buyerBps, sellerBps, lines int
	var journalID uuid.UUID
	err := env.db.QueryRowContext(context.Background(),
		`SELECT journal_id, buyer_fee_bps, seller_fee_bps FROM trade_settlements WHERE trade_id = $1`, request.GetTradeId()).
		Scan(&journalID, &buyerBps, &sellerBps)
	if err != nil {
		t.Fatalf("read settlement: %v", err)
	}
	if journalID.String() != response.GetSettlementId() || buyerBps != 100 || sellerBps != 100 {
		t.Errorf("settlement journal %s, rates %d/%d", journalID, buyerBps, sellerBps)
	}
	lines = env.count(t, `SELECT count(*) FROM ledger_lines WHERE journal_id = $1`, journalID)
	if lines != 3 {
		t.Errorf("ledger lines = %d, want 3", lines)
	}

	var buyerAfter, sellerAfter int64
	err = env.db.QueryRowContext(context.Background(),
		`SELECT
		   (SELECT l.balance_after FROM ledger_lines l JOIN ledger_accounts a ON a.id = l.account_id JOIN wallets w ON w.id = a.wallet_id WHERE l.journal_id = $1 AND w.user_id = $2),
		   (SELECT l.balance_after FROM ledger_lines l JOIN ledger_accounts a ON a.id = l.account_id JOIN wallets w ON w.id = a.wallet_id WHERE l.journal_id = $1 AND w.user_id = $3)`,
		journalID, buyer, seller).Scan(&buyerAfter, &sellerAfter)
	if err != nil {
		t.Fatalf("read balance_after: %v", err)
	}
	if buyerAfter != 10_000-4596 || sellerAfter != 4504 {
		t.Errorf("balance_after = %d buyer, %d seller", buyerAfter, sellerAfter)
	}

	if n := env.count(t, `SELECT count(*) FROM outbox WHERE topic = 'billing.trade.settled' AND key = $1`, request.GetTradeId()); n != 1 {
		t.Errorf("trade.settled events = %d, want 1", n)
	}
	if n := env.count(t, `SELECT count(*) FROM outbox WHERE topic = 'billing.wallet.updated' AND payload->>'trade_id' = $1`, request.GetTradeId()); n != 2 {
		t.Errorf("balance events for the trade = %d, want one per side", n)
	}
}

func TestFinalFillReturnsTheRestOfTheHold(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	seller := env.fund(t, 0)

	response := env.settle(t, fill(buyer, seller, order, 1000, testPrice, true))

	if response.GetBuyerReleasedRemainder() != 50 {
		t.Errorf("released = %d, want the 50 buffer", response.GetBuyerReleasedRemainder())
	}
	env.assertBalances(t, buyer, 10_000-4596, 0)
	if holdStatus, _, released := env.holdState(t, order); holdStatus != "RELEASED" || released != 50 {
		t.Errorf("hold = %s with %d released", holdStatus, released)
	}
	env.assertBooksBalance(t, 10_000)
}

func TestPriceImprovementReturnsTheDifference(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	seller := env.fund(t, 0)

	response := env.settle(t, fill(buyer, seller, order, 1000, 4000, true))

	// value 4000 + fee 40 = 4040 paid, 4646 - 4040 = 606 returned
	if response.GetTradeValue() != 4000 || response.GetBuyerReleasedRemainder() != 606 {
		t.Errorf("response = %v", response)
	}
	env.assertBalances(t, buyer, 10_000-4040, 0)
	env.assertBalances(t, seller, 3960, 0)
	env.assertBooksBalance(t, 10_000)
}

func TestPartialFillsAcrossSeveralTrades(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	seller := env.fund(t, 0)

	env.settle(t, fill(buyer, seller, order, 400, testPrice, false))
	env.settle(t, fill(buyer, seller, order, 300, testPrice, false))
	last := env.settle(t, fill(buyer, seller, order, 300, testPrice, true))

	// each fill: 400 Wh = 1820 + 18, 300 Wh = 1365 + 14 (13.65 rounds up)
	paid := int64(1820 + 18 + 1365 + 14 + 1365 + 14)
	if last.GetBuyerReleasedRemainder() != testOrderAmount-paid {
		t.Errorf("released = %d, want %d", last.GetBuyerReleasedRemainder(), testOrderAmount-paid)
	}
	env.assertBalances(t, buyer, 10_000-paid, 0)
	env.assertReservedMatchesHolds(t, buyer)
	env.assertBooksBalance(t, 10_000)
}

func TestMarketBuyFillsBelowItsCollar(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.fund(t, 10_000), uuid.New()
	request := limitOrder(buyer, order)
	request.OrderType = billingv1.OrderType_ORDER_TYPE_MARKET
	request.QtyWh, request.MaxPricePerKwh = 500, 6000
	if amount := env.reserve(t, request).GetReservedAmount(); amount != 3080 {
		t.Fatalf("reserved %d, want 3080", amount)
	}
	seller := env.fund(t, 0)

	response := env.settle(t, fill(buyer, seller, order, 500, testPrice, true))

	// value 2275 + fee 23 = 2298 paid, 3080 - 2298 = 782 back
	if response.GetBuyerReleasedRemainder() != 782 {
		t.Errorf("released = %d, want 782", response.GetBuyerReleasedRemainder())
	}
	env.assertBalances(t, buyer, 10_000-2298, 0)
	env.assertBooksBalance(t, 10_000)
}

func TestSellerPaysTheRateInEffectWhenTheTradeExecuted(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	seller := env.fund(t, 0)
	beforeChange := time.Now()
	exec(t, env.db, `INSERT INTO fee_configs (buyer_fee_bps, seller_fee_bps, effective_from) VALUES (500, 500, now())`)

	early := fill(buyer, seller, order, 500, testPrice, false)
	early.ExecutedAt = timestamppb.New(beforeChange)
	late := fill(buyer, seller, order, 500, testPrice, true)
	late.ExecutedAt = timestamppb.New(time.Now())

	// 500 Wh = 2275: 1% is 23, 5% is 114
	if response := env.settle(t, early); response.GetSellerFee() != 23 || response.GetBuyerFee() != 23 {
		t.Errorf("before the change: fees = %d buyer, %d seller, want 23/23", response.GetBuyerFee(), response.GetSellerFee())
	}
	if response := env.settle(t, late); response.GetSellerFee() != 114 || response.GetBuyerFee() != 23 {
		t.Errorf("after the change: fees = %d buyer, %d seller, want the locked 23 and the new 114", response.GetBuyerFee(), response.GetSellerFee())
	}
	env.assertBooksBalance(t, 10_000)
}

func TestNoRevenueLineWhenFeesAreZero(t *testing.T) {
	env := newReservationEnv(t)
	exec(t, env.db, `INSERT INTO fee_configs (buyer_fee_bps, seller_fee_bps, effective_from) VALUES (0, 0, now())`)
	buyer, order := env.buyOrder(t, 10_000)
	seller := env.fund(t, 0)

	response := env.settle(t, fill(buyer, seller, order, 1000, testPrice, true))

	if n := env.count(t, `SELECT count(*) FROM ledger_lines WHERE journal_id = $1`, response.GetSettlementId()); n != 2 {
		t.Errorf("ledger lines = %d, want buyer and seller only", n)
	}
	env.assertBalances(t, seller, 4550, 0)
	env.assertBooksBalance(t, 10_000)
}

func TestFrozenWalletsStillSettle(t *testing.T) {
	env := newReservationEnv(t)
	buyer, order := env.buyOrder(t, 10_000)
	seller := env.fund(t, 0)
	exec(t, env.db, `UPDATE wallets SET status = 'FROZEN' WHERE user_id IN ($1, $2)`, buyer, seller)

	env.settle(t, fill(buyer, seller, order, 1000, testPrice, true))

	env.assertBalances(t, buyer, 10_000-4596, 0)
	env.assertBalances(t, seller, 4504, 0)
}
