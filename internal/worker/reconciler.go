package worker

import (
	"context"
	"log/slog"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
)

// DefaultHoldBatchSize is how many expired holds one sweep handles.
const DefaultHoldBatchSize = 100

type HoldStore interface {
	ExpiredOrderHolds(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error)
	ExpireHold(ctx context.Context, orderID uuid.UUID, now time.Time) (domain.HoldExpiry, error)
}

// SweepStats summarises one sweep.
type SweepStats struct {
	Expired  int
	Released int64
	Skipped  int // locked by another transaction or already handled; retried next sweep if still expired
	Failed   int
}

// Reconciler is the safety net for holds whose order OMS has abandoned. OMS decides
// which holds can expire by setting expires_at, and a hold with none is never swept.
type Reconciler struct {
	store     HoldStore
	interval  time.Duration
	batchSize int
	logger    *slog.Logger
}

func NewReconciler(store HoldStore, interval time.Duration, batchSize int, logger *slog.Logger) *Reconciler {
	return &Reconciler{store: store, interval: interval, batchSize: batchSize, logger: logger}
}

// Run sweeps immediately and then on every interval until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		r.SweepExpiredHolds(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// SweepExpiredHolds handles one batch, each hold in its own transaction, so one
// failure cannot block the rest. Replicas can sweep at the same time.
func (r *Reconciler) SweepExpiredHolds(ctx context.Context) SweepStats {
	var stats SweepStats
	now := time.Now()

	orderIDs, err := r.store.ExpiredOrderHolds(ctx, now, r.batchSize)
	if err != nil {
		if ctx.Err() == nil {
			r.logger.Error("failed to list expired holds", "error", err)
			stats.Failed++
		}
		return stats
	}

	for _, orderID := range orderIDs {
		if ctx.Err() != nil {
			break
		}

		expiry, err := r.store.ExpireHold(ctx, orderID, now)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				r.logger.Error("failed to expire hold", "order_id", orderID, "error", err)
				stats.Failed++
			}
		case expiry.Expired:
			stats.Expired++
			stats.Released += expiry.Released
			r.logger.Info("expired hold", "order_id", orderID, "released", expiry.Released)
		default:
			stats.Skipped++
		}
	}

	return stats
}
