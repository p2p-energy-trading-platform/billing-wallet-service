//go:build integration

// Package integration runs the service against real dependencies started with
// testcontainers-go. Run with:
//
//	go test -tags integration ./test/integration/...
//
// Tests skip (not fail) when Docker is unavailable.
package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"billing-wallet-service/migrations"

	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	postgresImage = "postgres:16-alpine" // same image as gridx-infra

	adminUser   = "gridx_admin"
	serviceUser = "billing_service_user"
	password    = "test-password"
	database    = "gridx"

	sqlStateCheck      = "23514"
	sqlStateUnique     = "23505"
	sqlStateForeignKey = "23503"
	sqlStateRaised     = "P0001" // RAISE EXCEPTION, used by the append-only guard
)

// bootstrap mirrors gridx-infra/init-scripts/postgres/01-init-schemas.sql, so
// migrations run as the same non-superuser schema owner they run as for real. A
// superuser would skip the permission checks that role is subject to.
var bootstrap = []string{
	fmt.Sprintf(`CREATE USER %s WITH PASSWORD '%s'`, serviceUser, password),
	fmt.Sprintf(`CREATE SCHEMA billing_data AUTHORIZATION %s`, serviceUser),
	fmt.Sprintf(`ALTER ROLE %s SET search_path TO billing_data, public`, serviceUser),
	`REVOKE ALL ON SCHEMA public FROM PUBLIC`,
	fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, serviceUser),
}

func openDB(t *testing.T, url string) *sql.DB {
	t.Helper()

	config, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}

	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = db.Close() })

	return db
}

// startPostgres returns a connection as the service role to a database that is
// bootstrapped like gridx-infra but has no migrations applied.
func startPostgres(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()

	port := nat.Port("5432/tcp")
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{string(port)},
			Env: map[string]string{
				"POSTGRES_USER":     adminUser,
				"POSTGRES_PASSWORD": password,
				"POSTGRES_DB":       database,
			},
			WaitingFor: wait.ForSQL(port, "pgx", func(host string, p nat.Port) string {
				return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", adminUser, password, host, p.Port(), database)
			}).WithStartupTimeout(time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	mapped, err := container.MappedPort(ctx, port)
	if err != nil {
		t.Fatalf("container port: %v", err)
	}

	admin := openDB(t, fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", adminUser, password, host, mapped.Port(), database))
	for _, statement := range bootstrap {
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("bootstrap %q: %v", statement, err)
		}
	}

	return openDB(t, fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", serviceUser, password, host, mapped.Port(), database))
}

func newProvider(t *testing.T, db *sql.DB) *goose.Provider {
	t.Helper()

	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		db,
		migrations.FS,
		goose.WithSlog(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("create migration provider: %v", err)
	}

	return provider
}

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()

	db := startPostgres(t)
	if _, err := newProvider(t, db).Up(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()

	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func insertReturningID(t *testing.T, db *sql.DB, query string, args ...any) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", query, err)
	}

	return id
}

func insertWallet(t *testing.T, db *sql.DB) uuid.UUID {
	t.Helper()

	return insertReturningID(t, db, `INSERT INTO wallets (user_id) VALUES ($1) RETURNING id`, uuid.New())
}

func insertJournal(t *testing.T, db *sql.DB) uuid.UUID {
	t.Helper()

	return insertReturningID(t, db,
		`INSERT INTO journals (type, idempotency_key, ref_type, ref_id)
		 VALUES ('ADJUSTMENT', $1, 'test', $2) RETURNING id`,
		"test:"+uuid.NewString(), uuid.New())
}

func systemAccount(t *testing.T, db *sql.DB, accountType string) uuid.UUID {
	t.Helper()

	return insertReturningID(t, db, `SELECT id FROM ledger_accounts WHERE type = $1`, accountType)
}

func TestMigrationsCreateAllTablesInBillingSchema(t *testing.T) {
	db := migratedDB(t)

	rows, err := db.QueryContext(context.Background(),
		`SELECT table_name FROM information_schema.tables WHERE table_schema = 'billing_data'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read table names: %v", err)
	}

	for _, want := range []string{
		"wallets", "holds", "ledger_accounts", "journals", "ledger_lines",
		"trade_settlements", "fee_configs", "deposits", "provider_events",
		"withdrawals", "outbox",
	} {
		if !slices.Contains(tables, want) {
			t.Errorf("table %q not found in billing_data, got %v", want, tables)
		}
	}
}

func TestMigrationsRollBackAndReapply(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t)
	provider := newProvider(t, db)

	insertWallet(t, db)
	insertJournal(t, db)

	if _, err := provider.DownTo(ctx, 0); err != nil {
		t.Fatalf("migrate down to zero: %v", err)
	}

	var remaining int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'billing_data' AND table_name <> 'goose_db_version'`).Scan(&remaining)
	if err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d tables left after rolling back every migration", remaining)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
}

func TestSeedData(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t)

	for _, accountType := range []string{"PLATFORM_REVENUE", "PAYMENT_CLEARING"} {
		var count int
		err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM ledger_accounts WHERE type = $1 AND wallet_id IS NULL`, accountType).Scan(&count)
		if err != nil {
			t.Fatalf("count %s: %v", accountType, err)
		}
		if count != 1 {
			t.Errorf("%s accounts = %d, want 1", accountType, count)
		}
	}

	var buyerBps, sellerBps int
	err := db.QueryRowContext(ctx,
		`SELECT buyer_fee_bps, seller_fee_bps FROM fee_configs
		 WHERE effective_from <= now() ORDER BY effective_from DESC LIMIT 1`).Scan(&buyerBps, &sellerBps)
	if err != nil {
		t.Fatalf("read active fee config: %v", err)
	}
	if buyerBps != 100 || sellerBps != 100 {
		t.Errorf("active fee config = %d/%d bps, want 100/100", buyerBps, sellerBps)
	}
}

func TestConstraints(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t)

	walletID := insertWallet(t, db)
	userWithWallet := uuid.New()
	exec(t, db, `INSERT INTO wallets (user_id) VALUES ($1)`, userWithWallet)
	journalID := insertJournal(t, db)
	revenueID := systemAccount(t, db, "PLATFORM_REVENUE")
	exec(t, db, `INSERT INTO provider_events (provider, event_id, type) VALUES ('stripe', 'evt_taken', 'x')`)
	exec(t, db, `INSERT INTO journals (type, idempotency_key, ref_type, ref_id)
	             VALUES ('ADJUSTMENT', 'key_taken', 'test', $1)`, uuid.New())
	exec(t, db, `INSERT INTO deposits (wallet_id, amount, provider_ref, charge_currency, charge_amount, status)
	             VALUES ($1, 100, 'cs_taken', 'lkr', 100, 'PENDING')`, walletID)
	orderRef := uuid.New()

	const insertOrderHold = `INSERT INTO holds
		(wallet_id, kind, ref_id, order_type, qty_wh, max_price_per_kwh, buyer_fee_bps, buyer_fee_config_id,
		 amount_total, status)
		VALUES ($1, 'ORDER', $2, 'LIMIT', 1000, 4550, 100, $3, 4600, 'ACTIVE')`
	exec(t, db, insertOrderHold, walletID, orderRef, uuid.New())

	const insertWithdrawalHold = `INSERT INTO holds (wallet_id, kind, ref_id, amount_total, status)
		VALUES ($1, 'WITHDRAWAL', $2, 100, 'ACTIVE')`

	tests := []struct {
		name     string
		query    string
		args     []any
		wantCode string // empty means the statement must succeed
	}{
		// wallets
		{name: "wallet with negative available", query: `INSERT INTO wallets (user_id, available) VALUES ($1, -1)`, args: []any{uuid.New()}, wantCode: sqlStateCheck},
		{name: "wallet with negative reserved", query: `INSERT INTO wallets (user_id, reserved) VALUES ($1, -1)`, args: []any{uuid.New()}, wantCode: sqlStateCheck},
		{name: "wallet with unknown status", query: `INSERT INTO wallets (user_id, status) VALUES ($1, 'NOPE')`, args: []any{uuid.New()}, wantCode: sqlStateCheck},
		{name: "second wallet for a user", query: `INSERT INTO wallets (user_id) VALUES ($1)`, args: []any{userWithWallet}, wantCode: sqlStateUnique},

		// holds
		{name: "valid order hold", query: insertOrderHold, args: []any{walletID, uuid.New(), uuid.New()}},
		{name: "valid withdrawal hold", query: insertWithdrawalHold, args: []any{walletID, uuid.New()}},
		{name: "second order hold for the same order", query: insertOrderHold, args: []any{walletID, orderRef, uuid.New()}, wantCode: sqlStateUnique},
		{name: "hold on unknown wallet", query: insertWithdrawalHold, args: []any{uuid.New(), uuid.New()}, wantCode: sqlStateForeignKey},
		{name: "hold with zero total", query: `INSERT INTO holds (wallet_id, kind, ref_id, amount_total, status) VALUES ($1, 'WITHDRAWAL', $2, 0, 'ACTIVE')`, args: []any{walletID, uuid.New()}, wantCode: sqlStateCheck},
		{name: "hold over-consumed", query: `INSERT INTO holds (wallet_id, kind, ref_id, amount_total, amount_consumed, amount_released, status) VALUES ($1, 'WITHDRAWAL', $2, 100, 60, 41, 'ACTIVE')`, args: []any{walletID, uuid.New()}, wantCode: sqlStateCheck},
		{name: "hold with unknown status", query: `INSERT INTO holds (wallet_id, kind, ref_id, amount_total, status) VALUES ($1, 'WITHDRAWAL', $2, 100, 'NOPE')`, args: []any{walletID, uuid.New()}, wantCode: sqlStateCheck},
		{name: "order hold missing its order fields", query: `INSERT INTO holds (wallet_id, kind, ref_id, amount_total, status) VALUES ($1, 'ORDER', $2, 100, 'ACTIVE')`, args: []any{walletID, uuid.New()}, wantCode: sqlStateCheck},
		{name: "withdrawal hold with an order field", query: `INSERT INTO holds (wallet_id, kind, ref_id, qty_wh, amount_total, status) VALUES ($1, 'WITHDRAWAL', $2, 1000, 100, 'ACTIVE')`, args: []any{walletID, uuid.New()}, wantCode: sqlStateCheck},

		// ledger_accounts
		{name: "user wallet account", query: `INSERT INTO ledger_accounts (type, wallet_id, name) VALUES ('USER_WALLET', $1, 'wallet')`, args: []any{walletID}},
		{name: "duplicate system account", query: `INSERT INTO ledger_accounts (type, name) VALUES ('PLATFORM_REVENUE', 'again')`, wantCode: sqlStateUnique},
		{name: "user wallet account without a wallet", query: `INSERT INTO ledger_accounts (type, name) VALUES ('USER_WALLET', 'none')`, wantCode: sqlStateCheck},
		{name: "system account with a wallet", query: `INSERT INTO ledger_accounts (type, wallet_id, name) VALUES ('PAYMENT_CLEARING', $1, 'bad')`, args: []any{walletID}, wantCode: sqlStateCheck},

		// journals and ledger_lines
		{name: "journal with a used idempotency key", query: `INSERT INTO journals (type, idempotency_key, ref_type, ref_id) VALUES ('DEPOSIT', 'key_taken', 'test', $1)`, args: []any{uuid.New()}, wantCode: sqlStateUnique},
		{name: "journal with unknown type", query: `INSERT INTO journals (type, idempotency_key, ref_type, ref_id) VALUES ('NOPE', $1, 'test', $2)`, args: []any{uuid.NewString(), uuid.New()}, wantCode: sqlStateCheck},
		{name: "ledger line with zero amount", query: `INSERT INTO ledger_lines (journal_id, account_id, amount) VALUES ($1, $2, 0)`, args: []any{journalID, revenueID}, wantCode: sqlStateCheck},
		{name: "ledger line on unknown journal", query: `INSERT INTO ledger_lines (journal_id, account_id, amount) VALUES ($1, $2, 5)`, args: []any{uuid.New(), revenueID}, wantCode: sqlStateForeignKey},

		// fee_configs
		{name: "fee config at the maximum", query: `INSERT INTO fee_configs (buyer_fee_bps, seller_fee_bps, effective_from) VALUES (2000, 2000, now())`},
		{name: "buyer fee above the maximum", query: `INSERT INTO fee_configs (buyer_fee_bps, seller_fee_bps, effective_from) VALUES (2001, 100, now())`, wantCode: sqlStateCheck},
		{name: "seller fee below zero", query: `INSERT INTO fee_configs (buyer_fee_bps, seller_fee_bps, effective_from) VALUES (100, -1, now())`, wantCode: sqlStateCheck},

		// deposits and provider_events
		{name: "deposit with zero amount", query: `INSERT INTO deposits (wallet_id, amount, charge_currency, charge_amount, status) VALUES ($1, 0, 'lkr', 100, 'PENDING')`, args: []any{walletID}, wantCode: sqlStateCheck},
		{name: "deposit with a used provider ref", query: `INSERT INTO deposits (wallet_id, amount, provider_ref, charge_currency, charge_amount, status) VALUES ($1, 100, 'cs_taken', 'lkr', 100, 'PENDING')`, args: []any{walletID}, wantCode: sqlStateUnique},
		{name: "deposit with unknown status", query: `INSERT INTO deposits (wallet_id, amount, charge_currency, charge_amount, status) VALUES ($1, 100, 'lkr', 100, 'NOPE')`, args: []any{walletID}, wantCode: sqlStateCheck},
		{name: "replayed provider event", query: `INSERT INTO provider_events (provider, event_id, type) VALUES ('stripe', 'evt_taken', 'x')`, wantCode: sqlStateUnique},

		// withdrawals
		{name: "withdrawal on unknown hold", query: `INSERT INTO withdrawals (wallet_id, hold_id, amount, destination, status) VALUES ($1, $2, 100, '{}', 'PENDING')`, args: []any{walletID, uuid.New()}, wantCode: sqlStateForeignKey},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()

			_, err = tx.ExecContext(ctx, tt.query, tt.args...)

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("expected statement to succeed: %v", err)
				}
				return
			}

			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("error = %v, want SQLSTATE %s", err, tt.wantCode)
			}
			if pgErr.Code != tt.wantCode {
				t.Fatalf("SQLSTATE = %s (%s), want %s", pgErr.Code, pgErr.Message, tt.wantCode)
			}
		})
	}
}

func TestUpdatedAtIsMaintained(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t)

	walletID := insertWallet(t, db)
	holdID := insertReturningID(t, db,
		`INSERT INTO holds (wallet_id, kind, ref_id, amount_total, status)
		 VALUES ($1, 'WITHDRAWAL', $2, 100, 'ACTIVE') RETURNING id`, walletID, uuid.New())
	depositID := insertReturningID(t, db,
		`INSERT INTO deposits (wallet_id, amount, charge_currency, charge_amount, status)
		 VALUES ($1, 100, 'lkr', 100, 'PENDING') RETURNING id`, walletID)
	withdrawalID := insertReturningID(t, db,
		`INSERT INTO withdrawals (wallet_id, hold_id, amount, destination, status)
		 VALUES ($1, $2, 100, '{}', 'PENDING') RETURNING id`, walletID, holdID)

	tests := []struct {
		table  string
		id     uuid.UUID
		change string
	}{
		{table: "wallets", id: walletID, change: "available = 5"},
		{table: "holds", id: holdID, change: "amount_consumed = 5"},
		{table: "deposits", id: depositID, change: "status = 'COMPLETED'"},
		{table: "withdrawals", id: withdrawalID, change: "status = 'APPROVED'"},
	}

	for _, tt := range tests {
		t.Run(tt.table, func(t *testing.T) {
			// The trigger must overwrite the stale value this UPDATE tries to set.
			var before time.Time
			if err := db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
				t.Fatalf("read clock: %v", err)
			}

			exec(t, db, fmt.Sprintf(`UPDATE %s SET %s, updated_at = '2000-01-01' WHERE id = $1`, tt.table, tt.change), tt.id)

			var updatedAt time.Time
			query := fmt.Sprintf(`SELECT updated_at FROM %s WHERE id = $1`, tt.table)
			if err := db.QueryRowContext(ctx, query, tt.id).Scan(&updatedAt); err != nil {
				t.Fatalf("read updated_at: %v", err)
			}
			if updatedAt.Before(before) {
				t.Errorf("updated_at = %v, want at or after %v", updatedAt, before)
			}
		})
	}
}

func TestLedgerIsImmutable(t *testing.T) {
	db := migratedDB(t)

	journalID := insertJournal(t, db)
	revenueID := systemAccount(t, db, "PLATFORM_REVENUE")
	clearingID := systemAccount(t, db, "PAYMENT_CLEARING")
	exec(t, db, `INSERT INTO ledger_lines (journal_id, account_id, amount) VALUES ($1, $2, 100), ($1, $3, -100)`,
		journalID, revenueID, clearingID)

	for _, statement := range []string{
		`UPDATE journals SET ref_type = 'changed'`,
		`DELETE FROM journals`,
		`TRUNCATE journals CASCADE`,
		`UPDATE ledger_lines SET amount = 1`,
		`DELETE FROM ledger_lines`,
		`TRUNCATE ledger_lines`,
	} {
		t.Run(statement, func(t *testing.T) {
			_, err := db.ExecContext(context.Background(), statement)

			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != sqlStateRaised {
				t.Errorf("%q error = %v, want the append-only guard (SQLSTATE %s)", statement, err, sqlStateRaised)
			}

			var lines int
			err = db.QueryRowContext(context.Background(),
				`SELECT count(*) FROM ledger_lines WHERE journal_id = $1 AND amount IN (100, -100)`, journalID).Scan(&lines)
			if err != nil {
				t.Fatalf("count lines: %v", err)
			}
			if lines != 2 {
				t.Errorf("ledger lines left = %d, want 2", lines)
			}
		})
	}
}

// Every table that references a journal must still accept rows while the ledger
// is append-only, which is the normal settlement, deposit and withdrawal path.
func TestRowsCanReferenceImmutableJournals(t *testing.T) {
	db := migratedDB(t)

	walletID := insertWallet(t, db)
	sellerID := insertWallet(t, db)
	holdID := insertReturningID(t, db,
		`INSERT INTO holds (wallet_id, kind, ref_id, amount_total, status)
		 VALUES ($1, 'WITHDRAWAL', $2, 100, 'ACTIVE') RETURNING id`, walletID, uuid.New())

	exec(t, db, `INSERT INTO ledger_lines (journal_id, account_id, amount) VALUES ($1, $2, 5)`,
		insertJournal(t, db), systemAccount(t, db, "PLATFORM_REVENUE"))
	exec(t, db, `INSERT INTO deposits (wallet_id, amount, charge_currency, charge_amount, status, journal_id)
	             VALUES ($1, 100, 'lkr', 100, 'COMPLETED', $2)`, walletID, insertJournal(t, db))
	exec(t, db, `INSERT INTO withdrawals (wallet_id, hold_id, amount, destination, status, journal_id)
	             VALUES ($1, $2, 100, '{}', 'PAID', $3)`, walletID, holdID, insertJournal(t, db))
	exec(t, db, `INSERT INTO trade_settlements
		(trade_id, journal_id, buyer_wallet_id, seller_wallet_id, buy_order_id, sell_order_id,
		 qty_wh, price_per_kwh, trade_value, buyer_fee, seller_fee, buyer_fee_bps, seller_fee_bps,
		 buyer_fee_config_id, seller_fee_config_id)
		VALUES ($1, $2, $3, $4, $5, $6, 1000, 4550, 4550, 46, 46, 100, 100, $7, $8)`,
		uuid.New(), insertJournal(t, db), walletID, sellerID, uuid.New(), uuid.New(), uuid.New(), uuid.New())
}
