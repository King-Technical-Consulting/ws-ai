-- +goose Up
-- Routing floor (PLAN.md routing work, 2026-10-10): an endpoint's quality
-- tier, and whether general routing may fall back to it. A policy rule's
-- min_tier keeps a fallback from landing on a toy model, and an endpoint
-- that exists for one purpose (a training judge, a LoRA under evaluation)
-- is marked not routable so only a pick by name or a rule that names it
-- reaches it.
ALTER TABLE endpoints
    ADD COLUMN tier text NOT NULL DEFAULT '' CHECK (tier IN ('', 'small', 'medium', 'large')),  -- '' = derived from the model name
    ADD COLUMN routable boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE endpoints DROP COLUMN routable, DROP COLUMN tier;
