-- +goose Up
-- Training flywheel (PLAN.md M10, first cut): ratings on assistant
-- messages, a per-user consent flag, exported datasets, fine-tune jobs
-- and the adapters they produce.

-- Only conversations of users who opted in are exported.
ALTER TABLE users ADD COLUMN training_consent boolean NOT NULL DEFAULT false;

-- A thumbs up or down on an assistant message, one per user.
CREATE TABLE message_ratings (
    message_id  uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    score       smallint NOT NULL CHECK (score IN (-1, 1)),
    note        text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, user_id)
);
CREATE INDEX message_ratings_user_idx ON message_ratings (user_id, created_at DESC);

-- A dataset is one export: chat-format JSONL in the blob store (a train
-- file and a held-out eval file), built by the training.build job from
-- the filters it was created with.
CREATE TABLE datasets (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name           text NOT NULL,
    task_class     text NOT NULL DEFAULT '',
    -- filters: {modes:[...], models:[...], min_rating, since, holdout_pct, max_examples}
    filters        jsonb NOT NULL DEFAULT '{}',
    status         text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','building','ready','failed')),
    blob_key       text,
    eval_blob_key  text,
    bytes          bigint NOT NULL DEFAULT 0,
    examples       int NOT NULL DEFAULT 0,
    eval_examples  int NOT NULL DEFAULT 0,
    error          text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    built_at       timestamptz
);
CREATE INDEX datasets_created_idx ON datasets (created_at DESC);

-- A fine-tune job runs a dataset through a trainer container and ends in
-- an adapter.
CREATE TABLE finetune_jobs (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    dataset_id       uuid REFERENCES datasets(id) ON DELETE SET NULL,
    base_model       text NOT NULL,
    base_endpoint_id text,
    adapter_name     text NOT NULL,
    -- config: {epochs, learning_rate, rank, alpha, max_seq_len, image}
    config           jsonb NOT NULL DEFAULT '{}',
    status           text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','done','failed','cancelled')),
    progress         real NOT NULL DEFAULT 0,
    log              text NOT NULL DEFAULT '',
    adapter_id       uuid,
    error            text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    started_at       timestamptz,
    ended_at         timestamptz
);
CREATE INDEX finetune_jobs_created_idx ON finetune_jobs (created_at DESC);

-- An adapter (LoRA weights as a tarball in the blob store) on a base
-- model. endpoint_id is the endpoints row registered for it; eval_score
-- and baseline_score come from the eval gate (the held-out set judged
-- for the adapter and for the base route).
CREATE TABLE adapters (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name             text UNIQUE NOT NULL,
    base_model       text NOT NULL,
    base_endpoint_id text,
    finetune_job_id  uuid REFERENCES finetune_jobs(id) ON DELETE SET NULL,
    blob_key         text NOT NULL,
    bytes            bigint NOT NULL DEFAULT 0,
    eval_score       real,
    baseline_score   real,
    eval             jsonb,
    promoted         boolean NOT NULL DEFAULT false,
    endpoint_id      text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    evaluated_at     timestamptz
);

-- +goose Down
DROP TABLE adapters;
DROP TABLE finetune_jobs;
DROP TABLE datasets;
DROP TABLE message_ratings;
ALTER TABLE users DROP COLUMN training_consent;
