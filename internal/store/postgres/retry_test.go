package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/jackc/pgx/v5/pgconn"
)

func contention() error { return mapError(&pgconn.PgError{Code: "40P01"}) }

func TestRetryOnContentionSucceedsAfterContention(t *testing.T) {
	attempts := 0

	err := retryOnContention(context.Background(), 3, time.Millisecond, func() error {
		attempts++
		if attempts < 3 {
			return contention()
		}
		return nil
	})

	if err != nil || attempts != 3 {
		t.Errorf("err = %v after %d attempts, want success on the 3rd", err, attempts)
	}
}

func TestRetryOnContentionGivesUpAfterTheLimit(t *testing.T) {
	attempts := 0

	err := retryOnContention(context.Background(), 3, time.Millisecond, func() error {
		attempts++
		return contention()
	})

	if !errors.Is(err, domain.ErrRetryableContention) {
		t.Errorf("err = %v, want ErrRetryableContention", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestRetryOnContentionDoesNotRetryOtherErrors(t *testing.T) {
	attempts := 0

	err := retryOnContention(context.Background(), 3, time.Millisecond, func() error {
		attempts++
		return domain.ErrInsufficientFunds
	})

	if !errors.Is(err, domain.ErrInsufficientFunds) || attempts != 1 {
		t.Errorf("err = %v after %d attempts, want one attempt", err, attempts)
	}
}

func TestRetryOnContentionStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0

	err := retryOnContention(ctx, 3, time.Hour, func() error {
		attempts++
		cancel()
		return contention()
	})

	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Errorf("err = %v after %d attempts, want to stop on cancel", err, attempts)
	}
}
