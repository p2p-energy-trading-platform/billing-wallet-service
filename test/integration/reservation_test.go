//go:build integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// One kWh at 45.50 credits: value 4550 + 1% fee 46 + rounding buffer 50.
const (
	testQtyWh       = 1000
	testPrice       = 4550
	testOrderAmount = 4646
)

type reservationEnv struct {
	db     *sql.DB
	orders billingv1.OrderBillingServiceClient
}

func newReservationEnv(t *testing.T) *reservationEnv {
	t.Helper()

	database := migratedPostgres(t)

	return &reservationEnv{
		db:     database.db,
		orders: billingv1.NewOrderBillingServiceClient(startServer(t, database.serviceURL)),
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

func TestReserveLocksFundsAndRecordsTheHold(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)

	request := limitOrder(userID, orderID)
	request.ExpiresAt = timestamppb.New(expiresAt)
	response := env.reserve(t, request)

	if response.GetReservedAmount() != testOrderAmount {
		t.Errorf("reserved_amount = %d, want %d", response.GetReservedAmount(), testOrderAmount)
	}
	if response.GetAvailableAfter() != 10_000-testOrderAmount {
		t.Errorf("available_after = %d, want %d", response.GetAvailableAfter(), 10_000-testOrderAmount)
	}
	env.assertBalances(t, userID, 10_000-testOrderAmount, testOrderAmount)
	env.assertReservedMatchesHolds(t, userID)

	var (
		kind, holdStatus, orderType string
		qty, price, total           int64
		bps                         int
		configID, activeConfigID    uuid.UUID
		storedExpiry                time.Time
	)
	ctx := context.Background()
	err := env.db.QueryRowContext(ctx,
		`SELECT kind, status, order_type, qty_wh, max_price_per_kwh, amount_total, buyer_fee_bps, buyer_fee_config_id, expires_at
		 FROM holds WHERE id = $1 AND ref_id = $2`, response.GetHoldId(), orderID).
		Scan(&kind, &holdStatus, &orderType, &qty, &price, &total, &bps, &configID, &storedExpiry)
	if err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if kind != "ORDER" || holdStatus != "ACTIVE" || orderType != "LIMIT" {
		t.Errorf("hold = %s/%s/%s, want ORDER/ACTIVE/LIMIT", kind, holdStatus, orderType)
	}
	if qty != testQtyWh || price != testPrice || total != testOrderAmount || bps != 100 {
		t.Errorf("hold values = %d/%d/%d/%d", qty, price, total, bps)
	}
	if !storedExpiry.Equal(expiresAt) {
		t.Errorf("expires_at = %v, want %v", storedExpiry, expiresAt)
	}
	if err := env.db.QueryRowContext(ctx, `SELECT id FROM fee_configs LIMIT 1`).Scan(&activeConfigID); err != nil {
		t.Fatalf("read fee config: %v", err)
	}
	if configID != activeConfigID {
		t.Errorf("buyer_fee_config_id = %v, want %v", configID, activeConfigID)
	}
}

func TestReserveMovesNoMoneyInTheLedger(t *testing.T) {
	env := newReservationEnv(t)

	env.reserve(t, limitOrder(env.fund(t, 10_000), uuid.New()))

	var journals, lines int
	ctx := context.Background()
	if err := env.db.QueryRowContext(ctx, `SELECT count(*) FROM journals`).Scan(&journals); err != nil {
		t.Fatalf("count journals: %v", err)
	}
	if err := env.db.QueryRowContext(ctx, `SELECT count(*) FROM ledger_lines`).Scan(&lines); err != nil {
		t.Fatalf("count lines: %v", err)
	}
	if journals != 0 || lines != 0 {
		t.Errorf("journals = %d, lines = %d, want none: a hold only encumbers the user's own money", journals, lines)
	}
}

func TestReserveAndReleaseEachPublishABalanceEvent(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()

	env.reserve(t, limitOrder(userID, orderID))
	env.release(t, orderID)
	env.release(t, orderID) // moved nothing, so it publishes nothing

	rows, err := env.db.QueryContext(context.Background(),
		`SELECT topic, key, payload FROM outbox WHERE published_at IS NULL ORDER BY id`)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()

	type event struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
		UserID    string `json:"user_id"`
		Available int64  `json:"available"`
		Reserved  int64  `json:"reserved"`
		OrderID   string `json:"order_id"`
		Reason    string `json:"reason"`
	}
	var events []event
	for rows.Next() {
		var topic, key string
		var payload []byte
		if err := rows.Scan(&topic, &key, &payload); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		if topic != "billing.wallet.updated" || key != userID.String() {
			t.Errorf("topic/key = %q/%q", topic, key)
		}
		var decoded event
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		events = append(events, decoded)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox rows: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].EventID == "" || events[0].EventID == events[1].EventID {
		t.Errorf("event ids must be set and unique: %q, %q", events[0].EventID, events[1].EventID)
	}
	if events[0].EventType != "wallet.balance_changed" || events[0].Reserved != testOrderAmount || events[0].Available != 10_000-testOrderAmount {
		t.Errorf("reserve event = %+v", events[0])
	}
	if events[1].Reserved != 0 || events[1].Available != 10_000 || events[1].Reason != "CANCELLED" || events[1].OrderID != orderID.String() {
		t.Errorf("release event = %+v", events[1])
	}
}

func TestMarketOrderReservesAtTheCollar(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)

	request := limitOrder(userID, uuid.New())
	request.OrderType = billingv1.OrderType_ORDER_TYPE_MARKET
	response := env.reserve(t, request)

	if response.GetReservedAmount() != testOrderAmount {
		t.Errorf("reserved_amount = %d, want %d", response.GetReservedAmount(), testOrderAmount)
	}

	var orderType string
	var collar int64
	err := env.db.QueryRowContext(context.Background(),
		`SELECT order_type, max_price_per_kwh FROM holds WHERE id = $1`, response.GetHoldId()).Scan(&orderType, &collar)
	if err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if orderType != "MARKET" || collar != testPrice {
		t.Errorf("hold = %s at %d, want MARKET at %d", orderType, collar, testPrice)
	}
}

func TestReserveRejections(t *testing.T) {
	tests := []struct {
		name       string
		change     func(*billingv1.ReserveCreditsRequest)
		wantCode   codes.Code
		wantReason string
	}{
		{"market without collar", func(r *billingv1.ReserveCreditsRequest) {
			r.OrderType = billingv1.OrderType_ORDER_TYPE_MARKET
			r.MaxPricePerKwh = 0
		}, codes.InvalidArgument, "MARKET_ORDER_NEEDS_COLLAR"},
		{"zero quantity", func(r *billingv1.ReserveCreditsRequest) { r.QtyWh = 0 }, codes.InvalidArgument, "BAD_AMOUNT"},
		{"limit without price", func(r *billingv1.ReserveCreditsRequest) { r.MaxPricePerKwh = 0 }, codes.InvalidArgument, "BAD_AMOUNT"},
		{"amount overflow", func(r *billingv1.ReserveCreditsRequest) {
			r.QtyWh = math.MaxInt64
			r.MaxPricePerKwh = math.MaxInt64
		}, codes.InvalidArgument, "BAD_AMOUNT"},
		{"no order type", func(r *billingv1.ReserveCreditsRequest) {
			r.OrderType = billingv1.OrderType_ORDER_TYPE_UNSPECIFIED
		}, codes.InvalidArgument, "VALIDATION_FAILED"},
		{"bad user id", func(r *billingv1.ReserveCreditsRequest) { r.UserId = "nope" }, codes.InvalidArgument, "VALIDATION_FAILED"},
		{"bad order id", func(r *billingv1.ReserveCreditsRequest) { r.OrderId = "nope" }, codes.InvalidArgument, "VALIDATION_FAILED"},
		{"expired already", func(r *billingv1.ReserveCreditsRequest) {
			r.ExpiresAt = timestamppb.New(time.Now().Add(-time.Hour))
		}, codes.InvalidArgument, "VALIDATION_FAILED"},
	}

	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := limitOrder(userID, uuid.New())
			tt.change(request)

			_, err := env.orders.ReserveCredits(context.Background(), request)

			assertRejected(t, err, tt.wantCode, tt.wantReason)
			env.assertBalances(t, userID, 10_000, 0)
		})
	}
}

func TestReserveRequiresEnoughAvailableFunds(t *testing.T) {
	env := newReservationEnv(t)

	t.Run("one unit short", func(t *testing.T) {
		userID := env.fund(t, testOrderAmount-1)

		_, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, uuid.New()))

		assertRejected(t, err, codes.FailedPrecondition, "INSUFFICIENT_FUNDS")
		env.assertBalances(t, userID, testOrderAmount-1, 0)
	})

	t.Run("exactly enough", func(t *testing.T) {
		userID := env.fund(t, testOrderAmount)

		env.reserve(t, limitOrder(userID, uuid.New()))

		env.assertBalances(t, userID, 0, testOrderAmount)
	})

	t.Run("funds already reserved do not count", func(t *testing.T) {
		userID := env.fund(t, testOrderAmount+1)
		env.reserve(t, limitOrder(userID, uuid.New()))

		_, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, uuid.New()))

		assertRejected(t, err, codes.FailedPrecondition, "INSUFFICIENT_FUNDS")
		env.assertBalances(t, userID, 1, testOrderAmount)
	})

	t.Run("a user with no wallet has no funds", func(t *testing.T) {
		_, err := env.orders.ReserveCredits(context.Background(), limitOrder(uuid.New(), uuid.New()))

		assertRejected(t, err, codes.FailedPrecondition, "INSUFFICIENT_FUNDS")
	})
}

func TestReserveRejectsAWalletThatIsNotActive(t *testing.T) {
	env := newReservationEnv(t)

	for _, walletStatus := range []string{"FROZEN", "CLOSED"} {
		t.Run(walletStatus, func(t *testing.T) {
			userID := env.fund(t, 10_000)
			exec(t, env.db, `UPDATE wallets SET status = $1 WHERE user_id = $2`, walletStatus, userID)

			_, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, uuid.New()))

			assertRejected(t, err, codes.FailedPrecondition, "WALLET_FROZEN")
			env.assertBalances(t, userID, 10_000, 0)
		})
	}
}

func TestReserveReplayReturnsTheOriginalHold(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()

	first := env.reserve(t, limitOrder(userID, orderID))
	second := env.reserve(t, limitOrder(userID, orderID))

	if first.GetHoldId() != second.GetHoldId() || second.GetReservedAmount() != testOrderAmount {
		t.Errorf("replay = %v, first = %v", second, first)
	}
	if second.GetAvailableAfter() != 10_000-testOrderAmount {
		t.Errorf("available_after on replay = %d", second.GetAvailableAfter())
	}
	env.assertBalances(t, userID, 10_000-testOrderAmount, testOrderAmount)
}

func TestReserveReplayOnAFrozenWalletStillSucceeds(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	first := env.reserve(t, limitOrder(userID, orderID))
	exec(t, env.db, `UPDATE wallets SET status = 'FROZEN' WHERE user_id = $1`, userID)

	second := env.reserve(t, limitOrder(userID, orderID))

	if first.GetHoldId() != second.GetHoldId() {
		t.Errorf("hold changed from %s to %s", first.GetHoldId(), second.GetHoldId())
	}
}

func TestReserveConflictsOnDifferentParameters(t *testing.T) {
	tests := []struct {
		name   string
		change func(*billingv1.ReserveCreditsRequest)
	}{
		{"quantity", func(r *billingv1.ReserveCreditsRequest) { r.QtyWh = testQtyWh + 1 }},
		{"price", func(r *billingv1.ReserveCreditsRequest) { r.MaxPricePerKwh = testPrice + 1 }},
		{"order type", func(r *billingv1.ReserveCreditsRequest) { r.OrderType = billingv1.OrderType_ORDER_TYPE_MARKET }},
	}

	env := newReservationEnv(t)
	userID := env.fund(t, 100_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := limitOrder(userID, orderID)
			tt.change(request)

			_, err := env.orders.ReserveCredits(context.Background(), request)

			assertRejected(t, err, codes.AlreadyExists, "IDEMPOTENCY_CONFLICT")
			env.assertBalances(t, userID, 100_000-testOrderAmount, testOrderAmount)
		})
	}

	t.Run("another user", func(t *testing.T) {
		other := env.fund(t, 100_000)

		_, err := env.orders.ReserveCredits(context.Background(), limitOrder(other, orderID))

		assertRejected(t, err, codes.AlreadyExists, "IDEMPOTENCY_CONFLICT")
		env.assertBalances(t, other, 100_000, 0)
	})
}

func TestFeeChangeOnlyAffectsNewReservations(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 100_000)
	openOrder := uuid.New()
	before := env.reserve(t, limitOrder(userID, openOrder))

	exec(t, env.db, `INSERT INTO fee_configs (buyer_fee_bps, seller_fee_bps, effective_from) VALUES (500, 100, now())`)

	replay := env.reserve(t, limitOrder(userID, openOrder))
	if replay.GetReservedAmount() != before.GetReservedAmount() {
		t.Errorf("replay amount changed from %d to %d", before.GetReservedAmount(), replay.GetReservedAmount())
	}

	after := env.reserve(t, limitOrder(userID, uuid.New()))
	// value 4550 + 5% fee 228 (227.5 rounds up) + buffer 50
	if after.GetReservedAmount() != 4828 {
		t.Errorf("new order reserved %d, want 4828", after.GetReservedAmount())
	}

	var lockedBps, newBps int
	ctx := context.Background()
	if err := env.db.QueryRowContext(ctx, `SELECT buyer_fee_bps FROM holds WHERE id = $1`, before.GetHoldId()).Scan(&lockedBps); err != nil {
		t.Fatalf("read locked rate: %v", err)
	}
	if err := env.db.QueryRowContext(ctx, `SELECT buyer_fee_bps FROM holds WHERE id = $1`, after.GetHoldId()).Scan(&newBps); err != nil {
		t.Fatalf("read new rate: %v", err)
	}
	if lockedBps != 100 || newBps != 500 {
		t.Errorf("rates = %d and %d, want 100 and 500", lockedBps, newBps)
	}
}

func TestReserveIgnoresAFeeConfigNotYetEffective(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 100_000)
	exec(t, env.db, `INSERT INTO fee_configs (buyer_fee_bps, seller_fee_bps, effective_from) VALUES (500, 100, now() + interval '1 day')`)

	response := env.reserve(t, limitOrder(userID, uuid.New()))

	if response.GetReservedAmount() != testOrderAmount {
		t.Errorf("reserved %d, want %d at the current rate", response.GetReservedAmount(), testOrderAmount)
	}
}

func TestReleaseReturnsTheFunds(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))

	released := env.release(t, orderID)

	if released != testOrderAmount {
		t.Errorf("released = %d, want %d", released, testOrderAmount)
	}
	env.assertBalances(t, userID, 10_000, 0)
	env.assertReservedMatchesHolds(t, userID)

	var holdStatus string
	var amountReleased int64
	err := env.db.QueryRowContext(context.Background(),
		`SELECT status, amount_released FROM holds WHERE ref_id = $1`, orderID).Scan(&holdStatus, &amountReleased)
	if err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if holdStatus != "RELEASED" || amountReleased != testOrderAmount {
		t.Errorf("hold = %s with %d released", holdStatus, amountReleased)
	}
}

func TestReleaseReplayReturnsZero(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))
	env.release(t, orderID)

	if released := env.release(t, orderID); released != 0 {
		t.Errorf("second release = %d, want 0", released)
	}
	env.assertBalances(t, userID, 10_000, 0)
}

// Settlement does not exist yet, so a fill is simulated the way it will look:
// consumed on the hold, gone from the wallet's reserved balance.
func (e *reservationEnv) simulateFill(t *testing.T, userID, orderID uuid.UUID, consumed int64) {
	t.Helper()

	exec(t, e.db, `UPDATE holds SET amount_consumed = amount_consumed + $1 WHERE ref_id = $2`, consumed, orderID)
	exec(t, e.db, `UPDATE wallets SET reserved = reserved - $1 WHERE user_id = $2`, consumed, userID)
}

func TestReleaseAfterAPartialFillReturnsOnlyTheRemainder(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))
	env.simulateFill(t, userID, orderID, 1000)

	released := env.release(t, orderID)

	if released != testOrderAmount-1000 {
		t.Errorf("released = %d, want %d", released, testOrderAmount-1000)
	}
	env.assertBalances(t, userID, 10_000-1000, 0)
	env.assertReservedMatchesHolds(t, userID)
}

func TestReleaseAfterAFullFillConsumesTheHold(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))
	env.simulateFill(t, userID, orderID, testOrderAmount)

	if released := env.release(t, orderID); released != 0 {
		t.Errorf("released = %d, want 0", released)
	}

	var holdStatus string
	if err := env.db.QueryRowContext(context.Background(), `SELECT status FROM holds WHERE ref_id = $1`, orderID).Scan(&holdStatus); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if holdStatus != "CONSUMED" {
		t.Errorf("status = %s, want CONSUMED", holdStatus)
	}
	env.assertBalances(t, userID, 10_000-testOrderAmount, 0)
}

func TestReleaseStillWorksOnAFrozenWallet(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))
	exec(t, env.db, `UPDATE wallets SET status = 'FROZEN' WHERE user_id = $1`, userID)

	if released := env.release(t, orderID); released != testOrderAmount {
		t.Errorf("released = %d, want %d", released, testOrderAmount)
	}
	env.assertBalances(t, userID, 10_000, 0)
}

func TestReleaseRejections(t *testing.T) {
	env := newReservationEnv(t)
	ctx := context.Background()

	_, err := env.orders.ReleaseReservation(ctx, &billingv1.ReleaseReservationRequest{
		OrderId: uuid.NewString(),
		Reason:  billingv1.ReleaseReason_RELEASE_REASON_CANCELLED,
	})
	assertRejected(t, err, codes.NotFound, "HOLD_NOT_FOUND")

	_, err = env.orders.ReleaseReservation(ctx, &billingv1.ReleaseReservationRequest{
		OrderId: "nope",
		Reason:  billingv1.ReleaseReason_RELEASE_REASON_CANCELLED,
	})
	assertRejected(t, err, codes.InvalidArgument, "VALIDATION_FAILED")

	_, err = env.orders.ReleaseReservation(ctx, &billingv1.ReleaseReservationRequest{OrderId: uuid.NewString()})
	assertRejected(t, err, codes.InvalidArgument, "VALIDATION_FAILED")
}

func TestReleaseOnlyAppliesToOrderHolds(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	withdrawalID := uuid.New()
	exec(t, env.db,
		`INSERT INTO holds (wallet_id, kind, ref_id, amount_total, status)
		 SELECT id, 'WITHDRAWAL', $1, 100, 'ACTIVE' FROM wallets WHERE user_id = $2`, withdrawalID, userID)

	_, err := env.orders.ReleaseReservation(context.Background(), &billingv1.ReleaseReservationRequest{
		OrderId: withdrawalID.String(),
		Reason:  billingv1.ReleaseReason_RELEASE_REASON_CANCELLED,
	})

	assertRejected(t, err, codes.NotFound, "HOLD_NOT_FOUND")
}

func TestConcurrentReservationsNeverExceedTheBalance(t *testing.T) {
	env := newReservationEnv(t)
	const affordable = 10
	balance := int64(affordable*testOrderAmount + 1)
	userID := env.fund(t, balance)

	const callers = 100
	var succeeded, insufficient atomic.Int64
	var unexpected sync.Map

	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			_, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, uuid.New()))
			switch status.Code(err) {
			case codes.OK:
				succeeded.Add(1)
			case codes.FailedPrecondition:
				insufficient.Add(1)
			default:
				unexpected.Store(err.Error(), true)
			}
		})
	}
	wg.Wait()

	unexpected.Range(func(message, _ any) bool {
		t.Errorf("unexpected error: %v", message)
		return true
	})
	if succeeded.Load() != affordable || insufficient.Load() != callers-affordable {
		t.Errorf("succeeded = %d, insufficient = %d, want %d and %d", succeeded.Load(), insufficient.Load(), affordable, callers-affordable)
	}
	env.assertBalances(t, userID, 1, affordable*testOrderAmount)
	env.assertReservedMatchesHolds(t, userID)
}

func TestConcurrentRetriesOfOneOrderLockFundsOnce(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 100_000)
	orderID := uuid.New()

	const callers = 20
	holdIDs := make([]string, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			response, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, orderID))
			errs[i] = err
			holdIDs[i] = response.GetHoldId()
		})
	}
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if holdIDs[i] != holdIDs[0] {
			t.Errorf("caller %d got hold %q, caller 0 got %q", i, holdIDs[i], holdIDs[0])
		}
	}
	env.assertBalances(t, userID, 100_000-testOrderAmount, testOrderAmount)
}

func TestConcurrentFirstReservesForANewUserAreSerialised(t *testing.T) {
	env := newReservationEnv(t)
	userID := uuid.New()

	const callers = 20
	var wg sync.WaitGroup
	var unexpected sync.Map
	for range callers {
		wg.Go(func() {
			_, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, uuid.New()))
			if status.Code(err) != codes.FailedPrecondition {
				unexpected.Store(status.Code(err).String()+": "+status.Convert(err).Message(), true)
			}
		})
	}
	wg.Wait()

	unexpected.Range(func(message, _ any) bool {
		t.Errorf("unexpected result: %v", message)
		return true
	})
}

func TestConcurrentReleasesGiveFundsBackOnce(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 10_000)
	orderID := uuid.New()
	env.reserve(t, limitOrder(userID, orderID))

	const callers = 20
	var total atomic.Int64
	var failures atomic.Int64
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			response, err := env.orders.ReleaseReservation(context.Background(), &billingv1.ReleaseReservationRequest{
				OrderId: orderID.String(),
				Reason:  billingv1.ReleaseReason_RELEASE_REASON_CANCELLED,
			})
			if err != nil {
				failures.Add(1)
				return
			}
			total.Add(response.GetReleasedAmount())
		})
	}
	wg.Wait()

	if failures.Load() != 0 {
		t.Errorf("%d releases failed", failures.Load())
	}
	if total.Load() != testOrderAmount {
		t.Errorf("total released = %d, want %d", total.Load(), testOrderAmount)
	}
	env.assertBalances(t, userID, 10_000, 0)
}

func TestConcurrentReserveAndReleaseKeepBalancesConsistent(t *testing.T) {
	env := newReservationEnv(t)
	balance := int64(50 * testOrderAmount)
	userID := env.fund(t, balance)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			orderID := uuid.New()
			if _, err := env.orders.ReserveCredits(context.Background(), limitOrder(userID, orderID)); err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			if _, err := env.orders.ReleaseReservation(context.Background(), &billingv1.ReleaseReservationRequest{
				OrderId: orderID.String(),
				Reason:  billingv1.ReleaseReason_RELEASE_REASON_CANCELLED,
			}); err != nil {
				t.Errorf("release: %v", err)
			}
		})
	}
	wg.Wait()

	env.assertBalances(t, userID, balance, 0)
	env.assertReservedMatchesHolds(t, userID)
}
