-- +goose Up
CREATE TABLE wallets (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL UNIQUE, -- opaque ref to auth-service, no cross-database FK
    available  BIGINT NOT NULL DEFAULT 0 CHECK (available >= 0),
    reserved   BIGINT NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    status     TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'FROZEN', 'CLOSED')),
    version    BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER set_wallets_updated_at
    BEFORE UPDATE ON wallets
    FOR EACH ROW
    EXECUTE FUNCTION update_timestamp();

CREATE TABLE holds (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    wallet_id           UUID NOT NULL REFERENCES wallets (id),
    kind                TEXT NOT NULL CHECK (kind IN ('ORDER', 'WITHDRAWAL')),
    ref_id              UUID NOT NULL, -- order_id or withdrawal_id
    -- order holds only (NULL for withdrawals):
    order_type          TEXT CHECK (order_type IN ('LIMIT', 'MARKET')),
    qty_wh              BIGINT,
    max_price_per_kwh   BIGINT, -- limit price, or price collar for market orders
    buyer_fee_bps       INT,    -- buyer rate locked when the hold was created
    buyer_fee_config_id UUID,
    amount_total        BIGINT NOT NULL CHECK (amount_total > 0),
    amount_consumed     BIGINT NOT NULL DEFAULT 0 CHECK (amount_consumed >= 0),
    amount_released     BIGINT NOT NULL DEFAULT 0 CHECK (amount_released >= 0),
    status              TEXT NOT NULL CHECK (status IN ('ACTIVE', 'CONSUMED', 'RELEASED', 'EXPIRED')),
    expires_at          TIMESTAMPTZ, -- safety net only
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (kind, ref_id), -- makes ReserveCredits idempotent
    CHECK (amount_consumed + amount_released <= amount_total),
    CONSTRAINT holds_order_fields_match_kind CHECK (
        (
            kind = 'ORDER'
            AND order_type IS NOT NULL
            AND qty_wh IS NOT NULL
            AND max_price_per_kwh IS NOT NULL
            AND buyer_fee_bps IS NOT NULL
            AND buyer_fee_config_id IS NOT NULL
        )
        OR (
            kind = 'WITHDRAWAL'
            AND order_type IS NULL
            AND qty_wh IS NULL
            AND max_price_per_kwh IS NULL
            AND buyer_fee_bps IS NULL
            AND buyer_fee_config_id IS NULL
        )
    )
);

CREATE INDEX holds_expiry_idx ON holds (expires_at) WHERE status = 'ACTIVE';

CREATE TRIGGER set_holds_updated_at
    BEFORE UPDATE ON holds
    FOR EACH ROW
    EXECUTE FUNCTION update_timestamp();

-- +goose Down
DROP TABLE IF EXISTS holds;
DROP TABLE IF EXISTS wallets;
