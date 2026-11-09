//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"billing-wallet-service/internal/service"
	"billing-wallet-service/internal/store/postgres"
	transportgrpc "billing-wallet-service/internal/transport/grpc"
	grpcservices "billing-wallet-service/internal/transport/grpc/services"

	"github.com/google/uuid"
	billingv1 "github.com/p2p-energy-trading-platform/go-sdk/gen/gridx/billing/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// startServer runs the real gRPC server and store against the database and returns
// a connection to it over real networking.
func startServer(t *testing.T, serviceURL string) *grpc.ClientConn {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	pool, err := postgres.NewPool(context.Background(), serviceURL)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)

	feeConfigs := postgres.NewFeeConfigRepo(pool)
	server := transportgrpc.New("127.0.0.1:0", logger, transportgrpc.Services{
		Wallet: grpcservices.NewWalletHandler(service.NewWalletUsecase(postgres.NewWalletRepo(pool)), logger),
		Order:  grpcservices.NewOrderHandler(service.NewReservationUsecase(postgres.NewReservationRepo(pool), feeConfigs), logger),
		Admin:  grpcservices.NewAdminHandler(service.NewFeeConfigUsecase(feeConfigs, noCache{}), logger),
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// noCache leaves caching out of the integration server, so a test can change rates
// directly in the database and see them at once.
type noCache struct{}

func (noCache) Invalidate() {}

func startWalletServer(t *testing.T, serviceURL string) billingv1.WalletServiceClient {
	t.Helper()

	return billingv1.NewWalletServiceClient(startServer(t, serviceURL))
}

func asUser(userID uuid.UUID) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "x-gridx-user-id", userID.String())
}

func getWallet(t *testing.T, client billingv1.WalletServiceClient, userID uuid.UUID) *billingv1.Wallet {
	t.Helper()

	response, err := client.GetWallet(asUser(userID), &billingv1.GetWalletRequest{})
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}

	return response.GetWallet()
}

func TestGetWalletCreatesAnEmptyActiveWalletOnFirstUse(t *testing.T) {
	database := migratedPostgres(t)
	client := startWalletServer(t, database.serviceURL)
	userID := uuid.New()

	wallet := getWallet(t, client, userID)

	if wallet.GetUserId() != userID.String() {
		t.Errorf("user id = %q, want %v", wallet.GetUserId(), userID)
	}
	if wallet.GetAvailable() != 0 || wallet.GetReserved() != 0 || wallet.GetTotal() != 0 {
		t.Errorf("balances = %d/%d/%d, want all zero", wallet.GetAvailable(), wallet.GetReserved(), wallet.GetTotal())
	}
	if wallet.GetStatus() != billingv1.WalletStatus_WALLET_STATUS_ACTIVE {
		t.Errorf("status = %v, want ACTIVE", wallet.GetStatus())
	}
	if wallet.GetUpdatedAt() == nil {
		t.Error("updated_at is not set")
	}

	var accounts int
	err := database.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM ledger_accounts WHERE type = 'USER_WALLET' AND wallet_id = $1`, wallet.GetWalletId()).Scan(&accounts)
	if err != nil {
		t.Fatalf("count ledger accounts: %v", err)
	}
	if accounts != 1 {
		t.Errorf("ledger accounts = %d, want 1", accounts)
	}
}

func TestGetWalletReturnsTheSameWalletOnLaterCalls(t *testing.T) {
	database := migratedPostgres(t)
	client := startWalletServer(t, database.serviceURL)
	userID := uuid.New()

	first := getWallet(t, client, userID)
	second := getWallet(t, client, userID)

	if first.GetWalletId() != second.GetWalletId() {
		t.Errorf("wallet id changed from %q to %q", first.GetWalletId(), second.GetWalletId())
	}
}

func TestGetWalletConcurrentFirstCallsCreateOneWallet(t *testing.T) {
	database := migratedPostgres(t)
	client := startWalletServer(t, database.serviceURL)
	userID := uuid.New()

	const callers = 20
	ids := make([]string, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			response, err := client.GetWallet(asUser(userID), &billingv1.GetWalletRequest{})
			errs[i] = err
			ids[i] = response.GetWallet().GetWalletId()
		})
	}
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Errorf("caller %d got wallet %q, caller 0 got %q", i, ids[i], ids[0])
		}
	}

	var wallets, accounts int
	ctx := context.Background()
	if err := database.db.QueryRowContext(ctx, `SELECT count(*) FROM wallets WHERE user_id = $1`, userID).Scan(&wallets); err != nil {
		t.Fatalf("count wallets: %v", err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT count(*) FROM ledger_accounts WHERE type = 'USER_WALLET'`).Scan(&accounts); err != nil {
		t.Fatalf("count ledger accounts: %v", err)
	}
	if wallets != 1 || accounts != 1 {
		t.Errorf("wallets = %d, ledger accounts = %d, want 1 and 1", wallets, accounts)
	}
}

func TestGetWalletOnlyReturnsTheCallersWallet(t *testing.T) {
	database := migratedPostgres(t)
	client := startWalletServer(t, database.serviceURL)
	alice, bob := uuid.New(), uuid.New()

	aliceWallet := getWallet(t, client, alice)
	exec(t, database.db, `UPDATE wallets SET available = 500 WHERE id = $1`, aliceWallet.GetWalletId())

	bobWallet := getWallet(t, client, bob)

	if bobWallet.GetWalletId() == aliceWallet.GetWalletId() {
		t.Error("two users share a wallet")
	}
	if bobWallet.GetAvailable() != 0 {
		t.Errorf("bob's balance = %d, want 0", bobWallet.GetAvailable())
	}
}

func TestGetWalletReportsBalancesAndAccountState(t *testing.T) {
	statuses := map[string]billingv1.WalletStatus{
		"ACTIVE": billingv1.WalletStatus_WALLET_STATUS_ACTIVE,
		"FROZEN": billingv1.WalletStatus_WALLET_STATUS_FROZEN,
		"CLOSED": billingv1.WalletStatus_WALLET_STATUS_CLOSED,
	}

	database := migratedPostgres(t)
	client := startWalletServer(t, database.serviceURL)
	userID := uuid.New()
	walletID := getWallet(t, client, userID).GetWalletId()

	for status, want := range statuses {
		t.Run(status, func(t *testing.T) {
			exec(t, database.db, `UPDATE wallets SET available = 700, reserved = 300, status = $1 WHERE id = $2`, status, walletID)

			wallet := getWallet(t, client, userID)

			if wallet.GetStatus() != want {
				t.Errorf("status = %v, want %v", wallet.GetStatus(), want)
			}
			if wallet.GetAvailable() != 700 || wallet.GetReserved() != 300 || wallet.GetTotal() != 1000 {
				t.Errorf("balances = %d/%d/%d, want 700/300/1000", wallet.GetAvailable(), wallet.GetReserved(), wallet.GetTotal())
			}
		})
	}
}

func TestGetWalletRequiresACaller(t *testing.T) {
	database := migratedPostgres(t)
	client := startWalletServer(t, database.serviceURL)

	_, err := client.GetWallet(context.Background(), &billingv1.GetWalletRequest{})

	if code := status.Code(err); code != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", code)
	}
}

func TestGetWalletIsUnavailableWhenTheDatabaseIsDown(t *testing.T) {
	database := migratedPostgres(t)
	client := startWalletServer(t, database.serviceURL)
	userID := uuid.New()
	getWallet(t, client, userID)

	ctx := context.Background()
	stopTimeout := 10 * time.Second
	if err := database.container.Stop(ctx, &stopTimeout); err != nil {
		t.Fatalf("stop database: %v", err)
	}

	callCtx, cancel := context.WithTimeout(asUser(userID), 10*time.Second)
	defer cancel()
	_, err := client.GetWallet(callCtx, &billingv1.GetWalletRequest{})

	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("error = %v, want Unavailable", err)
	}
}
