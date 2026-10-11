-- +goose Up
-- Scheduled prompts (PLAN.md M13, cut 5): a prompt that runs on its own,
-- once at a time or on a cron expression, as an ordinary chat run in a
-- conversation. Not an agent: no goal, no triggers, no tools unless the
-- member allows web_fetch. The worker's minute tick claims due rows.
CREATE TABLE scheduled_prompts (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id         uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title              text NOT NULL DEFAULT '',
    prompt             text NOT NULL,
    model_selector     text NOT NULL DEFAULT 'auto',
    kind               text NOT NULL CHECK (kind IN ('once','cron')),
    cron_expr          text NOT NULL DEFAULT '',            -- 5 fields, evaluated in timezone
    timezone           text NOT NULL DEFAULT 'UTC',         -- IANA name
    run_at             timestamptz,                         -- kind once: when
    enabled            boolean NOT NULL DEFAULT true,
    allow_web_fetch    boolean NOT NULL DEFAULT false,
    notify_email       boolean NOT NULL DEFAULT false,
    reuse_conversation boolean NOT NULL DEFAULT false,      -- keep adding to one conversation
    conversation_id    uuid REFERENCES conversations(id) ON DELETE SET NULL,
    next_run_at        timestamptz,                         -- NULL: nothing left to run
    last_run_at        timestamptz,
    last_status        text NOT NULL DEFAULT '',            -- queued, running, done, failed, skipped
    last_error         text NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX scheduled_prompts_user_idx ON scheduled_prompts(user_id, created_at DESC);
CREATE INDEX scheduled_prompts_due_idx ON scheduled_prompts(next_run_at) WHERE enabled AND next_run_at IS NOT NULL;

CREATE TABLE scheduled_prompt_runs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    prompt_id       uuid NOT NULL REFERENCES scheduled_prompts(id) ON DELETE CASCADE,
    run_id          uuid REFERENCES agent_runs(id) ON DELETE SET NULL,
    conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL,
    status          text NOT NULL DEFAULT 'queued',         -- queued, running, done, failed, cancelled, skipped
    error           text NOT NULL DEFAULT '',
    notified        boolean NOT NULL DEFAULT false,
    started_at      timestamptz NOT NULL DEFAULT now(),
    ended_at        timestamptz
);
CREATE INDEX scheduled_prompt_runs_prompt_idx ON scheduled_prompt_runs(prompt_id, started_at DESC);
CREATE INDEX scheduled_prompt_runs_open_idx ON scheduled_prompt_runs(started_at) WHERE ended_at IS NULL;

-- +goose Down
DROP TABLE scheduled_prompt_runs;
DROP TABLE scheduled_prompts;
