package grpc

import (
	"billing-service/internal/domain"
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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

	// Validation and arguments
	if errors.Is(err, domain.ErrValidation) {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	if errors.Is(err, domain.ErrMarketOrderNeedsCollar) {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	// Precondition failures
	if errors.Is(err, domain.ErrInsufficientFunds) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}

	if errors.Is(err, domain.ErrWalletFrozen) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}

	if errors.Is(err, domain.ErrHoldInsufficient) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}

	if errors.Is(err, domain.ErrPriceExceedsReservation) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}

	// Not found errors
	if errors.Is(err, domain.ErrNotFound) {
		return status.Error(codes.NotFound, err.Error())
	}

	if errors.Is(err, domain.ErrHoldNotFound) {
		return status.Error(codes.NotFound, err.Error())
	}

	// Conflicts errors
	if errors.Is(err, domain.ErrIdempotencyConflict) {
		return status.Error(codes.AlreadyExists, err.Error())
	}

	if errors.Is(err, domain.ErrAlreadySettled) {
		return status.Error(codes.AlreadyExists, err.Error())
	}

	// Security and webhooks
	if errors.Is(err, domain.ErrWebhookSignatureInvalid) {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	return status.Error(codes.Internal, "Internal Server Error")
}
