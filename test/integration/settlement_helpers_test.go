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

// fill describes one trade against a buyer's order, at the limit price by default.
func fill(buyer, seller, buyOrder uuid.UUID, qtyWh, price int64, final bool) *billingv1.SettleTradeRequest {
	return &billingv1.SettleTradeRequest{
		TradeId:       uuid.NewString(),
		BuyOrderId:    buyOrder.String(),
		SellOrderId:   uuid.NewString(),
		BuyerUserId:   buyer.String(),
		SellerUserId:  seller.String(),
		QtyWh:         qtyWh,
		PricePerKwh:   price,
		BuyOrderFinal: final,
		ExecutedAt:    timestamppb.New(time.Now()),
	}
}

func (e *reservationEnv) settle(t *testing.T, request *billingv1.SettleTradeRequest) *billingv1.SettleTradeResponse {
	t.Helper()

	response, err := e.orders.SettleTrade(context.Background(), request)
	if err != nil {
		t.Fatalf("SettleTrade: %v", err)
	}

	return response
}

// buyOrder funds a buyer and reserves one 1 kWh limit order at 45.50, which locks
// testOrderAmount (4646).
func (e *reservationEnv) buyOrder(t *testing.T, available int64) (buyer, orderID uuid.UUID) {
	t.Helper()

	buyer, orderID = e.fund(t, available), uuid.New()
	e.reserve(t, limitOrder(buyer, orderID))

	return buyer, orderID
}

func (e *reservationEnv) count(t *testing.T, query string, args ...any) int {
	t.Helper()

	var n int
	if err := e.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}

	return n
}

func (e *reservationEnv) platformRevenue(t *testing.T) int64 {
	t.Helper()

	var revenue int64
	err := e.db.QueryRowContext(context.Background(),
		`SELECT COALESCE(SUM(l.amount), 0) FROM ledger_lines l
		 JOIN ledger_accounts a ON a.id = l.account_id WHERE a.type = 'PLATFORM_REVENUE'`).Scan(&revenue)
	if err != nil {
		t.Fatalf("read platform revenue: %v", err)
	}

	return revenue
}

// assertBooksBalance checks that every journal sums to zero, and that credits are
// conserved: what the wallets hold plus what the platform earned equals what was
// funded.
func (e *reservationEnv) assertBooksBalance(t *testing.T, funded int64) {
	t.Helper()

	if unbalanced := e.count(t, `SELECT count(*) FROM (SELECT journal_id FROM ledger_lines GROUP BY journal_id HAVING SUM(amount) <> 0) j`); unbalanced != 0 {
		t.Errorf("%d journals do not sum to zero", unbalanced)
	}

	var inWallets int64
	if err := e.db.QueryRowContext(context.Background(), `SELECT COALESCE(SUM(available + reserved), 0) FROM wallets`).Scan(&inWallets); err != nil {
		t.Fatalf("sum wallets: %v", err)
	}
	if revenue := e.platformRevenue(t); inWallets+revenue != funded {
		t.Errorf("wallets %d + revenue %d = %d, want %d funded", inWallets, revenue, inWallets+revenue, funded)
	}
}
