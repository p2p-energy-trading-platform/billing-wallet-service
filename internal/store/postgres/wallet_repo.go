package postgres

import (
	"context"
	"errors"
	"fmt"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const walletColumns = `id, user_id, available, reserved, status, version, created_at, updated_at`

type WalletRepo struct {
	pool *pgxpool.Pool
}

func NewWalletRepo(pool *pgxpool.Pool) *WalletRepo {
	return &WalletRepo{pool: pool}
}

func (r *WalletRepo) FindWalletByUserID(ctx context.Context, userID uuid.UUID) (domain.Wallet, error) {
	wallet, err := scanWallet(r.pool.QueryRow(ctx,
		`SELECT `+walletColumns+` FROM wallets WHERE user_id = $1`, userID))
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("find wallet: %w", mapNotFound(err))
	}

	return wallet, nil
}

// CreateWallet returns the user's wallet, creating it if needed.
func (r *WalletRepo) CreateWallet(ctx context.Context, userID uuid.UUID) (domain.Wallet, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("begin create wallet: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	wallet, err := ensureWallet(ctx, tx, userID)
	if err != nil {
		return domain.Wallet{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Wallet{}, fmt.Errorf("commit create wallet: %w", mapError(err))
	}

	return wallet, nil
}

// ensureWallet creates the wallet and its ledger account together, so a wallet
// never exists without an account to post to. When a concurrent transaction wins
// the insert, this waits for it to commit and returns that wallet.
func ensureWallet(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (domain.Wallet, error) {
	wallet, err := scanWallet(tx.QueryRow(ctx,
		`INSERT INTO wallets (user_id) VALUES ($1)
		 ON CONFLICT (user_id) DO NOTHING
		 RETURNING `+walletColumns, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		wallet, err = scanWallet(tx.QueryRow(ctx,
			`SELECT `+walletColumns+` FROM wallets WHERE user_id = $1`, userID))
		if err != nil {
			return domain.Wallet{}, fmt.Errorf("read existing wallet: %w", mapError(err))
		}

		return wallet, nil
	}
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("insert wallet: %w", mapError(err))
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO ledger_accounts (type, wallet_id, name) VALUES ($1, $2, $3)`,
		domain.AccountUserWallet, wallet.ID, "wallet:"+userID.String())
	if err != nil {
		return domain.Wallet{}, fmt.Errorf("insert ledger account: %w", mapError(err))
	}

	return wallet, nil
}

func scanWallet(row pgx.Row) (domain.Wallet, error) {
	var wallet domain.Wallet
	err := row.Scan(
		&wallet.ID,
		&wallet.UserID,
		&wallet.Available,
		&wallet.Reserved,
		&wallet.Status,
		&wallet.Version,
		&wallet.CreatedAt,
		&wallet.UpdatedAt,
	)

	return wallet, err
}

func mapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: wallet", domain.ErrNotFound)
	}

	return mapError(err)
}
