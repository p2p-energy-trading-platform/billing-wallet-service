package postgres

import (
	"billing-wallet-service/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const depositColumns = `
	id, wallet_id, amount, provider, provider_ref, provider_payment_ref,
	charge_currency, charge_amount, fx_rate::text, status, expires_at, journal_id,
	created_at, updated_at`

type DepositRepo struct {
	pool *pgxpool.Pool
}

func NewDepositRepo(pool *pgxpool.Pool) *DepositRepo {
	return &DepositRepo{pool: pool}
}

func (r *DepositRepo) CreatePendingDeposits(ctx context.Context, userID uuid.UUID, d domain.Deposit) (domain.Deposit, error) {
	var created domain.Deposit
	err := inTx(ctx, r.pool, func(tx pgx.Tx) error {
		wallet, err := ensureWallet(ctx, tx, userID)
		if err != nil {
			return err
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO deposits (
				id, wallet_id, amount, provider, charge_currency, charge_amount, fx_rate, status, expires_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9)
			RETURNING `+depositColumns,
			d.ID, wallet.ID, d.Amount, d.Provider, d.ChargeCurrency, d.ChargeAmount, d.FxRate, domain.DepositPending, d.ExpiresAt,
		)

		created, err = scanDeposit(row)
		if err != nil {
			return fmt.Errorf("insert pending deposit: %w", mapError(err))
		}
		return nil
	})

	return created, err
}

func (r *DepositRepo) AttachProviderRef(ctx context.Context, depositID uuid.UUID, providerRef string) (domain.Deposit, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE deposits
		SET provider_ref = $2
		WHERE id = $1
		RETURNING `+depositColumns,
		depositID, providerRef,
	)
	dep, err := scanDeposit(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Deposit{}, fmt.Errorf("%w: deposit", domain.ErrNotFound)
		}
		return domain.Deposit{}, fmt.Errorf("attach provider_ref: %w", mapError(err))
	}
	return dep, nil
}

func (r *DepositRepo) GetDepositForUser(ctx context.Context, userID, depositID uuid.UUID) (domain.Deposit, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT d.id, d.wallet_id, d.amount, d.provider, d.provider_ref, d.provider_payment_ref,
		       d.charge_currency, d.charge_amount, d.fx_rate::text, d.status, d.expires_at, d.journal_id,
		       d.created_at, d.updated_at
		FROM deposits d
		JOIN wallets w ON w.id = d.wallet_id
		WHERE d.id = $1 AND w.user_id = $2`,
		depositID, userID,
	)
	dep, err := scanDeposit(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Deposit{}, fmt.Errorf("%w: deposit", domain.ErrNotFound)
		}
		return domain.Deposit{}, fmt.Errorf("get deposit: %w", mapError(err))
	}
	return dep, nil
}

type DepositConfirmOutcome struct {
	Processed       bool
	Deposit         domain.Deposit
	AmountMismatch  bool
	LateAfterExpiry bool
}

func (r *DepositRepo) ConfirmDepositWebhook(
	ctx context.Context,
	provider string,
	eventID string,
	eventType string,
	providerRef string,
	paymentIntentID string,
	paidCurrency string,
	paidAmount int64,
	isPaid bool,
	markTerminal *domain.DepositStatus, // DepositFailed or DepositExpired when not paid
) (DepositConfirmOutcome, error) {
	var outcome DepositConfirmOutcome

	err := inTx(ctx, r.pool, func(tx pgx.Tx) error {
		// Dedupe on provider events
		cmd, err := tx.Exec(ctx, `
			INSERT INTO provider_events (provider, event_id, type)
			VALUES ($1, $2, $3)
			ON CONFLICT (provider, event_id) DO NOTHING`,
			provider, eventID, eventType,
		)

		if err != nil {
			return fmt.Errorf("insert provider_events: %w", mapError(err))
		}

		if cmd.RowsAffected() == 0 {
			// Already processed this exact event.
			return nil
		}

		// Lock deposit row by provider_ref
		dep, err := scanDeposit(tx.QueryRow(ctx,
			`SELECT `+depositColumns+` FROM deposits WHERE provider_ref = $1 FOR UPDATE`,
			providerRef,
		))

		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Return error so transaction rolls back provider_events insert and Stripe retries
				// if webhook arrived before AttachProviderRef committed.
				return fmt.Errorf("%w: deposit for provider_ref %s", domain.ErrNotFound, providerRef)
			}
			return fmt.Errorf("lock deposit: %w", mapError(err))
		}

		outcome.Deposit = dep

		// Terminal failure/expiry handling
		if !isPaid {
			if markTerminal != nil && dep.Status.CanTransitionTo(*markTerminal) {
				updated, err := scanDeposit(tx.QueryRow(ctx, `
					UPDATE deposits SET status = $2 WHERE id = $1 RETURNING `+depositColumns,
					dep.ID, *markTerminal,
				))
				if err != nil {
					return fmt.Errorf("mark deposit %s: %w", *markTerminal, mapError(err))
				}
				outcome.Deposit = updated
				outcome.Processed = true
			}
			return nil
		}

		// Paid event scenario
		if !dep.Status.CanTransitionTo(domain.DepositCompleted) {
			// Already completed or in a non-creditable terminal state.
			return nil
		}

		if paidCurrency != "" && paidAmount > 0 {
			if paidCurrency != dep.ChargeCurrency || paidAmount != dep.ChargeAmount {
				outcome.AmountMismatch = true
				return nil
			}
		}

		if dep.Status == domain.DepositExpired {
			outcome.LateAfterExpiry = true
		}

		// Lock wallet and credit available balance
		var userID uuid.UUID
		var newAvailable, newReserved int64
		err = tx.QueryRow(ctx, `
			UPDATE wallets
			SET available = available + $2, version = version + 1
			WHERE id = $1
			RETURNING user_id, available, reserved`,
			dep.WalletID, dep.Amount,
		).Scan(&userID, &newAvailable, &newReserved)

		if err != nil {
			return fmt.Errorf("credit wallet: %w", mapError(err))
		}

		// Update ledger accounts
		var clearingAccountID, userAccountID uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT id FROM ledger_accounts WHERE type = $1 AND wallet_id IS NULL`,
			domain.AccountPaymentClearing,
		).Scan(&clearingAccountID); err != nil {
			return fmt.Errorf("lookup clearing account: %w", mapError(err))
		}

		if err := tx.QueryRow(ctx,
			`SELECT id FROM ledger_accounts WHERE type = $1 AND wallet_id = $2`,
			domain.AccountUserWallet, dep.WalletID,
		).Scan(&userAccountID); err != nil {
			return fmt.Errorf("lookup user wallet account: %w", mapError(err))
		}

		var journalID uuid.UUID
		err = tx.QueryRow(ctx, `
			INSERT INTO journals (type, idempotency_key, ref_type, ref_id, metadata)
			VALUES ($1, $2, 'DEPOSIT', $3, jsonb_build_object('provider', $4, 'provider_ref', $5, 'payment_intent_id', $6, 'late_after_expiry', $7))
			RETURNING id`,
			domain.JournalDeposit,
			"deposit:"+dep.ID.String(),
			dep.ID,
			provider,
			providerRef,
			paymentIntentID,
			outcome.LateAfterExpiry,
		).Scan(&journalID)
		if err != nil {
			return fmt.Errorf("insert deposit journal: %w", mapError(err))
		}

		totalAfter := newAvailable + newReserved
		lines := []domain.LedgerLine{
			{JournalID: journalID, AccountID: clearingAccountID, Amount: -dep.Amount},
			{JournalID: journalID, AccountID: userAccountID, Amount: dep.Amount, BalanceAfter: &totalAfter},
		}
		if err := domain.ValidateBalanced(lines); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO ledger_lines (journal_id, account_id, amount, balance_after)
			VALUES ($1, $2, $3, NULL), ($1, $4, $5, $6)`,
			journalID, clearingAccountID, -dep.Amount,
			userAccountID, dep.Amount, totalAfter,
		)
		if err != nil {
			return fmt.Errorf("insert deposit ledger_lines: %w", mapError(err))
		}

		updated, err := scanDeposit(tx.QueryRow(ctx, `
			UPDATE deposits
			SET status = $2,
				provider_payment_ref = NULLIF($3, ''),
				journal_id = $4
			WHERE id = $1
			RETURNING `+depositColumns,
			dep.ID, domain.DepositCompleted, paymentIntentID, journalID,
		))
		if err != nil {
			return fmt.Errorf("complete deposit: %w", mapError(err))
		}

		// outbox events
		depPayload, _ := json.Marshal(map[string]any{
			"deposit_id": updated.ID.String(),
			"wallet_id":  updated.WalletID.String(),
			"user_id":    userID.String(),
			"amount":     updated.Amount,
			"status":     updated.Status,
		})
		walletPayload, _ := json.Marshal(map[string]any{
			"wallet_id": updated.WalletID.String(),
			"user_id":   userID.String(),
			"available": newAvailable,
			"reserved":  newReserved,
		})

		_, err = tx.Exec(ctx, `
			INSERT INTO outbox (topic, key, payload)
			VALUES ('billing.deposit.completed', $1, $2),
			       ('billing.wallet.updated', $3, $4)`,
			updated.ID.String(), depPayload,
			userID.String(), walletPayload,
		)
		if err != nil {
			return fmt.Errorf("insert deposit outbox events: %w", mapError(err))
		}

		outcome.Deposit = updated
		outcome.Processed = true
		return nil
	})

	return outcome, err
}

func (r *DepositRepo) ListStalePendingDeposits(ctx context.Context, olderThan time.Time, limit int) ([]domain.Deposit, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+depositColumns+`
		FROM deposits
		WHERE status = 'PENDING'
		  AND (expires_at < now() OR created_at < $1)
		ORDER BY created_at ASC
		LIMIT $2`,
		olderThan, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list stale pending deposits: %w", mapError(err))
	}
	defer rows.Close()

	var list []domain.Deposit
	for rows.Next() {
		dep, err := scanDeposit(rows)
		if err != nil {
			return nil, fmt.Errorf("scan stale deposit: %w", mapError(err))
		}
		list = append(list, dep)
	}
	return list, rows.Err()
}

func scanDeposit(row pgx.Row) (domain.Deposit, error) {
	var d domain.Deposit
	err := row.Scan(
		&d.ID, &d.WalletID, &d.Amount, &d.Provider, &d.ProviderRef, &d.ProviderPaymentRef,
		&d.ChargeCurrency, &d.ChargeAmount, &d.FxRate, &d.Status, &d.ExpiresAt, &d.JournalID,
		&d.CreatedAt, &d.UpdatedAt,
	)
	return d, err
}
