-- +goose Up
-- Per-endpoint request body overrides, merged into every OpenAI-compatible
-- request for that endpoint. Used to pin an OpenRouter model to one upstream
-- provider ({"provider": {"order": ["cerebras"], "allow_fallbacks": false}}),
-- or to pass engine-specific knobs to a local server.
ALTER TABLE endpoints ADD COLUMN extra_body jsonb NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE endpoints DROP COLUMN extra_body;
