package worker

import (
	"context"
	"log/slog"
	"time"

	"billing-wallet-service/internal/domain"
	"billing-wallet-service/internal/provider/stripe"
	"billing-wallet-service/internal/service"
)

const (
	DefaultDepositBatchSize  = 50
	DefaultDepositInterval   = time.Minute
	DefaultDepositStaleAfter = 15 * time.Minute
)

type DepositReconciler struct {
	store      service.DepositStore
	provider   stripe.PaymentProvider
	interval   time.Duration
	staleAfter time.Duration
	batchSize  int
	logger     *slog.Logger
}

type DepositSweepStats struct {
	Checked int
	Paid    int
	Expired int
	Skipped int
	Failed  int
}

func NewDepositReconciler(
	store service.DepositStore,
	provider stripe.PaymentProvider,
	interval time.Duration,
	staleAfter time.Duration,
	batchSize int,
	logger *slog.Logger,
) *DepositReconciler {
	if interval <= 0 {
		interval = DefaultDepositInterval
	}

	if staleAfter <= 0 {
		staleAfter = DefaultDepositStaleAfter
	}

	if batchSize <= 0 {
		batchSize = DefaultDepositBatchSize
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &DepositReconciler{
		store:      store,
		provider:   provider,
		interval:   interval,
		staleAfter: staleAfter,
		batchSize:  batchSize,
		logger:     logger,
	}
}

func (r *DepositReconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		r.Sweep(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *DepositReconciler) Sweep(ctx context.Context) DepositSweepStats {
	var stats DepositSweepStats

	cutoff := time.Now().UTC().Add(-r.staleAfter)

	stale, err := r.store.ListStalePendingDeposits(
		ctx,
		cutoff,
		r.batchSize,
	)

	if err != nil {
		if ctx.Err() == nil {
			r.logger.Error(
				"failed to list stale pending deposits",
				slog.Any("error", err),
			)
			stats.Failed++
		}
		return stats
	}

	for _, dep := range stale {
		if ctx.Err() != nil {
			break
		}

		stats.Checked++

		if dep.ProviderRef == nil || *dep.ProviderRef == "" {
			expired := domain.DepositExpired

			_, err := r.store.ConfirmDepositWebhook(
				ctx,
				r.provider.Name(),
				"reconcile-orphan:"+dep.ID.String(),
				"reconcile.orphan_expired",
				"",
				"",
				"",
				0,
				false,
				&expired,
			)

			if err != nil {
				stats.Failed++
				r.logger.Warn(
					"failed to expire orphan deposit",
					slog.String("deposit_id", dep.ID.String()),
					slog.Any("error", err),
				)
			} else {
				stats.Expired++
			}

			continue
		}

		status, err := r.provider.GetCheckoutStatus(ctx, *dep.ProviderRef)
		if err != nil {
			stats.Failed++
			r.logger.Warn(
				"failed to retrieve checkout status",
				slog.String("deposit_id", dep.ID.String()),
				slog.Any("error", err),
			)
			continue
		}

		switch {
		case status.PaymentStatus == "paid":
			_, err = r.store.ConfirmDepositWebhook(
				ctx,
				r.provider.Name(),
				"reconcile-paid:"+*dep.ProviderRef,
				"reconcile.checkout.completed",
				*dep.ProviderRef,
				status.PaymentIntent,
				status.Currency,
				status.AmountTotal,
				true,
				nil,
			)

			if err != nil {
				stats.Failed++
				r.logger.Warn(
					"failed to reconcile paid deposit",
					slog.String("deposit_id", dep.ID.String()),
					slog.Any("error", err),
				)
			} else {
				stats.Paid++
			}

		case status.Status == "expired":
			expired := domain.DepositExpired

			_, err = r.store.ConfirmDepositWebhook(
				ctx,
				r.provider.Name(),
				"reconcile-expired:"+*dep.ProviderRef,
				"reconcile.checkout.expired",
				*dep.ProviderRef,
				"",
				"",
				0,
				false,
				&expired,
			)

			if err != nil {
				stats.Failed++
				r.logger.Warn(
					"failed to reconcile expired deposit",
					slog.String("deposit_id", dep.ID.String()),
					slog.Any("error", err),
				)
			} else {
				stats.Expired++
			}

		default:
			// Checkout is still open or awaiting asynchronous payment.
			stats.Skipped++
		}
	}

	r.logger.Info(
		"deposit reconciliation completed",
		slog.Int("checked", stats.Checked),
		slog.Int("paid", stats.Paid),
		slog.Int("expired", stats.Expired),
		slog.Int("skipped", stats.Skipped),
		slog.Int("failed", stats.Failed),
	)

	return stats
}
