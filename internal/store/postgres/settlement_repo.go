package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"billing-wallet-service/internal/domain"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const settlementColumns = `trade_id, journal_id, buyer_wallet_id, seller_wallet_id, buy_order_id, sell_order_id,
	qty_wh, price_per_kwh, trade_value, buyer_fee, seller_fee, buyer_fee_bps, seller_fee_bps,
	buyer_fee_config_id, seller_fee_config_id, settled_at`

// settlementMetadata is kept on the settlement's journal, so a replay can return
// the original response.
type settlementMetadata struct {
	BuyerReleasedRemainder int64 `json:"buyer_released_remainder"`
	BuyOrderFinal          bool  `json:"buy_order_final"`
}

type SettlementRepo struct {
	pool *pgxpool.Pool
}

func NewSettlementRepo(pool *pgxpool.Pool) *SettlementRepo {
	return &SettlementRepo{pool: pool}
}

// SettleTrade settles one fill in a single transaction, retried on contention. Both
// wallets are locked in wallet_id order, then the buyer's hold, so concurrent
// settlements, reservations and releases cannot deadlock.
//
// The seller's rate is read before the transaction starts. Reading it inside would
// need a second connection while the transaction holds one, and enough concurrent
// settlements would then exhaust the pool and wait on each other forever.
func (r *SettlementRepo) SettleTrade(ctx context.Context, request domain.SettleRequest, fees domain.FeeRates) (domain.SettlementResult, error) {
	// A retry of a settled trade returns the original result without needing a rate,
	// which may not exist at its executed_at any more.
	if replay, found, err := r.findSettledReplay(ctx, request); found || err != nil {
		return replay, err
	}

	// The seller pays the rate in effect when the trade executed.
	sellerFee, err := fees.ActiveFeeConfig(ctx, request.ExecutedAt)
	if err != nil {
		return domain.SettlementResult{}, fmt.Errorf("load seller fee config: %w", err)
	}

	var result domain.SettlementResult

	err = inTx(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		result, err = settle(ctx, tx, request, sellerFee)
		return err
	})
	if err != nil {
		return domain.SettlementResult{}, err
	}

	return result, nil
}

// findSettledReplay is the replay check before any locks. It cannot see a settlement
// still in progress, so settle checks again once it holds the locks.
func (r *SettlementRepo) findSettledReplay(ctx context.Context, request domain.SettleRequest) (domain.SettlementResult, bool, error) {
	var buyerWalletID, sellerWalletID *uuid.UUID
	err := r.pool.QueryRow(ctx,
		`SELECT (SELECT id FROM wallets WHERE user_id = $1), (SELECT id FROM wallets WHERE user_id = $2)`,
		request.BuyerUserID, request.SellerUserID).Scan(&buyerWalletID, &sellerWalletID)
	if err != nil {
		return domain.SettlementResult{}, false, fmt.Errorf("find wallets: %w", mapError(err))
	}
	// Without both wallets this trade cannot have been settled.
	if buyerWalletID == nil || sellerWalletID == nil {
		return domain.SettlementResult{}, false, nil
	}

	return findReplay(ctx, r.pool, request, *buyerWalletID, *sellerWalletID)
}

func settle(ctx context.Context, tx pgx.Tx, request domain.SettleRequest, sellerFee domain.FeeConfig) (domain.SettlementResult, error) {
	buyer, err := scanWallet(tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE user_id = $1`, request.BuyerUserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SettlementResult{}, domain.ErrHoldNotFound
	}
	if err != nil {
		return domain.SettlementResult{}, fmt.Errorf("find buyer wallet: %w", mapError(err))
	}

	seller, err := ensureWallet(ctx, tx, request.SellerUserID)
	if err != nil {
		return domain.SettlementResult{}, err
	}

	locked := map[uuid.UUID]domain.Wallet{}
	ids := []uuid.UUID{buyer.ID, seller.ID}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	for _, id := range ids {
		if locked[id], err = lockWallet(ctx, tx, id); err != nil {
			return domain.SettlementResult{}, err
		}
	}
	buyer, seller = locked[buyer.ID], locked[seller.ID]

	// Another settlement of this trade may have committed while this one waited.
	if replay, found, err := findReplay(ctx, tx, request, buyer.ID, seller.ID); found || err != nil {
		return replay, err
	}

	hold, err := findOrderHold(ctx, tx, request.BuyOrderID, "FOR UPDATE")
	if err != nil {
		return domain.SettlementResult{}, err
	}
	if hold.WalletID != buyer.ID {
		return domain.SettlementResult{}, fmt.Errorf("buy order %s is not the buyer's: %w", request.BuyOrderID, domain.ErrHoldNotFound)
	}

	plan, err := hold.PlanSettlement(request, sellerFee.SellerFeeBps)
	if err != nil {
		return domain.SettlementResult{}, err
	}

	buyer, err = scanWallet(tx.QueryRow(ctx,
		`UPDATE wallets
		 SET reserved = reserved - $1 - $2, available = available + $2, version = version + 1
		 WHERE id = $3
		 RETURNING `+walletColumns, plan.Split.BuyerDebit, plan.Released, buyer.ID))
	if err != nil {
		return domain.SettlementResult{}, fmt.Errorf("debit buyer: %w", mapError(err))
	}

	seller, err = scanWallet(tx.QueryRow(ctx,
		`UPDATE wallets
		 SET available = available + $1, version = version + 1
		 WHERE id = $2
		 RETURNING `+walletColumns, plan.Split.SellerCredit, seller.ID))
	if err != nil {
		return domain.SettlementResult{}, fmt.Errorf("credit seller: %w", mapError(err))
	}

	hold, err = scanHold(tx.QueryRow(ctx,
		`UPDATE holds
		 SET amount_consumed = amount_consumed + $1, amount_released = amount_released + $2, status = $3
		 WHERE id = $4
		 RETURNING `+holdColumns, plan.Split.BuyerDebit, plan.Released, plan.HoldStatus, hold.ID))
	if err != nil {
		return domain.SettlementResult{}, fmt.Errorf("update hold: %w", mapError(err))
	}

	journalID, err := postSettlementJournal(ctx, tx, request, plan, buyer, seller)
	if err != nil {
		return domain.SettlementResult{}, err
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO trade_settlements (`+settlementColumns+`)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, now())`,
		request.TradeID, journalID, buyer.ID, seller.ID, request.BuyOrderID, request.SellOrderID,
		request.QtyWh, request.PricePerKwh, plan.Split.Value, plan.Split.BuyerFee, plan.Split.SellerFee,
		*hold.BuyerFeeBps, sellerFee.SellerFeeBps, *hold.BuyerFeeConfigID, sellerFee.ID)
	if err != nil {
		return domain.SettlementResult{}, fmt.Errorf("insert settlement: %w", raceToRetry(err))
	}

	if err := publishSettlement(ctx, tx, request, plan, journalID, buyer, seller, hold); err != nil {
		return domain.SettlementResult{}, err
	}

	return domain.SettlementResult{
		SettlementID:           journalID,
		TradeValue:             plan.Split.Value,
		BuyerFee:               plan.Split.BuyerFee,
		SellerFee:              plan.Split.SellerFee,
		BuyerReleasedRemainder: plan.Released,
	}, nil
}

// findReplay returns the stored result if this trade is already settled, or a
// conflict if it was settled with different details.
func findReplay(ctx context.Context, db querier, request domain.SettleRequest, buyerWalletID, sellerWalletID uuid.UUID) (domain.SettlementResult, bool, error) {
	var settled domain.TradeSettlement
	var metadata []byte
	err := db.QueryRow(ctx,
		`SELECT s.trade_id, s.journal_id, s.buyer_wallet_id, s.seller_wallet_id, s.buy_order_id, s.sell_order_id,
		        s.qty_wh, s.price_per_kwh, s.trade_value, s.buyer_fee, s.seller_fee, j.metadata
		 FROM trade_settlements s JOIN journals j ON j.id = s.journal_id
		 WHERE s.trade_id = $1`, request.TradeID).
		Scan(&settled.TradeID, &settled.JournalID, &settled.BuyerWalletID, &settled.SellerWalletID,
			&settled.BuyOrderID, &settled.SellOrderID, &settled.QtyWh, &settled.PricePerKwh,
			&settled.TradeValue, &settled.BuyerFee, &settled.SellerFee, &metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SettlementResult{}, false, nil
	}
	if err != nil {
		return domain.SettlementResult{}, false, fmt.Errorf("find settlement: %w", mapError(err))
	}

	var stored settlementMetadata
	if err := json.Unmarshal(metadata, &stored); err != nil {
		return domain.SettlementResult{}, true, fmt.Errorf("decode settlement metadata: %w", err)
	}

	if !settled.MatchesRequest(request, stored.BuyOrderFinal, buyerWalletID, sellerWalletID) {
		return domain.SettlementResult{}, true, fmt.Errorf("trade %s: %w", request.TradeID, domain.ErrIdempotencyConflict)
	}

	return domain.SettlementResult{
		SettlementID:           settled.JournalID,
		TradeValue:             settled.TradeValue,
		BuyerFee:               settled.BuyerFee,
		SellerFee:              settled.SellerFee,
		BuyerReleasedRemainder: stored.BuyerReleasedRemainder,
		AlreadySettled:         true,
	}, true, nil
}

// querier is a pool or a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// postSettlementJournal writes the journal and its lines, which must sum to zero.
func postSettlementJournal(ctx context.Context, tx pgx.Tx, request domain.SettleRequest, plan domain.SettlementPlan, buyer, seller domain.Wallet) (uuid.UUID, error) {
	accounts := map[uuid.UUID]uuid.UUID{}
	for _, wallet := range []domain.Wallet{buyer, seller} {
		var accountID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE wallet_id = $1`, wallet.ID).Scan(&accountID)
		if err != nil {
			return uuid.Nil, fmt.Errorf("find ledger account for wallet %s: %w", wallet.ID, mapError(err))
		}
		accounts[wallet.ID] = accountID
	}

	var revenueID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE type = $1 AND wallet_id IS NULL`, domain.AccountPlatformRevenue).Scan(&revenueID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("find platform revenue account: %w", mapError(err))
	}

	lines := domain.SettlementLines(accounts[buyer.ID], accounts[seller.ID], revenueID, plan.Split)
	if err := domain.ValidateBalanced(lines); err != nil {
		return uuid.Nil, err
	}

	metadata, err := json.Marshal(settlementMetadata{BuyerReleasedRemainder: plan.Released, BuyOrderFinal: request.BuyOrderFinal})
	if err != nil {
		return uuid.Nil, fmt.Errorf("encode settlement metadata: %w", err)
	}

	var journalID uuid.UUID
	err = tx.QueryRow(ctx,
		`INSERT INTO journals (type, idempotency_key, ref_type, ref_id, metadata)
		 VALUES ($1, $2, 'trade', $3, $4)
		 RETURNING id`,
		domain.JournalTradeSettlement, "settle:"+request.TradeID.String(), request.TradeID, metadata).Scan(&journalID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert journal: %w", raceToRetry(err))
	}

	totals := map[uuid.UUID]int64{accounts[buyer.ID]: buyer.Total(), accounts[seller.ID]: seller.Total()}
	for _, line := range lines {
		var balanceAfter *int64
		if total, ok := totals[line.AccountID]; ok {
			balanceAfter = &total
		}

		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_lines (journal_id, account_id, amount, balance_after) VALUES ($1, $2, $3, $4)`,
			journalID, line.AccountID, line.Amount, balanceAfter)
		if err != nil {
			return uuid.Nil, fmt.Errorf("insert ledger line: %w", mapError(err))
		}
	}

	return journalID, nil
}

func publishSettlement(ctx context.Context, tx pgx.Tx, request domain.SettleRequest, plan domain.SettlementPlan, journalID uuid.UUID, buyer, seller domain.Wallet, hold domain.Hold) error {
	err := insertTradeSettled(ctx, tx, tradeSettledEvent{
		TradeID:        request.TradeID.String(),
		SettlementID:   journalID.String(),
		BuyOrderID:     request.BuyOrderID.String(),
		SellOrderID:    request.SellOrderID.String(),
		BuyerUserID:    request.BuyerUserID.String(),
		SellerUserID:   request.SellerUserID.String(),
		QtyWh:          request.QtyWh,
		PricePerKwh:    request.PricePerKwh,
		TradeValue:     plan.Split.Value,
		BuyerFee:       plan.Split.BuyerFee,
		SellerFee:      plan.Split.SellerFee,
		BuyerDebit:     plan.Split.BuyerDebit,
		SellerCredit:   plan.Split.SellerCredit,
		PlatformCredit: plan.Split.PlatformCredit,
		ExecutedAt:     request.ExecutedAt,
	})
	if err != nil {
		return err
	}

	if err := insertBalanceChanged(ctx, tx, buyer, balanceChange{
		OrderID: request.BuyOrderID, HoldID: hold.ID, TradeID: request.TradeID, Reason: domain.BalanceReasonTradeSettled,
	}); err != nil {
		return err
	}

	return insertBalanceChanged(ctx, tx, seller, balanceChange{
		OrderID: request.SellOrderID, TradeID: request.TradeID, Reason: domain.BalanceReasonTradeSettled,
	})
}

// raceToRetry turns a duplicate key on the settlement's natural keys into a retry.
// It means a concurrent settlement of the same trade won, and the retry will find
// it and replay or report a conflict.
func raceToRetry(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: %w", domain.ErrRetryableContention, err)
	}

	return mapError(err)
}
