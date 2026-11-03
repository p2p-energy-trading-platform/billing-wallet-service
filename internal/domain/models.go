package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// All money fields are int64 minor units (see UnitsPerCredit) and all energy
// quantities are int64 Wh. Prices are minor units per kWh. Nullable columns are
// pointers.

type WalletStatus string

const (
	WalletActive WalletStatus = "ACTIVE"
	WalletFrozen WalletStatus = "FROZEN"
	WalletClosed WalletStatus = "CLOSED"
)

type Wallet struct {
	ID        uuid.UUID
	UserID    uuid.UUID // opaque reference to auth-service, no cross-database FK
	Available int64
	Reserved  int64
	Status    WalletStatus
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Total returns the user's whole balance, spendable and locked.
func (w Wallet) Total() int64 {
	return w.Available + w.Reserved
}

type HoldKind string

const (
	HoldKindOrder      HoldKind = "ORDER"
	HoldKindWithdrawal HoldKind = "WITHDRAWAL"
)

type OrderType string

const (
	OrderTypeLimit  OrderType = "LIMIT"
	OrderTypeMarket OrderType = "MARKET"
)

type HoldStatus string

const (
	HoldActive   HoldStatus = "ACTIVE"
	HoldConsumed HoldStatus = "CONSUMED"
	HoldReleased HoldStatus = "RELEASED"
	HoldExpired  HoldStatus = "EXPIRED"
)

var holdTransitions = map[HoldStatus][]HoldStatus{
	HoldActive: {HoldConsumed, HoldReleased, HoldExpired},
}

// CanTransitionTo reports whether the state machine allows moving to next.
func (s HoldStatus) CanTransitionTo(next HoldStatus) bool {
	return slices.Contains(holdTransitions[s], next)
}

// Hold locks part of a wallet's funds against an order or a pending withdrawal.
// The order-only fields are set for ORDER holds and nil for WITHDRAWAL holds.
type Hold struct {
	ID               uuid.UUID
	WalletID         uuid.UUID
	Kind             HoldKind
	RefID            uuid.UUID // order_id or withdrawal_id
	OrderType        *OrderType
	QtyWh            *int64
	MaxPricePerKwh   *int64 // limit price, or price collar for market orders
	BuyerFeeBps      *int32 // buyer rate locked when the hold was created
	BuyerFeeConfigID *uuid.UUID
	AmountTotal      int64
	AmountConsumed   int64
	AmountReleased   int64
	Status           HoldStatus
	ExpiresAt        *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Remaining returns the part of the hold not yet consumed or released.
func (h Hold) Remaining() int64 {
	return h.AmountTotal - h.AmountConsumed - h.AmountReleased
}

type LedgerAccountType string

const (
	AccountUserWallet      LedgerAccountType = "USER_WALLET"
	AccountPlatformRevenue LedgerAccountType = "PLATFORM_REVENUE"
	AccountPaymentClearing LedgerAccountType = "PAYMENT_CLEARING"
)

type LedgerAccount struct {
	ID       uuid.UUID
	Type     LedgerAccountType
	WalletID *uuid.UUID // only for USER_WALLET
	Name     string
}

type JournalType string

const (
	JournalDeposit         JournalType = "DEPOSIT"
	JournalWithdrawal      JournalType = "WITHDRAWAL"
	JournalTradeSettlement JournalType = "TRADE_SETTLEMENT"
	JournalAdjustment      JournalType = "ADJUSTMENT"
)

type Journal struct {
	ID             uuid.UUID
	Type           JournalType
	IdempotencyKey string // for example "settle:<trade_id>"
	RefType        string
	RefID          uuid.UUID
	Metadata       json.RawMessage
	CreatedAt      time.Time
}

type LedgerLine struct {
	ID           int64
	JournalID    uuid.UUID
	AccountID    uuid.UUID
	Amount       int64  // signed: + credit to the account, - debit
	BalanceAfter *int64 // for user wallets: total after this line
	CreatedAt    time.Time
}

// ValidateBalanced checks a journal's lines before commit: there must be at
// least one line, no line may be zero, and the amounts must sum to zero.
func ValidateBalanced(lines []LedgerLine) error {
	if len(lines) == 0 {
		return fmt.Errorf("%w: journal has no lines", ErrUnbalancedJournal)
	}

	var sum int64
	for _, line := range lines {
		if line.Amount == 0 {
			return fmt.Errorf("%w: journal has a zero amount line", ErrUnbalancedJournal)
		}
		sum += line.Amount
	}

	if sum != 0 {
		return fmt.Errorf("%w: lines sum to %d", ErrUnbalancedJournal, sum)
	}

	return nil
}

type TradeSettlement struct {
	TradeID           uuid.UUID
	JournalID         uuid.UUID
	BuyerWalletID     uuid.UUID
	SellerWalletID    uuid.UUID
	BuyOrderID        uuid.UUID
	SellOrderID       uuid.UUID
	QtyWh             int64
	PricePerKwh       int64
	TradeValue        int64
	BuyerFee          int64
	SellerFee         int64
	BuyerFeeBps       int32
	SellerFeeBps      int32
	BuyerFeeConfigID  uuid.UUID
	SellerFeeConfigID uuid.UUID
	SettledAt         time.Time
}

// FeeConfig is append-only: the active row is the latest with
// EffectiveFrom <= now, and old rows are never edited.
type FeeConfig struct {
	ID            uuid.UUID
	BuyerFeeBps   int32
	SellerFeeBps  int32
	EffectiveFrom time.Time
	CreatedBy     *uuid.UUID
	CreatedAt     time.Time
}

type DepositStatus string

const (
	DepositPending   DepositStatus = "PENDING"
	DepositCompleted DepositStatus = "COMPLETED"
	DepositFailed    DepositStatus = "FAILED"
	DepositExpired   DepositStatus = "EXPIRED"
	DepositRefunded  DepositStatus = "REFUNDED"
	DepositDisputed  DepositStatus = "DISPUTED"
)

var depositTransitions = map[DepositStatus][]DepositStatus{
	DepositPending: {DepositCompleted, DepositFailed, DepositExpired},
	// Late success after expiry: the money was received, so it is still credited.
	DepositExpired:   {DepositCompleted},
	DepositCompleted: {DepositRefunded, DepositDisputed},
}

// CanTransitionTo reports whether the state machine allows moving to next.
func (s DepositStatus) CanTransitionTo(next DepositStatus) bool {
	return slices.Contains(depositTransitions[s], next)
}

type Deposit struct {
	ID                 uuid.UUID
	WalletID           uuid.UUID
	Amount             int64 // credits to grant
	Provider           string
	ProviderRef        *string // Stripe Checkout Session id (cs_...)
	ProviderPaymentRef *string // PaymentIntent id (pi_...), filled on completion
	ChargeCurrency     string
	ChargeAmount       int64   // minor units of ChargeCurrency, fixed at intent creation
	FxRate             *string // NUMERIC kept as decimal text so it never passes through a float
	Status             DepositStatus
	ExpiresAt          *time.Time
	JournalID          *uuid.UUID
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ProviderEvent is one processed webhook, used for de-duplication.
type ProviderEvent struct {
	Provider   string
	EventID    string // Stripe evt_...
	Type       string
	ReceivedAt time.Time
}

type WithdrawalStatus string

const (
	WithdrawalPending   WithdrawalStatus = "PENDING"
	WithdrawalApproved  WithdrawalStatus = "APPROVED"
	WithdrawalPaid      WithdrawalStatus = "PAID"
	WithdrawalRejected  WithdrawalStatus = "REJECTED"
	WithdrawalCancelled WithdrawalStatus = "CANCELLED"
	WithdrawalFailed    WithdrawalStatus = "FAILED"
)

var withdrawalTransitions = map[WithdrawalStatus][]WithdrawalStatus{
	WithdrawalPending:  {WithdrawalApproved, WithdrawalRejected, WithdrawalCancelled, WithdrawalFailed},
	WithdrawalApproved: {WithdrawalPaid, WithdrawalFailed},
}

// CanTransitionTo reports whether the state machine allows moving to next.
func (s WithdrawalStatus) CanTransitionTo(next WithdrawalStatus) bool {
	return slices.Contains(withdrawalTransitions[s], next)
}

type Withdrawal struct {
	ID          uuid.UUID
	WalletID    uuid.UUID
	HoldID      uuid.UUID
	Amount      int64
	Destination json.RawMessage // payout details: sensitive, never log
	Status      WithdrawalStatus
	ReviewedBy  *uuid.UUID
	JournalID   *uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// OutboxEvent is a transactional outbox row. A nil PublishedAt means it is
// still waiting for the relay.
type OutboxEvent struct {
	ID          int64
	Topic       string
	Key         string
	Payload     json.RawMessage
	CreatedAt   time.Time
	PublishedAt *time.Time
}
