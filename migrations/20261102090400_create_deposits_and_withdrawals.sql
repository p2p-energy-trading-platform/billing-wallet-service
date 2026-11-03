-- +goose Up
CREATE TABLE deposits (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    wallet_id            UUID NOT NULL REFERENCES wallets (id),
    amount               BIGINT NOT NULL CHECK (amount > 0), -- credits to grant (minor units)
    provider             TEXT NOT NULL DEFAULT 'stripe',
    provider_ref         TEXT UNIQUE,                        -- Stripe Checkout Session id (cs_...)
    provider_payment_ref TEXT,                               -- PaymentIntent id (pi_...), filled on completion
    charge_currency      TEXT NOT NULL,                      -- 'lkr' or 'usd'
    charge_amount        BIGINT NOT NULL,                    -- minor units of charge_currency, fixed at intent creation
    fx_rate              NUMERIC(18, 8),                     -- credits per 1 charge-currency unit; NULL when charging in LKR
    status               TEXT NOT NULL CHECK (status IN ('PENDING', 'COMPLETED', 'FAILED', 'EXPIRED', 'REFUNDED', 'DISPUTED')),
    expires_at           TIMESTAMPTZ,
    journal_id           UUID REFERENCES journals (id),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER set_deposits_updated_at
    BEFORE UPDATE ON deposits
    FOR EACH ROW
    EXECUTE FUNCTION update_timestamp();

-- Webhook de-duplication: one row per provider event processed
CREATE TABLE provider_events (
    provider    TEXT NOT NULL,
    event_id    TEXT NOT NULL, -- Stripe evt_...
    type        TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, event_id)
);

CREATE TABLE withdrawals (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    wallet_id   UUID NOT NULL REFERENCES wallets (id),
    hold_id     UUID NOT NULL REFERENCES holds (id),
    amount      BIGINT NOT NULL CHECK (amount > 0),
    destination JSONB NOT NULL, -- bank account / payout details (encrypt sensitive fields)
    status      TEXT NOT NULL CHECK (status IN ('PENDING', 'APPROVED', 'PAID', 'REJECTED', 'CANCELLED', 'FAILED')),
    reviewed_by UUID,
    journal_id  UUID REFERENCES journals (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER set_withdrawals_updated_at
    BEFORE UPDATE ON withdrawals
    FOR EACH ROW
    EXECUTE FUNCTION update_timestamp();

-- +goose Down
DROP TABLE IF EXISTS withdrawals;
DROP TABLE IF EXISTS provider_events;
DROP TABLE IF EXISTS deposits;
