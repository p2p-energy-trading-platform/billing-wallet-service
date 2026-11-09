//go:build integration

package integration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// One kWh at 45.50 credits: value 4550 + 1% fee 46 + rounding buffer 50.
const (
	testQtyWh       = 1000
	testPrice       = 4550
	testOrderAmount = 4646
)

type reservationEnv struct {
	db         *sql.DB
	orders     billingv1.OrderBillingServiceClient
	admin      billingv1.AdminBillingServiceClient
	serviceURL string
}

func newReservationEnv(t *testing.T) *reservationEnv {
	t.Helper()

	database := migratedPostgres(t)

	conn := startServer(t, database.serviceURL)

	return &reservationEnv{
		db:         database.db,
		orders:     billingv1.NewOrderBillingServiceClient(conn),
		admin:      billingv1.NewAdminBillingServiceClient(conn),
		serviceURL: database.serviceURL,
	}
}

// fund creates a wallet holding the balance. Deposits do not exist yet, so this
// writes the row directly.
func (e *reservationEnv) fund(t *testing.T, available int64) uuid.UUID {
	t.Helper()

	userID := uuid.New()
	exec(t, e.db, `INSERT INTO wallets (user_id, available) VALUES ($1, $2)`, userID, available)

	return userID
}

func limitOrder(userID, orderID uuid.UUID) *billingv1.ReserveCreditsRequest {
	return &billingv1.ReserveCreditsRequest{
		UserId:         userID.String(),
		OrderId:        orderID.String(),
		OrderType:      billingv1.OrderType_ORDER_TYPE_LIMIT,
		QtyWh:          testQtyWh,
		MaxPricePerKwh: testPrice,
	}
}

func (e *reservationEnv) reserve(t *testing.T, request *billingv1.ReserveCreditsRequest) *billingv1.ReserveCreditsResponse {
	t.Helper()

	response, err := e.orders.ReserveCredits(context.Background(), request)
	if err != nil {
		t.Fatalf("ReserveCredits: %v", err)
	}

	return response
}

func (e *reservationEnv) release(t *testing.T, orderID uuid.UUID) int64 {
	t.Helper()

	response, err := e.orders.ReleaseReservation(context.Background(), &billingv1.ReleaseReservationRequest{
		OrderId: orderID.String(),
		Reason:  billingv1.ReleaseReason_RELEASE_REASON_CANCELLED,
	})
	if err != nil {
		t.Fatalf("ReleaseReservation: %v", err)
	}

	return response.GetReleasedAmount()
}

func (e *reservationEnv) balances(t *testing.T, userID uuid.UUID) (available, reserved int64) {
	t.Helper()

	err := e.db.QueryRowContext(context.Background(),
		`SELECT available, reserved FROM wallets WHERE user_id = $1`, userID).Scan(&available, &reserved)
	if err != nil {
		t.Fatalf("read balances: %v", err)
	}

	return available, reserved
}

func (e *reservationEnv) assertBalances(t *testing.T, userID uuid.UUID, wantAvailable, wantReserved int64) {
	t.Helper()

	available, reserved := e.balances(t, userID)
	if available != wantAvailable || reserved != wantReserved {
		t.Errorf("balances = %d available, %d reserved, want %d, %d", available, reserved, wantAvailable, wantReserved)
	}
}

// assertReservedMatchesHolds checks the reconciliation invariant from the plan:
// wallet.reserved equals the remaining amount of its ACTIVE holds.
func (e *reservationEnv) assertReservedMatchesHolds(t *testing.T, userID uuid.UUID) {
	t.Helper()

	var reserved, remaining int64
	err := e.db.QueryRowContext(context.Background(),
		`SELECT w.reserved,
		        COALESCE((SELECT SUM(h.amount_total - h.amount_consumed - h.amount_released)
		                  FROM holds h WHERE h.wallet_id = w.id AND h.status = 'ACTIVE'), 0)
		 FROM wallets w WHERE w.user_id = $1`, userID).Scan(&reserved, &remaining)
	if err != nil {
		t.Fatalf("read reservation invariant: %v", err)
	}
	if reserved != remaining {
		t.Errorf("wallet.reserved = %d, active holds remaining = %d", reserved, remaining)
	}
}

func assertRejected(t *testing.T, err error, wantCode codes.Code, wantReason string) {
	t.Helper()

	if code := status.Code(err); code != wantCode {
		t.Fatalf("code = %v (%v), want %v", code, err, wantCode)
	}

	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok {
			if info.GetReason() != wantReason {
				t.Errorf("reason = %q, want %q", info.GetReason(), wantReason)
			}
			return
		}
	}

	t.Errorf("error %v has no ErrorInfo, want reason %q", err, wantReason)
}

// Settlement does not exist yet, so a fill is simulated the way it will look:
// consumed on the hold, gone from the wallet's reserved balance.
func (e *reservationEnv) simulateFill(t *testing.T, userID, orderID uuid.UUID, consumed int64) {
	t.Helper()

	exec(t, e.db, `UPDATE holds SET amount_consumed = amount_consumed + $1 WHERE ref_id = $2`, consumed, orderID)
	exec(t, e.db, `UPDATE wallets SET reserved = reserved - $1 WHERE user_id = $2`, consumed, userID)
}
