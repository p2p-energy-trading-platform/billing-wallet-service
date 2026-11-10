package postgres

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	contentionAttempts = 3
	contentionBackoff  = 20 * time.Millisecond
)

// retryOnContention runs fn again after a serialization failure or deadlock, up to
// attempts times with jittered backoff, then returns ErrRetryableContention.
func retryOnContention(ctx context.Context, attempts int, backoff time.Duration, fn func() error) error {
	var err error

	for attempt := 1; attempt <= attempts; attempt++ {
		err = fn()
		if !errors.Is(err, domain.ErrRetryableContention) || attempt == attempts {
			return err
		}

		wait := time.Duration(attempt)*backoff + time.Duration(rand.Int64N(int64(backoff)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}

	return err
}

// inTx runs fn in a transaction that commits if fn succeeds, retrying the whole
// transaction on contention. fn must not have effects outside the transaction.
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return retryOnContention(ctx, contentionAttempts, contentionBackoff, func() error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return mapError(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		if err := fn(tx); err != nil {
			return err
		}

		return mapError(tx.Commit(ctx))
	})
}
