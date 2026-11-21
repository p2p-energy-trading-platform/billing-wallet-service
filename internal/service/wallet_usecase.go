package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"billing-wallet-service/internal/domain"
	"billing-wallet-service/internal/provider/stripe"
	"billing-wallet-service/internal/store/postgres"

	"github.com/google/uuid"
)

type WalletStore interface {
	FindWalletByUserID(ctx context.Context, userID uuid.UUID) (domain.Wallet, error)
	// CreateWallet returns the user's existing wallet if another request created it first.
	CreateWallet(ctx context.Context, userID uuid.UUID) (domain.Wallet, error)
}

type DepositStore interface {
	CreatePendingDeposits(ctx context.Context, userID uuid.UUID, d domain.Deposit) (domain.Deposit, error)
	AttachProviderRef(ctx context.Context, depositID uuid.UUID, providerRef string) (domain.Deposit, error)
	GetDepositForUser(ctx context.Context, userID, depositID uuid.UUID) (domain.Deposit, error)
	ConfirmDepositWebhook(
		ctx context.Context,
		provider, eventID, eventType, providerRef, paymentIntentID, paidCurrency string,
		paidAmount int64,
		isPaid bool,
		markTerminal *domain.DepositStatus,
	) (postgres.DepositConfirmOutcome, error)
	ListStalePendingDeposits(ctx context.Context, olderThan time.Time, limit int) ([]domain.Deposit, error)
}

type WithdrawalStore interface {
	RequestWithdrawal(ctx context.Context, userID, withdrawalID uuid.UUID, amount int64, destination json.RawMessage) (domain.Withdrawal, error)
	CancelWithdrawal(ctx context.Context, userID, withdrawalID uuid.UUID) (domain.Withdrawal, error)
	ListWithdrawalsByUser(ctx context.Context, userID uuid.UUID, limit int) ([]domain.Withdrawal, error)
}

type DepositConfig struct {
	ChargeCurrency   string        // "lkr" or "usd"
	CreditsPerUSD    int64         // used only when ChargeCurrency == "usd" (e.g., 300 credits per 1 USD)
	MinDepositAmount int64         // minor units
	MaxDepositAmount int64         // minor units
	SessionTTL       time.Duration // e.g., 30 * time.Minute
	MinWithdrawal    int64         // minor units
}

type CreateDepositInput struct {
	UserID         uuid.UUID
	IdempotencyKey string
	Amount         int64
	SuccessURL     string
	CancelURL      string
}

type DepositIntentResult struct {
	Deposit     domain.Deposit
	CheckoutURL string
}

type WalletUsecase struct {
	store    WalletStore
	dStore   DepositStore
	wStore   WithdrawalStore
	provider stripe.PaymentProvider
	cfg      DepositConfig
	logger   *slog.Logger
}

func NewWalletUsecase(
	store WalletStore,
	dStore DepositStore,
	wStore WithdrawalStore,
	provider stripe.PaymentProvider,
	cfg DepositConfig,
	logger *slog.Logger,
) *WalletUsecase {
	if cfg.ChargeCurrency == "" {
		cfg.ChargeCurrency = "lkr"
	}

	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 30 * time.Minute
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &WalletUsecase{
		store:    store,
		dStore:   dStore,
		wStore:   wStore,
		provider: provider,
		cfg:      cfg,
		logger:   logger,
	}
}

// GetWallet returns the user's wallet. Wallets are created lazily on first use.
func (u *WalletUsecase) GetWallet(ctx context.Context, userID uuid.UUID) (domain.Wallet, error) {
	wallet, err := u.store.FindWalletByUserID(ctx, userID)
	if errors.Is(err, domain.ErrNotFound) {
		return u.store.CreateWallet(ctx, userID)
	}

	return wallet, err
}

func (u *WalletUsecase) CreateDepositIntent(ctx context.Context, in CreateDepositInput) (DepositIntentResult, error) {
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return DepositIntentResult{}, domain.NewValidationError("idempotency_key", "is required")
	}
	if in.Amount <= 0 {
		return DepositIntentResult{}, domain.NewBadAmountError("amount", "must be positive")
	}
	if u.cfg.MinDepositAmount > 0 && in.Amount < u.cfg.MinDepositAmount {
		return DepositIntentResult{}, domain.NewBadAmountError("amount", fmt.Sprintf("minimum deposit is %d", u.cfg.MinDepositAmount))
	}
	if u.cfg.MaxDepositAmount > 0 && in.Amount > u.cfg.MaxDepositAmount {
		return DepositIntentResult{}, domain.NewBadAmountError("amount", fmt.Sprintf("maximum deposit is %d", u.cfg.MaxDepositAmount))
	}
	if in.SuccessURL == "" || in.CancelURL == "" {
		return DepositIntentResult{}, domain.NewValidationError("redirect_urls", "success_url and cancel_url are required")
	}

	chargeCurrency, chargeAmount, fxRate, err := u.computeCharge(in.Amount)
	if err != nil {
		return DepositIntentResult{}, err
	}

	expiresAt := time.Now().UTC().Add(u.cfg.SessionTTL)
	depositID := uuid.New()

	dep, err := u.dStore.CreatePendingDeposits(ctx, in.UserID, domain.Deposit{
		ID:             depositID,
		Amount:         in.Amount,
		Provider:       u.provider.Name(),
		ChargeCurrency: chargeCurrency,
		ChargeAmount:   chargeAmount,
		FxRate:         fxRate,
		ExpiresAt:      &expiresAt,
	})
	if err != nil {
		return DepositIntentResult{}, err
	}

	checkout, err := u.provider.CreateCheckout(ctx, stripe.CheckoutInput{
		DepositID:      dep.ID.String(),
		WalletID:       dep.WalletID.String(),
		UserID:         in.UserID.String(),
		CreditsAmount:  dep.Amount,
		ChargeCurrency: dep.ChargeCurrency,
		ChargeAmount:   dep.ChargeAmount,
		SuccessURL:     in.SuccessURL,
		CancelURL:      in.CancelURL,
		ExpiresAt:      expiresAt,
		IdempotencyKey: "checkout:" + in.IdempotencyKey,
	})
	if err != nil {
		// Deposit stays PENDING without provider_ref; reconciler expires orphan PENDING rows.
		return DepositIntentResult{}, err
	}

	dep, err = u.dStore.AttachProviderRef(ctx, dep.ID, checkout.ProviderRef)
	if err != nil {
		return DepositIntentResult{}, err
	}

	return DepositIntentResult{
		Deposit:     dep,
		CheckoutURL: checkout.CheckoutURL,
	}, nil
}

func (u *WalletUsecase) ConfirmDeposit(ctx context.Context, headers http.Header, rawBody []byte) (postgres.DepositConfirmOutcome, error) {
	evt, err := u.provider.ParseWebhook(headers, rawBody)
	if err != nil {
		return postgres.DepositConfirmOutcome{}, err
	}

	var isPaid bool
	var markTerminal *domain.DepositStatus

	switch evt.Type {
	case "checkout.session.completed":
		if evt.PaymentStatus != "paid" {
			// Async payment method still pending; wait for async_payment_succeeded/failed.
			return postgres.DepositConfirmOutcome{}, nil
		}
		isPaid = true
	case "checkout.session.async_payment_succeeded":
		isPaid = true
	case "checkout.session.async_payment_failed":
		st := domain.DepositFailed
		markTerminal = &st
	case "checkout.session.expired":
		st := domain.DepositExpired
		markTerminal = &st
	default:
		// Ignore unknown event types so Stripe stops retrying.
		return postgres.DepositConfirmOutcome{}, nil
	}

	outcome, err := u.dStore.ConfirmDepositWebhook(
		ctx,
		u.provider.Name(),
		evt.EventID,
		evt.Type,
		evt.ProviderRef,
		evt.PaymentIntent,
		evt.Currency,
		evt.AmountTotal,
		isPaid,
		markTerminal,
	)
	if err != nil {
		return postgres.DepositConfirmOutcome{}, err
	}

	if outcome.AmountMismatch {
		u.logger.Error("stripe deposit paid amount or currency mismatch; manual review required",
			slog.String("deposit_id", outcome.Deposit.ID.String()),
			slog.String("provider_ref", evt.ProviderRef),
			slog.Int64("expected_charge_amount", outcome.Deposit.ChargeAmount),
			slog.Int64("paid_amount", evt.AmountTotal),
			slog.String("expected_currency", outcome.Deposit.ChargeCurrency),
			slog.String("paid_currency", evt.Currency),
		)
	}
	if outcome.LateAfterExpiry {
		u.logger.Warn("deposit succeeded after expiry; credited and flagged for review",
			slog.String("deposit_id", outcome.Deposit.ID.String()),
			slog.String("provider_ref", evt.ProviderRef),
		)
	}

	return outcome, nil
}

func (u *WalletUsecase) GetDeposit(ctx context.Context, userID, depositID uuid.UUID) (domain.Deposit, error) {
	return u.dStore.GetDepositForUser(ctx, userID, depositID)
}

func (u *WalletUsecase) RequestWithdrawal(
	ctx context.Context,
	userID uuid.UUID,
	amount int64,
	destination json.RawMessage,
) (domain.Withdrawal, error) {
	if amount <= 0 {
		return domain.Withdrawal{}, domain.NewBadAmountError("amount", "must be positive")
	}
	if u.cfg.MinWithdrawal > 0 && amount < u.cfg.MinWithdrawal {
		return domain.Withdrawal{}, domain.NewBadAmountError("amount", fmt.Sprintf("minimum withdrawal is %d", u.cfg.MinWithdrawal))
	}
	if len(destination) == 0 || !json.Valid(destination) {
		return domain.Withdrawal{}, domain.NewValidationError("destination", "valid JSON destination details are required")
	}

	return u.wStore.RequestWithdrawal(ctx, userID, uuid.New(), amount, destination)
}

func (u *WalletUsecase) CancelWithdrawal(ctx context.Context, userID, withdrawalID uuid.UUID) (domain.Withdrawal, error) {
	return u.wStore.CancelWithdrawal(ctx, userID, withdrawalID)
}

func (u *WalletUsecase) ListWithdrawals(ctx context.Context, userID uuid.UUID, limit int) ([]domain.Withdrawal, error) {
	return u.wStore.ListWithdrawalsByUser(ctx, userID, limit)
}

func (u *WalletUsecase) computeCharge(creditsAmount int64) (string, int64, *string, error) {
	cur := strings.ToLower(strings.TrimSpace(u.cfg.ChargeCurrency))
	if cur == "lkr" {
		return "lkr", creditsAmount, nil, nil
	}
	if cur != "usd" {
		return "", 0, nil, domain.NewValidationError("charge_currency", "must be lkr or usd")
	}
	if u.cfg.CreditsPerUSD <= 0 {
		return "", 0, nil, fmt.Errorf("%w: CREDITS_PER_USD must be configured when charging in usd", domain.ErrUnavailable)
	}

	// Both creditsAmount and USD cents are 1/100 minor units:
	// charge_cents = ceil(credits_minor / CREDITS_PER_USD)
	chargeCents := (creditsAmount + u.cfg.CreditsPerUSD - 1) / u.cfg.CreditsPerUSD
	fxStr := fmt.Sprintf("%.8f", float64(u.cfg.CreditsPerUSD))
	return "usd", chargeCents, &fxStr, nil
}
