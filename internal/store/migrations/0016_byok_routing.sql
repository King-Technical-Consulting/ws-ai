-- +goose Up
-- Strict bring-your-own-key routing. A member's request to a hosted
-- provider uses only that member's own key; the server's shared keys are the
-- owner's, or any user the owner grants them to. Spend on a person's own key
-- is not the owner's money, so the ledger marks it and budgets skip it.
ALTER TABLE users ADD COLUMN shared_provider_keys boolean NOT NULL DEFAULT false;
ALTER TABLE usage_ledger ADD COLUMN own_key boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE usage_ledger DROP COLUMN IF EXISTS own_key;
ALTER TABLE users DROP COLUMN IF EXISTS shared_provider_keys;
