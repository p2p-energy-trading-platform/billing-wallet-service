package postgres

import (
	"billing-wallet-service/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const withdrawalColumns = `
	id, wallet_id, hold_id, amount, destination, status,
	reviewed_by, journal_id, created_at, updated_at`

type WithdrawalRepo struct {
	pool *pgxpool.Pool
}

func NewWithdrawalRepo(pool *pgxpool.Pool) *WithdrawalRepo {
	return &WithdrawalRepo{pool: pool}
}

func (r *WithdrawalRepo) RequestWithdrawal(
	ctx context.Context,
	userID uuid.UUID,
	withdrawalID uuid.UUID,
	amount int64,
	destination json.RawMessage,
) (domain.Withdrawal, error) {
	var created domain.Withdrawal

	err := inTx(ctx, r.pool, func(tx pgx.Tx) error {
		wallet, err := scanWallet(tx.QueryRow(ctx,
			`SELECT `+walletColumns+` FROM wallets WHERE user_id = $1 FOR UPDATE`,
			userID,
		))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrInsufficientFunds
			}
			return fmt.Errorf("lock wallet for withdrawal: %w", mapError(err))
		}

		if err := wallet.EnsureActive(); err != nil {
			return err
		}
		if wallet.Available < amount {
			return domain.ErrInsufficientFunds
		}

		var newAvailable, newReserved int64
		err = tx.QueryRow(ctx, `
			UPDATE wallets
			SET available = available - $2,
				reserved = reserved + $2,
				version = version + 1
			WHERE id = $1
			RETURNING available, reserved`,
			wallet.ID, amount,
		).Scan(&newAvailable, &newReserved)
		if err != nil {
			return fmt.Errorf("lock withdrawal funds: %w", mapError(err))
		}

		var holdID uuid.UUID
		err = tx.QueryRow(ctx, `
			INSERT INTO holds (wallet_id, kind, ref_id, amount_total, status)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id`,
			wallet.ID, domain.HoldKindWithdrawal, withdrawalID, amount, domain.HoldActive,
		).Scan(&holdID)
		if err != nil {
			return fmt.Errorf("insert withdrawal hold: %w", mapError(err))
		}

		created, err = scanWithdrawal(tx.QueryRow(ctx, `
			INSERT INTO withdrawals (id, wallet_id, hold_id, amount, destination, status)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING `+withdrawalColumns,
			withdrawalID, wallet.ID, holdID, amount, destination, domain.WithdrawalPending,
		))
		if err != nil {
			return fmt.Errorf("insert withdrawal: %w", mapError(err))
		}

		wPayload, _ := json.Marshal(map[string]any{
			"withdrawal_id": created.ID.String(),
			"wallet_id":     created.WalletID.String(),
			"user_id":       userID.String(),
			"amount":        created.Amount,
			"status":        created.Status,
		})
		balPayload, _ := json.Marshal(map[string]any{
			"wallet_id": created.WalletID.String(),
			"user_id":   userID.String(),
			"available": newAvailable,
			"reserved":  newReserved,
		})

		_, err = tx.Exec(ctx, `
			INSERT INTO outbox (topic, key, payload)
			VALUES ('billing.withdrawal.updated', $1, $2),
			       ('billing.wallet.updated', $3, $4)`,
			created.ID.String(), wPayload,
			userID.String(), balPayload,
		)
		if err != nil {
			return fmt.Errorf("insert withdrawal outbox events: %w", mapError(err))
		}

		return nil
	})

	return created, err
}

func (r *WithdrawalRepo) CancelWithdrawal(ctx context.Context, userID, withdrawalID uuid.UUID) (domain.Withdrawal, error) {
	var updated domain.Withdrawal

	err := inTx(ctx, r.pool, func(tx pgx.Tx) error {
		w, err := scanWithdrawal(tx.QueryRow(ctx, `
			SELECT w.id, w.wallet_id, w.hold_id, w.amount, w.destination, w.status,
			       w.reviewed_by, w.journal_id, w.created_at, w.updated_at
			FROM withdrawals w
			JOIN wallets wl ON wl.id = w.wallet_id
			WHERE w.id = $1 AND wl.user_id = $2
			FOR UPDATE OF w, wl`,
			withdrawalID, userID,
		))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: withdrawal", domain.ErrNotFound)
			}
			return fmt.Errorf("lock withdrawal for cancel: %w", mapError(err))
		}

		if w.Status == domain.WithdrawalCancelled {
			updated = w
			return nil
		}
		if !w.Status.CanTransitionTo(domain.WithdrawalCancelled) {
			return domain.NewValidationError("status", "withdrawal can only be cancelled while PENDING")
		}

		_, err = tx.Exec(ctx, `
			UPDATE holds
			SET amount_released = amount_total - amount_consumed,
				status = $2
			WHERE id = $1 AND status = $3`,
			w.HoldID, domain.HoldReleased, domain.HoldActive,
		)
		if err != nil {
			return fmt.Errorf("release withdrawal hold: %w", mapError(err))
		}

		var newAvailable, newReserved int64
		err = tx.QueryRow(ctx, `
			UPDATE wallets
			SET available = available + $2,
				reserved = reserved - $2,
				version = version + 1
			WHERE id = $1
			RETURNING available, reserved`,
			w.WalletID, w.Amount,
		).Scan(&newAvailable, &newReserved)
		if err != nil {
			return fmt.Errorf("release reserved withdrawal funds: %w", mapError(err))
		}

		updated, err = scanWithdrawal(tx.QueryRow(ctx, `
			UPDATE withdrawals
			SET status = $2
			WHERE id = $1 AND status = $3
			RETURNING `+withdrawalColumns,
			w.ID, domain.WithdrawalCancelled, domain.WithdrawalPending,
		))
		if err != nil {
			return fmt.Errorf("mark withdrawal cancelled: %w", mapError(err))
		}

		wPayload, _ := json.Marshal(map[string]any{
			"withdrawal_id": updated.ID.String(),
			"wallet_id":     updated.WalletID.String(),
			"user_id":       userID.String(),
			"amount":        updated.Amount,
			"status":        updated.Status,
		})
		balPayload, _ := json.Marshal(map[string]any{
			"wallet_id": updated.WalletID.String(),
			"user_id":   userID.String(),
			"available": newAvailable,
			"reserved":  newReserved,
		})

		_, err = tx.Exec(ctx, `
			INSERT INTO outbox (topic, key, payload)
			VALUES ('billing.withdrawal.updated', $1, $2),
			       ('billing.wallet.updated', $3, $4)`,
			updated.ID.String(), wPayload,
			userID.String(), balPayload,
		)
		if err != nil {
			return fmt.Errorf("insert cancel withdrawal outbox events: %w", mapError(err))
		}

		return nil
	})

	return updated, err
}

func (r *WithdrawalRepo) ListWithdrawalsByUser(ctx context.Context, userID uuid.UUID, limit int) ([]domain.Withdrawal, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `
		SELECT w.id, w.wallet_id, w.hold_id, w.amount, w.destination, w.status,
		       w.reviewed_by, w.journal_id, w.created_at, w.updated_at
		FROM withdrawals w
		JOIN wallets wl ON wl.id = w.wallet_id
		WHERE wl.user_id = $1
		ORDER BY w.created_at DESC
		LIMIT $2`,
		userID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list withdrawals: %w", mapError(err))
	}
	defer rows.Close()

	var items []domain.Withdrawal
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return nil, fmt.Errorf("scan withdrawal: %w", mapError(err))
		}
		items = append(items, w)
	}
	return items, rows.Err()
}

func scanWithdrawal(row pgx.Row) (domain.Withdrawal, error) {
	var w domain.Withdrawal
	err := row.Scan(
		&w.ID, &w.WalletID, &w.HoldID, &w.Amount, &w.Destination, &w.Status,
		&w.ReviewedBy, &w.JournalID, &w.CreatedAt, &w.UpdatedAt,
	)
	return w, err
}
