//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"billing-wallet-service/internal/store/postgres"
	"billing-wallet-service/internal/worker"

	"github.com/google/uuid"
)

// newSweeper returns a reconciler with its own connection pool, the way another
// replica of the service would have.
func newSweeper(t *testing.T, env *reservationEnv, batchSize int) *worker.Reconciler {
	t.Helper()

	pool, err := postgres.NewPool(context.Background(), env.serviceURL)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	return worker.NewReconciler(postgres.NewReservationRepo(pool), time.Minute, batchSize, logger)
}

// reserveExpired reserves an order and moves its expiry into the past. The API
// refuses an expiry already in the past, so this writes the row directly.
func (e *reservationEnv) reserveExpired(t *testing.T, userID uuid.UUID) uuid.UUID {
	t.Helper()

	orderID := uuid.New()
	e.reserve(t, limitOrder(userID, orderID))
	exec(t, e.db, `UPDATE holds SET expires_at = now() - interval '1 hour' WHERE ref_id = $1`, orderID)

	return orderID
}

func (e *reservationEnv) holdState(t *testing.T, orderID uuid.UUID) (holdStatus string, consumed, released int64) {
	t.Helper()

	err := e.db.QueryRowContext(context.Background(),
		`SELECT status, amount_consumed, amount_released FROM holds WHERE ref_id = $1`, orderID).
		Scan(&holdStatus, &consumed, &released)
	if err != nil {
		t.Fatalf("read hold: %v", err)
	}

	return holdStatus, consumed, released
}
