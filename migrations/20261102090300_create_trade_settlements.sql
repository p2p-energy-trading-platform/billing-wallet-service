-- +goose Up
CREATE TABLE trade_settlements (
    trade_id             UUID PRIMARY KEY,
    journal_id           UUID NOT NULL REFERENCES journals (id),
    buyer_wallet_id      UUID NOT NULL REFERENCES wallets (id),
    seller_wallet_id     UUID NOT NULL REFERENCES wallets (id),
    buy_order_id         UUID NOT NULL,
    sell_order_id        UUID NOT NULL,
    qty_wh               BIGINT NOT NULL,
    price_per_kwh        BIGINT NOT NULL,
    trade_value          BIGINT NOT NULL,
    buyer_fee            BIGINT NOT NULL,
    seller_fee           BIGINT NOT NULL,
    buyer_fee_bps        INT NOT NULL,
    seller_fee_bps       INT NOT NULL,
    buyer_fee_config_id  UUID NOT NULL,
    seller_fee_config_id UUID NOT NULL,
    settled_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS trade_settlements;
