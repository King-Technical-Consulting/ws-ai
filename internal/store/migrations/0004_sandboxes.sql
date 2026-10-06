-- +goose Up
-- One coding sandbox per (project, user): a Docker container on the
-- internal sandbox network with a persistent volume at /workspace. The
-- container is disposable (stopped when idle, removed after a week); the
-- volume is the user's working copy and survives both.
CREATE TABLE sandboxes (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    container_id  text,
    volume_name   text NOT NULL,
    image         text NOT NULL,
    runtime       text NOT NULL DEFAULT 'runc',
    status        text NOT NULL DEFAULT 'created' CHECK (status IN ('created','running','stopped','failed')),
    error         text,
    last_used_at  timestamptz NOT NULL DEFAULT now(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, user_id)
);
CREATE INDEX sandboxes_status_idx ON sandboxes(status, last_used_at);

CREATE TABLE sandbox_events (
    id          bigserial PRIMARY KEY,
    sandbox_id  uuid NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    kind        text NOT NULL,          -- created | started | stopped | removed | exec | error | clone
    detail      jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sandbox_events_sandbox_idx ON sandbox_events(sandbox_id, id);

-- +goose Down
DROP TABLE sandbox_events;
DROP TABLE sandboxes;
