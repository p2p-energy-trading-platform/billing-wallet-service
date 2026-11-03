-- +goose Up
CREATE TABLE ledger_accounts (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type      TEXT NOT NULL CHECK (type IN ('USER_WALLET', 'PLATFORM_REVENUE', 'PAYMENT_CLEARING')),
    wallet_id UUID UNIQUE REFERENCES wallets (id), -- only for USER_WALLET
    name      TEXT NOT NULL,
    CONSTRAINT ledger_accounts_wallet_matches_type CHECK ((type = 'USER_WALLET') = (wallet_id IS NOT NULL))
);

-- wallet_id UNIQUE does not stop duplicates here because NULLs never conflict.
CREATE UNIQUE INDEX ledger_accounts_one_per_system_type_idx ON ledger_accounts (type) WHERE wallet_id IS NULL;

INSERT INTO ledger_accounts (type, name) VALUES
    ('PLATFORM_REVENUE', 'Platform revenue'),
    ('PAYMENT_CLEARING', 'Payment clearing');

CREATE TABLE journals (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type            TEXT NOT NULL CHECK (type IN ('DEPOSIT', 'WITHDRAWAL', 'TRADE_SETTLEMENT', 'ADJUSTMENT')),
    idempotency_key TEXT NOT NULL UNIQUE, -- Ex: 'settle:<trade_id>'
    ref_type        TEXT NOT NULL,
    ref_id          UUID NOT NULL,
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ledger_lines (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    journal_id    UUID NOT NULL REFERENCES journals (id),
    account_id    UUID NOT NULL REFERENCES ledger_accounts (id),
    amount        BIGINT NOT NULL CHECK (amount <> 0), -- signed: + credit to account, - debit
    balance_after BIGINT,                              -- for user wallets: total after line
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ledger_lines_account_idx ON ledger_lines (account_id, id DESC);

-- Corrections are new ADJUSTMENT journals, never edits. This uses triggers rather
-- than REVOKE because the service role owns these tables: foreign key checks take
-- FOR KEY SHARE on journals as the owner, which needs UPDATE privilege, so revoking
-- it would refuse every row that references a journal.
-- +goose StatementBegin
CREATE FUNCTION prevent_ledger_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed: the ledger is append-only', TG_OP, TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER journals_append_only
    BEFORE UPDATE OR DELETE OR TRUNCATE ON journals
    FOR EACH STATEMENT
    EXECUTE FUNCTION prevent_ledger_mutation();

CREATE TRIGGER ledger_lines_append_only
    BEFORE UPDATE OR DELETE OR TRUNCATE ON ledger_lines
    FOR EACH STATEMENT
    EXECUTE FUNCTION prevent_ledger_mutation();

-- +goose Down
DROP TABLE IF EXISTS ledger_lines;
DROP TABLE IF EXISTS journals;
DROP TABLE IF EXISTS ledger_accounts;
DROP FUNCTION IF EXISTS prevent_ledger_mutation();
