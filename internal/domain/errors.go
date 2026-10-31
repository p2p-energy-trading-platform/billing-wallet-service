package domain

import (
	"errors"
	"fmt"
)

// ErrValidation is the sentinel every ValidationError wraps. Callers
// can use errors.Is(err, domain.ErrValidation) to classify a payload/argument
// failure as permanent.
var ErrValidation = errors.New("domain: validation failed")

type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation: field %q: %s", e.Field, e.Reason)
}

func (e *ValidationError) Unwrap() error {
	return ErrValidation
}

func NewValidationError(field, reason string) error {
	return &ValidationError{Field: field, Reason: reason}
}

// Sentinel errors mapped to financial domain rules and gRPC status codes
var (
	ErrNotFound                = errors.New("domain: resource not found")
	ErrInsufficientFunds       = errors.New("domain: insufficient available funds")
	ErrWalletFrozen            = errors.New("domain: wallet is frozen")
	ErrHoldNotFound            = errors.New("domain: hold not found or already terminal")
	ErrHoldInsufficient        = errors.New("domain: hold remaining amount insufficient for settlement")
	ErrIdempotencyConflict     = errors.New("domain: idempotency key conflict with different payload")
	ErrMarketOrderNeedsCollar  = errors.New("domain: market buy order requires a price collar")
	ErrPriceExceedsReservation = errors.New("domain: fill price exceeds reservation price collar")
	ErrWebhookSignatureInvalid = errors.New("domain: stripe webhook signature verification failed")
	ErrAlreadySettled          = errors.New("domain: trade already settled")
)
