package domain

import (
	"errors"
	"fmt"
)

// ErrValidation is the sentinel every ValidationError wraps. Callers
// can use errors.Is(err, domain.ErrValidation) to classify a payload/argument
// failure as permanent.
var ErrValidation = errors.New("domain: validation failed")

// ErrBadAmount narrows ErrValidation to a non-positive or overflowing quantity,
// price or amount, so those failures can report the BAD_AMOUNT reason.
var ErrBadAmount = errors.New("domain: bad amount")

type ValidationError struct {
	Field     string
	Reason    string
	badAmount bool
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation: field %q: %s", e.Field, e.Reason)
}

func (e *ValidationError) Unwrap() []error {
	if e.badAmount {
		return []error{ErrValidation, ErrBadAmount}
	}

	return []error{ErrValidation}
}

func NewValidationError(field, reason string) error {
	return &ValidationError{Field: field, Reason: reason}
}

func NewBadAmountError(field, reason string) error {
	return &ValidationError{Field: field, Reason: reason, badAmount: true}
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
	ErrUnbalancedJournal       = errors.New("domain: journal lines do not sum to zero")
	ErrCallerNotAllowed        = errors.New("domain: caller is not allowed to perform this action")
)

// ErrUnavailable marks an infrastructure failure that is worth retrying, such as
// the database being down. Callers retry with backoff.
var ErrUnavailable = errors.New("domain: dependency unavailable")

// ErrRetryableContention marks a serialization failure or deadlock. Nothing was
// committed, so the caller can safely retry.
var ErrRetryableContention = errors.New("domain: transaction contention, retry")
