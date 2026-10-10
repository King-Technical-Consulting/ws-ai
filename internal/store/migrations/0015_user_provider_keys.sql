-- +goose Up
-- A person's own API key for a hosted provider (bring your own key). The
-- key is sealed with AES-256-GCM under WS_SECRETS_KEY before it is stored,
-- bound to the user and provider it belongs to, and never leaves the
-- server: the API returns only the last four characters.
CREATE TABLE user_provider_keys (
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider_id text NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    sealed      bytea NOT NULL,
    last4       text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, provider_id)
);

-- +goose Down
DROP TABLE IF EXISTS user_provider_keys;
