-- +goose Up
-- Training from a recipe (PLAN.md M12, cut c). A fine-tune job can start
-- from a recipe instead of one dataset: the job merges the recipe's
-- datasets under its spec into a new dataset (kind 'merged'), trains on
-- that, and the adapter keeps the recipe's system prompt and rubric.

-- The merged dataset a recipe job builds is a datasets row like any other,
-- so download, preview and the eval gate work on it unchanged.
ALTER TABLE datasets DROP CONSTRAINT datasets_kind_check;
ALTER TABLE datasets ADD CONSTRAINT datasets_kind_check CHECK (kind IN ('chat', 'uploaded', 'merged'));

-- spec is the recipe's spec as it was when the job started, so editing the
-- recipe later does not change a job that is queued or running.
ALTER TABLE finetune_jobs
    ADD COLUMN recipe_id uuid REFERENCES training_recipes(id) ON DELETE SET NULL,
    ADD COLUMN spec      jsonb NOT NULL DEFAULT '{}';

-- The adapter carries what it was trained under: its endpoint serves with
-- the system prompt by default and the eval gate judges with the rubric.
ALTER TABLE adapters
    ADD COLUMN recipe_id     uuid REFERENCES training_recipes(id) ON DELETE SET NULL,
    ADD COLUMN system_prompt text NOT NULL DEFAULT '',
    ADD COLUMN rubric        text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE adapters DROP COLUMN rubric, DROP COLUMN system_prompt, DROP COLUMN recipe_id;
ALTER TABLE finetune_jobs DROP COLUMN spec, DROP COLUMN recipe_id;
UPDATE finetune_jobs SET dataset_id = NULL WHERE dataset_id IN (SELECT id FROM datasets WHERE kind = 'merged');
DELETE FROM datasets WHERE kind = 'merged';
ALTER TABLE datasets DROP CONSTRAINT datasets_kind_check;
ALTER TABLE datasets ADD CONSTRAINT datasets_kind_check CHECK (kind IN ('chat', 'uploaded'));
