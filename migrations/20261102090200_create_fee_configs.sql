-- +goose Up
CREATE TABLE fee_configs (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_fee_bps  INT NOT NULL CHECK (buyer_fee_bps BETWEEN 0 AND 2000),
    seller_fee_bps INT NOT NULL CHECK (seller_fee_bps BETWEEN 0 AND 2000),
    effective_from TIMESTAMPTZ NOT NULL,
    created_by     UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Balanced split default: 1% from each side. Reservations need an active row.
INSERT INTO fee_configs (buyer_fee_bps, seller_fee_bps, effective_from) VALUES (100, 100, now());

-- +goose Down
DROP TABLE IF EXISTS fee_configs;
