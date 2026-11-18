package grpc

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"billing-wallet-service/internal/domain"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func errorReason(t *testing.T, err error) string {
	t.Helper()

	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}

	return ""
}

func TestToGRPCErrorMapsDomainErrors(t *testing.T) {
	tests := []struct {
		err        error
		wantCode   codes.Code
		wantReason string
	}{
		{err: domain.ErrInsufficientFunds, wantCode: codes.FailedPrecondition, wantReason: "INSUFFICIENT_FUNDS"},
		{err: domain.ErrWalletFrozen, wantCode: codes.FailedPrecondition, wantReason: "WALLET_FROZEN"},
		{err: domain.ErrHoldNotFound, wantCode: codes.NotFound, wantReason: "HOLD_NOT_FOUND"},
		{err: domain.ErrHoldInsufficient, wantCode: codes.FailedPrecondition, wantReason: "HOLD_INSUFFICIENT_REMAINING"},
		{err: domain.ErrIdempotencyConflict, wantCode: codes.AlreadyExists, wantReason: "IDEMPOTENCY_CONFLICT"},
		{err: domain.ErrMarketOrderNeedsCollar, wantCode: codes.InvalidArgument, wantReason: "MARKET_ORDER_NEEDS_COLLAR"},
		{err: domain.ErrPriceExceedsReservation, wantCode: codes.FailedPrecondition, wantReason: "PRICE_EXCEEDS_RESERVATION"},
		{err: domain.ErrWebhookSignatureInvalid, wantCode: codes.InvalidArgument, wantReason: "WEBHOOK_SIGNATURE_INVALID"},
		{err: domain.ErrNotFound, wantCode: codes.NotFound, wantReason: "NOT_FOUND"},
		{err: domain.ErrAlreadySettled, wantCode: codes.AlreadyExists, wantReason: "ALREADY_SETTLED"},
		{err: domain.NewValidationError("order_id", "must be a UUID"), wantCode: codes.InvalidArgument, wantReason: "VALIDATION_FAILED"},
		{err: domain.NewBadAmountError("qty_wh", "must be positive"), wantCode: codes.InvalidArgument, wantReason: "BAD_AMOUNT"},
		{err: domain.ErrRetryableContention, wantCode: codes.Aborted, wantReason: "RETRYABLE_CONTENTION"},
		{err: domain.ErrCallerNotAllowed, wantCode: codes.PermissionDenied, wantReason: "CALLER_NOT_ALLOWED"},
	}

	for _, tt := range tests {
		t.Run(tt.wantReason, func(t *testing.T) {
			got := ToGRPCError(fmt.Errorf("while handling: %w", tt.err))

			if code := status.Code(got); code != tt.wantCode {
				t.Errorf("code = %v, want %v", code, tt.wantCode)
			}
			if reason := errorReason(t, got); reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestToGRPCErrorHidesInfrastructureDetails(t *testing.T) {
	const secret = "postgres://user:hunter2@db:5432"

	tests := []struct {
		name     string
		err      error
		wantCode codes.Code
	}{
		{name: "unavailable", err: fmt.Errorf("%w: dial %s", domain.ErrUnavailable, secret), wantCode: codes.Unavailable},
		{name: "unknown", err: errors.New("dial " + secret), wantCode: codes.Internal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToGRPCError(tt.err)

			if code := status.Code(got); code != tt.wantCode {
				t.Errorf("code = %v, want %v", code, tt.wantCode)
			}
			if strings.Contains(got.Error(), secret) {
				t.Errorf("error %q leaks infrastructure details", got)
			}
		})
	}
}

// The caller should retry, but the SQL behind the contention is not theirs to see.
func TestToGRPCErrorHidesContentionDetails(t *testing.T) {
	err := fmt.Errorf("%w: ERROR: deadlock detected (SQLSTATE 40P01) on relation wallets", domain.ErrRetryableContention)

	got := ToGRPCError(err)

	if status.Code(got) != codes.Aborted || errorReason(t, got) != "RETRYABLE_CONTENTION" {
		t.Errorf("got %v, want Aborted with RETRYABLE_CONTENTION", got)
	}
	if strings.Contains(got.Error(), "SQLSTATE") || strings.Contains(got.Error(), "wallets") {
		t.Errorf("error %q leaks database details", got)
	}
}

func TestToGRPCErrorKeepsExistingStatus(t *testing.T) {
	original := status.Error(codes.Unauthenticated, "no user")

	if got := ToGRPCError(original); status.Code(got) != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", status.Code(got))
	}
}

func TestToGRPCErrorNil(t *testing.T) {
	if got := ToGRPCError(nil); got != nil {
		t.Errorf("ToGRPCError(nil) = %v, want nil", got)
	}
}
