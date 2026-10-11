-- +goose Up
-- Training recipes (PLAN.md M12, cut b): a base model, the owner's
-- instructions in plain words, the datasets attached, and the spec a model
-- fleshes out from them (system prompt, mappings, eval rubric,
-- hyperparameters). Nothing trains from a recipe until cut c.
CREATE TABLE training_recipes (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            text NOT NULL,
    base_model      text NOT NULL,
    base_endpoint_id text,
    instructions    text NOT NULL DEFAULT '',
    -- datasets attached, in order; ids of datasets rows (no foreign key:
    -- an array cannot have one, and a deleted dataset is dropped on read)
    dataset_ids     uuid[] NOT NULL DEFAULT '{}',
    -- where it will run (local, remote, rental; empty takes the default)
    -- and the fine-tune config the owner chose, for cut c
    target          text NOT NULL DEFAULT '',
    config          jsonb NOT NULL DEFAULT '{}',
    -- spec: what training uses (internal/training/recipe.go Spec); the
    -- owner edits it by hand. edited lists the top-level spec keys the
    -- owner changed, which a later flesh-out keeps unless told to replace.
    spec            jsonb NOT NULL DEFAULT '{}',
    edited          text[] NOT NULL DEFAULT '{}',
    -- spec_draft is the last reply the model gave, as parsed, untouched
    -- by edits, so the page can show what the model suggested.
    spec_draft      jsonb NOT NULL DEFAULT '{}',
    -- the model selector flesh-out routes with ("auto" under task class
    -- training), and what the last run used and said
    spec_model      text NOT NULL DEFAULT 'auto',
    spec_status     text NOT NULL DEFAULT 'none' CHECK (spec_status IN ('none', 'queued', 'running', 'ready', 'failed')),
    spec_error      text,
    spec_used_model text,
    spec_at         timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX training_recipes_created_idx ON training_recipes (created_at DESC);

-- +goose Down
DROP TABLE training_recipes;
