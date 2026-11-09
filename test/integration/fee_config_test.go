//go:build integration

package integration

import (
	"billing-wallet-service/internal/domain"
	"billing-wallet-service/internal/service"
	"billing-wallet-service/internal/store/postgres"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func asAdmin(actor uuid.UUID) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(),
		"x-gridx-user-id", actor.String(), "x-user-role", "ADMIN")
}

func feeChange(key string, buyerBps, sellerBps int32) *billingv1.SetFeeConfigRequest {
	return &billingv1.SetFeeConfigRequest{
		IdempotencyKey: key,
		BuyerFeeBps:    proto.Int32(buyerBps),
		SellerFeeBps:   proto.Int32(sellerBps),
	}
}

func (e *reservationEnv) setFees(t *testing.T, actor uuid.UUID, request *billingv1.SetFeeConfigRequest) *billingv1.FeeConfig {
	t.Helper()

	response, err := e.admin.SetFeeConfig(asAdmin(actor), request)
	if err != nil {
		t.Fatalf("SetFeeConfig: %v", err)
	}

	return response.GetFeeConfig()
}

func (e *reservationEnv) feeConfigCount(t *testing.T) int {
	t.Helper()

	var count int
	if err := e.db.QueryRowContext(context.Background(), `SELECT count(*) FROM fee_configs`).Scan(&count); err != nil {
		t.Fatalf("count fee configs: %v", err)
	}

	return count
}

func TestFeeConfigsAreAppendOnly(t *testing.T) {
	env := newReservationEnv(t)

	for _, statement := range []string{
		`UPDATE fee_configs SET buyer_fee_bps = 0`,
		`DELETE FROM fee_configs`,
		`TRUNCATE fee_configs`,
	} {
		t.Run(statement, func(t *testing.T) {
			_, err := env.db.ExecContext(context.Background(), statement)

			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != sqlStateRaised {
				t.Errorf("error = %v, want the append-only guard (SQLSTATE %s)", err, sqlStateRaised)
			}
			if count := env.feeConfigCount(t); count != 1 {
				t.Errorf("rows = %d, want the seeded 1", count)
			}
		})
	}
}

func TestSetFeeConfigAddsARowAndNewOrdersUseIt(t *testing.T) {
	env := newReservationEnv(t)
	actor := uuid.New()
	userID := env.fund(t, 100_000)

	config := env.setFees(t, actor, feeChange("change-1", 500, 25))

	if config.GetBuyerFeeBps() != 500 || config.GetSellerFeeBps() != 25 || config.GetCreatedBy() != actor.String() {
		t.Errorf("config = %v", config)
	}
	if count := env.feeConfigCount(t); count != 2 {
		t.Errorf("rows = %d, want 2", count)
	}

	// value 4550 + 5% fee 228 (227.5 rounds up) + buffer 50
	if amount := env.reserve(t, limitOrder(userID, uuid.New())).GetReservedAmount(); amount != 4828 {
		t.Errorf("reserved %d, want 4828", amount)
	}

	var seeded int
	err := env.db.QueryRowContext(context.Background(),
		`SELECT buyer_fee_bps FROM fee_configs WHERE idempotency_key IS NULL`).Scan(&seeded)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	if seeded != 100 {
		t.Errorf("seeded config changed to %d", seeded)
	}
}

func TestZeroRatesAreAccepted(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 100_000)

	env.setFees(t, uuid.New(), feeChange("free", 0, 0))

	// value 4550 + no fee + buffer 50
	if amount := env.reserve(t, limitOrder(userID, uuid.New())).GetReservedAmount(); amount != 4600 {
		t.Errorf("reserved %d, want 4600", amount)
	}
}

func TestAScheduledRateIsNotUsedBeforeItStarts(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 100_000)
	request := feeChange("later", 500, 100)
	request.EffectiveFrom = timestamppb.New(time.Now().Add(time.Hour))

	env.setFees(t, uuid.New(), request)

	if amount := env.reserve(t, limitOrder(userID, uuid.New())).GetReservedAmount(); amount != testOrderAmount {
		t.Errorf("reserved %d, want %d at the current rate", amount, testOrderAmount)
	}
}

func TestOpenOrdersKeepTheirLockedRateAfterAChange(t *testing.T) {
	env := newReservationEnv(t)
	userID := env.fund(t, 100_000)
	open := env.reserve(t, limitOrder(userID, uuid.New()))

	env.setFees(t, uuid.New(), feeChange("change", 500, 100))

	var bps int
	if err := env.db.QueryRowContext(context.Background(),
		`SELECT buyer_fee_bps FROM holds WHERE id = $1`, open.GetHoldId()).Scan(&bps); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if bps != 100 {
		t.Errorf("locked rate = %d, want 100", bps)
	}
}

func TestSetFeeConfigRetryReturnsTheOriginal(t *testing.T) {
	env := newReservationEnv(t)
	actor := uuid.New()
	request := feeChange("retry", 150, 50)
	future := time.Now().Add(time.Hour)
	request.EffectiveFrom = timestamppb.New(future)

	first := env.setFees(t, actor, request)
	second := env.setFees(t, actor, request)

	if first.GetFeeConfigId() != second.GetFeeConfigId() {
		t.Errorf("ids differ: %s, %s", first.GetFeeConfigId(), second.GetFeeConfigId())
	}
	if count := env.feeConfigCount(t); count != 2 {
		t.Errorf("rows = %d, want 2", count)
	}
}

func TestSetFeeConfigConflictsWhenAKeyIsReusedDifferently(t *testing.T) {
	env := newReservationEnv(t)
	actor := uuid.New()
	env.setFees(t, actor, feeChange("taken", 150, 50))

	_, err := env.admin.SetFeeConfig(asAdmin(actor), feeChange("taken", 151, 50))

	assertRejected(t, err, codes.AlreadyExists, "IDEMPOTENCY_CONFLICT")
	if count := env.feeConfigCount(t); count != 2 {
		t.Errorf("rows = %d, want 2", count)
	}
}

func TestConcurrentRetriesCreateOneFeeConfig(t *testing.T) {
	env := newReservationEnv(t)
	actor := uuid.New()

	const callers = 20
	ids := make([]string, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			response, err := env.admin.SetFeeConfig(asAdmin(actor), feeChange("race", 150, 50))
			errs[i] = err
			ids[i] = response.GetFeeConfig().GetFeeConfigId()
		})
	}
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Errorf("caller %d got %q, caller 0 got %q", i, ids[i], ids[0])
		}
	}
	if count := env.feeConfigCount(t); count != 2 {
		t.Errorf("rows = %d, want 2", count)
	}
}

func TestSetFeeConfigRejections(t *testing.T) {
	env := newReservationEnv(t)
	actor := uuid.New()
	past := feeChange("past", 150, 50)
	past.EffectiveFrom = timestamppb.New(time.Now().Add(-time.Hour))

	tests := []struct {
		name       string
		ctx        context.Context
		request    *billingv1.SetFeeConfigRequest
		wantCode   codes.Code
		wantReason string
	}{
		{"not an admin", metadata.AppendToOutgoingContext(context.Background(), "x-gridx-user-id", actor.String(), "x-user-role", "CONSUMER"), feeChange("a", 150, 50), codes.PermissionDenied, "CALLER_NOT_ALLOWED"},
		{"no role", metadata.AppendToOutgoingContext(context.Background(), "x-gridx-user-id", actor.String()), feeChange("b", 150, 50), codes.PermissionDenied, "CALLER_NOT_ALLOWED"},
		{"buyer rate above 2000", asAdmin(actor), feeChange("c", 2001, 50), codes.InvalidArgument, "BAD_AMOUNT"},
		{"seller rate below 0", asAdmin(actor), feeChange("d", 150, -1), codes.InvalidArgument, "BAD_AMOUNT"},
		{"no rates", asAdmin(actor), &billingv1.SetFeeConfigRequest{IdempotencyKey: "e"}, codes.InvalidArgument, "VALIDATION_FAILED"},
		{"no key", asAdmin(actor), feeChange("", 150, 50), codes.InvalidArgument, "VALIDATION_FAILED"},
		{"start time in the past", asAdmin(actor), past, codes.InvalidArgument, "VALIDATION_FAILED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := env.admin.SetFeeConfig(tt.ctx, tt.request)

			assertRejected(t, err, tt.wantCode, tt.wantReason)
			if count := env.feeConfigCount(t); count != 1 {
				t.Errorf("rows = %d, want the seeded 1", count)
			}
		})
	}
}

// The rest of this file runs without the cache. These use the real cache over the
// real store, the same way the service is wired.
func newCachedFees(t *testing.T, env *reservationEnv) (*service.FeeConfigCache, *service.FeeConfigUsecase) {
	t.Helper()

	pool, err := postgres.NewPool(context.Background(), env.serviceURL)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)

	repo := postgres.NewFeeConfigRepo(pool)
	cache := service.NewFeeConfigCache(repo, service.DefaultFeeConfigCacheTTL)

	return cache, service.NewFeeConfigUsecase(repo, cache)
}

func TestARateChangeReachesTheCacheImmediately(t *testing.T) {
	env := newReservationEnv(t)
	cache, usecase := newCachedFees(t, env)
	ctx := context.Background()
	if fee, err := cache.ActiveFeeConfig(ctx, time.Now()); err != nil || fee.BuyerFeeBps != 100 {
		t.Fatalf("fee = %+v, err = %v", fee, err)
	}

	if _, err := usecase.SetFeeConfig(ctx, domain.SetFeeConfigRequest{
		IdempotencyKey: "cached", BuyerFeeBps: 500, SellerFeeBps: 25, ActorID: uuid.New(),
	}); err != nil {
		t.Fatalf("SetFeeConfig: %v", err)
	}

	fee, err := cache.ActiveFeeConfig(ctx, time.Now())
	if err != nil {
		t.Fatalf("ActiveFeeConfig: %v", err)
	}
	if fee.BuyerFeeBps != 500 {
		t.Errorf("rate = %d, want 500", fee.BuyerFeeBps)
	}
}

func TestAScheduledRateStartsOnTimeThroughTheCache(t *testing.T) {
	env := newReservationEnv(t)
	cache, usecase := newCachedFees(t, env)
	ctx := context.Background()
	start := time.Now().Add(2 * time.Second).Truncate(time.Microsecond)

	if _, err := usecase.SetFeeConfig(ctx, domain.SetFeeConfigRequest{
		IdempotencyKey: "scheduled", BuyerFeeBps: 500, SellerFeeBps: 25, EffectiveFrom: &start, ActorID: uuid.New(),
	}); err != nil {
		t.Fatalf("SetFeeConfig: %v", err)
	}

	if fee, err := cache.ActiveFeeConfig(ctx, time.Now()); err != nil || fee.BuyerFeeBps != 100 {
		t.Fatalf("before the start: fee = %+v, err = %v", fee, err)
	}

	time.Sleep(time.Until(start) + 50*time.Millisecond)

	fee, err := cache.ActiveFeeConfig(ctx, time.Now())
	if err != nil {
		t.Fatalf("ActiveFeeConfig: %v", err)
	}
	if fee.BuyerFeeBps != 500 {
		t.Errorf("after the start: rate = %d, want 500", fee.BuyerFeeBps)
	}
}
