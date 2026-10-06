-- +goose Up
-- Media jobs (PLAN.md M6): one row per image or video generation, written
-- by the media service (internal/media) and driven by the River job
-- media.generate. Outputs are attachments (blob store); progress and the
-- final state live here so a gallery card survives a reload. The ledger
-- gets a usage_ledger row per job under task_class image or video.
CREATE TABLE media_jobs (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id               uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id            uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    conversation_id       uuid REFERENCES conversations(id) ON DELETE SET NULL,
    kind                  text NOT NULL CHECK (kind IN ('image','video','edit','upscale')),
    -- selector is the model picker's choice (endpoint id or alias/auto);
    -- endpoint_id is the endpoint that served it, set when the job runs.
    selector              text NOT NULL DEFAULT 'auto',
    endpoint_id           text,
    -- inputs: {prompt, size, n, quality, seconds, aspect, source_attachment_id, estimate_usd}
    inputs                jsonb NOT NULL DEFAULT '{}',
    provider_job_id       text,
    status                text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','done','failed','cancelled')),
    progress              real NOT NULL DEFAULT 0,
    output_attachment_ids uuid[] NOT NULL DEFAULT '{}',
    cost_usd              numeric(12,6) NOT NULL DEFAULT 0,
    error                 text,
    created_at            timestamptz NOT NULL DEFAULT now(),
    started_at            timestamptz,
    ended_at              timestamptz,
    updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX media_jobs_project_idx ON media_jobs(project_id, created_at DESC);
CREATE INDEX media_jobs_user_idx ON media_jobs(user_id, created_at DESC);
CREATE INDEX media_jobs_open_idx ON media_jobs(status) WHERE status IN ('queued','running');
CREATE TRIGGER media_jobs_updated_at BEFORE UPDATE ON media_jobs FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE media_jobs;
