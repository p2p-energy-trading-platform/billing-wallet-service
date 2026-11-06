package grpc

import (
	"billing-wallet-service/internal/domain"
	"context"
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const errorDomain = "billing.gridx"

// domainErrors maps domain errors to the gRPC codes and typed reasons in the API
// contract. Reasons are what callers should branch on.
var domainErrors = []struct {
	target error
	code   codes.Code
	reason string
}{
	// ErrBadAmount also matches ErrValidation, so it has to be checked first.
	{domain.ErrBadAmount, codes.InvalidArgument, "BAD_AMOUNT"},
	{domain.ErrValidation, codes.InvalidArgument, "VALIDATION_FAILED"},
	{domain.ErrMarketOrderNeedsCollar, codes.InvalidArgument, "MARKET_ORDER_NEEDS_COLLAR"},
	{domain.ErrInsufficientFunds, codes.FailedPrecondition, "INSUFFICIENT_FUNDS"},
	{domain.ErrWalletFrozen, codes.FailedPrecondition, "WALLET_FROZEN"},
	{domain.ErrHoldInsufficient, codes.FailedPrecondition, "HOLD_INSUFFICIENT_REMAINING"},
	{domain.ErrPriceExceedsReservation, codes.FailedPrecondition, "PRICE_EXCEEDS_RESERVATION"},
	{domain.ErrNotFound, codes.NotFound, "NOT_FOUND"},
	{domain.ErrHoldNotFound, codes.NotFound, "HOLD_NOT_FOUND"},
	{domain.ErrIdempotencyConflict, codes.AlreadyExists, "IDEMPOTENCY_CONFLICT"},
	{domain.ErrAlreadySettled, codes.AlreadyExists, "ALREADY_SETTLED"},
	{domain.ErrWebhookSignatureInvalid, codes.InvalidArgument, "WEBHOOK_SIGNATURE_INVALID"},
	{domain.ErrRetryableContention, codes.Aborted, "RETRYABLE_CONTENTION"},
}

func ToGRPCError(err error) error {
	if err == nil {
		return nil
	}

	if _, ok := status.FromError(err); ok {
		return err
	}

	// Context errors
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "request canceled")
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	}

	for _, mapping := range domainErrors {
		if errors.Is(err, mapping.target) {
			return statusWithReason(mapping.code, mapping.reason, err.Error())
		}
	}

	// Infrastructure failures use fixed messages so their details never reach callers.
	if errors.Is(err, domain.ErrUnavailable) {
		return status.Error(codes.Unavailable, "service temporarily unavailable")
	}

	return status.Error(codes.Internal, "Internal Server Error")
}

func statusWithReason(code codes.Code, reason, message string) error {
	st := status.New(code, message)

	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: errorDomain})
	if err != nil {
		return st.Err()
	}

	return withInfo.Err()
}
