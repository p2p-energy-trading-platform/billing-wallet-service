package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	holdColumns = `id, wallet_id, kind, ref_id, order_type, qty_wh, max_price_per_kwh, buyer_fee_bps,
		buyer_fee_config_id, amount_total, amount_consumed, amount_released, status, expires_at, created_at, updated_at`

	uniqueViolation = "23505"
)

type ReservationRepo struct {
	pool *pgxpool.Pool
}

func NewReservationRepo(pool *pgxpool.Pool) *ReservationRepo {
	return &ReservationRepo{pool: pool}
}

// ReserveHold moves amount from available to reserved and records the hold in one
// transaction. The replay check runs while holding the wallet lock, so concurrent
// retries of one order wait for each other and cannot lock the funds twice.
func (r *ReservationRepo) ReserveHold(ctx context.Context, request domain.ReserveRequest, fee domain.FeeConfig, amount int64) (domain.Reservation, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("begin reserve: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ensured, err := ensureWallet(ctx, tx, request.UserID)
	if err != nil {
		return domain.Reservation{}, err
	}

	wallet, err := lockWallet(ctx, tx, ensured.ID)
	if err != nil {
		return domain.Reservation{}, err
	}

	existing, err := findOrderHold(ctx, tx, request.OrderID, "")
	switch {
	case err == nil:
		if !existing.MatchesReservation(wallet.ID, request) {
			return domain.Reservation{}, fmt.Errorf("order %s: %w", request.OrderID, domain.ErrIdempotencyConflict)
		}
		return domain.Reservation{Hold: existing, AvailableAfter: wallet.Available}, nil
	case !errors.Is(err, domain.ErrHoldNotFound):
		return domain.Reservation{}, err
	}

	if err := wallet.EnsureActive(); err != nil {
		return domain.Reservation{}, err
	}
	if wallet.Available < amount {
		return domain.Reservation{}, domain.ErrInsufficientFunds
	}

	wallet, err = scanWallet(tx.QueryRow(ctx,
		`UPDATE wallets
		 SET available = available - $1, reserved = reserved + $1, version = version + 1
		 WHERE id = $2
		 RETURNING `+walletColumns, amount, wallet.ID))
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("lock funds: %w", mapError(err))
	}

	hold, err := scanHold(tx.QueryRow(ctx,
		`INSERT INTO holds (wallet_id, kind, ref_id, order_type, qty_wh, max_price_per_kwh,
		                    buyer_fee_bps, buyer_fee_config_id, amount_total, status, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 RETURNING `+holdColumns,
		wallet.ID, domain.HoldKindOrder, request.OrderID, request.OrderType, request.QtyWh, request.MaxPricePerKwh,
		fee.BuyerFeeBps, fee.ID, amount, domain.HoldActive, request.ExpiresAt))
	if err != nil {
		// The wallet lock only serialises one user, so this is the same order being
		// reserved for a different user at the same moment.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return domain.Reservation{}, fmt.Errorf("order %s: %w", request.OrderID, domain.ErrIdempotencyConflict)
		}
		return domain.Reservation{}, fmt.Errorf("insert hold: %w", mapError(err))
	}

	if err := insertBalanceChanged(ctx, tx, wallet, balanceChange{OrderID: hold.RefID, HoldID: hold.ID}); err != nil {
		return domain.Reservation{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Reservation{}, fmt.Errorf("commit reserve: %w", mapError(err))
	}

	return domain.Reservation{Hold: hold, AvailableAfter: wallet.Available}, nil
}

// ReleaseHold returns what is left of an order's hold to the wallet. It takes the
// wallet lock before the hold lock, the same order as every other balance change,
// so it cannot deadlock with them. A hold that is already terminal releases 0.
func (r *ReservationRepo) ReleaseHold(ctx context.Context, orderID uuid.UUID, reason domain.ReleaseReason) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin release: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	unlocked, err := findOrderHold(ctx, tx, orderID, "")
	if err != nil {
		return 0, err
	}

	wallet, err := lockWallet(ctx, tx, unlocked.WalletID)
	if err != nil {
		return 0, err
	}

	hold, err := findOrderHold(ctx, tx, orderID, "FOR UPDATE")
	if err != nil {
		return 0, err
	}

	if hold.Status != domain.HoldActive {
		return 0, nil
	}

	leftover, nextStatus := hold.ReleaseOutcome()

	if err := returnHoldFunds(ctx, tx, wallet, hold, leftover, nextStatus, reason); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit release: %w", mapError(err))
	}

	return leftover, nil
}

// ExpiredOrderHolds lists order holds past their expiry, oldest first. It takes no
// locks: ExpireHold locks and rechecks each one.
func (r *ReservationRepo) ExpiredOrderHolds(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT ref_id FROM holds
		 WHERE status = $1 AND kind = $2 AND expires_at < $3
		 ORDER BY expires_at
		 LIMIT $4`, domain.HoldActive, domain.HoldKindOrder, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired holds: %w", mapError(err))
	}

	orderIDs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("read expired holds: %w", mapError(err))
	}

	return orderIDs, nil
}

// ExpireHold returns what is left of an expired hold to the wallet. It locks the
// wallet before the hold, the same order as every other balance change, so it
// cannot deadlock with them. SKIP LOCKED means a busy wallet or another replica
// makes it report Expired false rather than wait, and a later sweep retries.
func (r *ReservationRepo) ExpireHold(ctx context.Context, orderID uuid.UUID, now time.Time) (domain.HoldExpiry, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.HoldExpiry{}, fmt.Errorf("begin expire: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	unlocked, err := findOrderHold(ctx, tx, orderID, "")
	if err != nil {
		return domain.HoldExpiry{}, err
	}

	wallet, err := scanWallet(tx.QueryRow(ctx,
		`SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE SKIP LOCKED`, unlocked.WalletID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.HoldExpiry{}, nil
	}
	if err != nil {
		return domain.HoldExpiry{}, fmt.Errorf("lock wallet: %w", mapError(err))
	}

	hold, err := findOrderHold(ctx, tx, orderID, "FOR UPDATE SKIP LOCKED")
	if errors.Is(err, domain.ErrHoldNotFound) {
		return domain.HoldExpiry{}, nil
	}
	if err != nil {
		return domain.HoldExpiry{}, err
	}

	// It may have been released or filled since it was listed.
	if !hold.IsExpired(now) {
		return domain.HoldExpiry{}, nil
	}

	leftover, nextStatus := hold.ExpireOutcome()

	if err := returnHoldFunds(ctx, tx, wallet, hold, leftover, nextStatus, domain.ReleaseExpired); err != nil {
		return domain.HoldExpiry{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.HoldExpiry{}, fmt.Errorf("commit expire: %w", mapError(err))
	}

	return domain.HoldExpiry{Expired: true, Released: leftover}, nil
}

// returnHoldFunds closes a hold and gives leftover back to the wallet. The caller
// must hold both row locks. An event is only recorded if money moved.
func returnHoldFunds(ctx context.Context, tx pgx.Tx, wallet domain.Wallet, hold domain.Hold, leftover int64, nextStatus domain.HoldStatus, reason domain.ReleaseReason) error {
	hold, err := scanHold(tx.QueryRow(ctx,
		`UPDATE holds
		 SET amount_released = amount_released + $1, status = $2
		 WHERE id = $3
		 RETURNING `+holdColumns, leftover, nextStatus, hold.ID))
	if err != nil {
		return fmt.Errorf("update hold: %w", mapError(err))
	}

	if leftover == 0 {
		return nil
	}

	wallet, err = scanWallet(tx.QueryRow(ctx,
		`UPDATE wallets
		 SET available = available + $1, reserved = reserved - $1, version = version + 1
		 WHERE id = $2
		 RETURNING `+walletColumns, leftover, wallet.ID))
	if err != nil {
		return fmt.Errorf("return funds: %w", mapError(err))
	}

	return insertBalanceChanged(ctx, tx, wallet, balanceChange{OrderID: hold.RefID, HoldID: hold.ID, Reason: string(reason)})
}

func lockWallet(ctx context.Context, tx pgx.Tx, walletID uuid.UUID) (domain.Wallet, error) {
	wallet, err := scanWallet(tx.QueryRow(ctx,
		`SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, walletID))
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("lock wallet: %w", mapError(err))
	}

	return wallet, nil
}

// findOrderHold reads an order's hold, optionally with a locking clause.
func findOrderHold(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, lock string) (domain.Hold, error) {
	hold, err := scanHold(tx.QueryRow(ctx,
		`SELECT `+holdColumns+` FROM holds WHERE kind = $1 AND ref_id = $2 `+lock,
		domain.HoldKindOrder, orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Hold{}, domain.ErrHoldNotFound
	}
	if err != nil {
		return domain.Hold{}, fmt.Errorf("find hold: %w", mapError(err))
	}

	return hold, nil
}

func scanHold(row pgx.Row) (domain.Hold, error) {
	var hold domain.Hold
	err := row.Scan(
		&hold.ID,
		&hold.WalletID,
		&hold.Kind,
		&hold.RefID,
		&hold.OrderType,
		&hold.QtyWh,
		&hold.MaxPricePerKwh,
		&hold.BuyerFeeBps,
		&hold.BuyerFeeConfigID,
		&hold.AmountTotal,
		&hold.AmountConsumed,
		&hold.AmountReleased,
		&hold.Status,
		&hold.ExpiresAt,
		&hold.CreatedAt,
		&hold.UpdatedAt,
	)

	return hold, err
}
