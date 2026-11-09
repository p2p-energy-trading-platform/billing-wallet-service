-- +goose Up
-- SetFeeConfig retries return the row their key already created.
ALTER TABLE fee_configs ADD COLUMN idempotency_key TEXT UNIQUE;

-- Rate changes are new rows, because settlements and holds reference old ones.
-- +goose StatementBegin
CREATE FUNCTION prevent_fee_config_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% on fee_configs is not allowed: fee configs are append-only', TG_OP;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER fee_configs_append_only
    BEFORE UPDATE OR DELETE OR TRUNCATE ON fee_configs
    FOR EACH STATEMENT
    EXECUTE FUNCTION prevent_fee_config_mutation();

-- +goose Down
DROP TRIGGER IF EXISTS fee_configs_append_only ON fee_configs;
DROP FUNCTION IF EXISTS prevent_fee_config_mutation();
ALTER TABLE fee_configs DROP COLUMN IF EXISTS idempotency_key;
