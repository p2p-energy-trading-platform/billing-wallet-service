package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const feeConfigColumns = `id, buyer_fee_bps, seller_fee_bps, effective_from, created_by, created_at`

type FeeConfigRepo struct {
	pool *pgxpool.Pool
}

func NewFeeConfigRepo(pool *pgxpool.Pool) *FeeConfigRepo {
	return &FeeConfigRepo{pool: pool}
}

// ActiveFeeConfig returns the latest config in effect at the given time. It fails
// rather than guessing a rate when there is none.
func (r *FeeConfigRepo) ActiveFeeConfig(ctx context.Context, at time.Time) (domain.FeeConfig, error) {
	fee, err := scanFeeConfig(r.pool.QueryRow(ctx,
		`SELECT `+feeConfigColumns+`
		 FROM fee_configs
		 WHERE effective_from <= $1
		 ORDER BY effective_from DESC, created_at DESC
		 LIMIT 1`, at))
	if err != nil {
		return domain.FeeConfig{}, fmt.Errorf("find active fee config: %w", mapError(err))
	}

	return fee, nil
}

// NextFeeConfigStart returns the earliest start after the given time, or nil if no
// later config is scheduled.
func (r *FeeConfigRepo) NextFeeConfigStart(ctx context.Context, after time.Time) (*time.Time, error) {
	var next *time.Time
	err := r.pool.QueryRow(ctx,
		`SELECT min(effective_from) FROM fee_configs WHERE effective_from > $1`, after).Scan(&next)
	if err != nil {
		return nil, fmt.Errorf("find next fee config start: %w", mapError(err))
	}

	return next, nil
}

func (r *FeeConfigRepo) FindFeeConfigByKey(ctx context.Context, key string) (domain.FeeConfig, error) {
	fee, err := scanFeeConfig(r.pool.QueryRow(ctx,
		`SELECT `+feeConfigColumns+` FROM fee_configs WHERE idempotency_key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.FeeConfig{}, fmt.Errorf("fee config key %q: %w", key, domain.ErrNotFound)
	}
	if err != nil {
		return domain.FeeConfig{}, fmt.Errorf("find fee config: %w", mapError(err))
	}

	return fee, nil
}

// InsertFeeConfig adds the row. When a concurrent request wins the key, this waits
// for it to commit and returns that row instead.
func (r *FeeConfigRepo) InsertFeeConfig(ctx context.Context, request domain.SetFeeConfigRequest) (domain.FeeConfig, error) {
	fee, err := scanFeeConfig(r.pool.QueryRow(ctx,
		`INSERT INTO fee_configs (buyer_fee_bps, seller_fee_bps, effective_from, created_by, idempotency_key)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (idempotency_key) DO NOTHING
		 RETURNING `+feeConfigColumns,
		request.BuyerFeeBps, request.SellerFeeBps, request.EffectiveFrom, request.ActorID, request.IdempotencyKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return r.FindFeeConfigByKey(ctx, request.IdempotencyKey)
	}
	if err != nil {
		return domain.FeeConfig{}, fmt.Errorf("insert fee config: %w", mapError(err))
	}

	return fee, nil
}

func scanFeeConfig(row pgx.Row) (domain.FeeConfig, error) {
	var fee domain.FeeConfig
	err := row.Scan(&fee.ID, &fee.BuyerFeeBps, &fee.SellerFeeBps, &fee.EffectiveFrom, &fee.CreatedBy, &fee.CreatedAt)

	return fee, err
}
