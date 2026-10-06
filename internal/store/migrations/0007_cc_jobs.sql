-- +goose Up
-- Claude Code job handles for the read-only web tab (docs/CLAUDE_CODE_JOBS.md
-- §6.5, spec M5). A row is the handle a launcher returned: where a tmux
-- window lives, never what is in it. `wsj run` / `kill` / `clean` report
-- from the Mac (POST / DELETE /api/jobs/cc), the router's spawn_job writes
-- its own launches, and a refresh over the launcher adopts windows it
-- finds on a target that nobody reported. tmux stays authoritative for
-- liveness: `status` is the last thing a refresh saw, `seen_at` when.
-- No column holds a prompt body, Claude Code output or a credential.
CREATE TABLE cc_jobs (
    id          text PRIMARY KEY,                       -- job id, the tmux handle id (window job-<id>)
    user_id     uuid REFERENCES users(id) ON DELETE SET NULL,
    target      text NOT NULL,                          -- targets.toml name on the reporting host
    session     text NOT NULL,                          -- tmux session (subscription)
    "window"    text NOT NULL,                          -- tmux window id (@N)
    cwd         text NOT NULL DEFAULT '',
    lane        text NOT NULL DEFAULT 'claude-subscription',
    model       text,
    source      text NOT NULL CHECK (source IN ('wsj','router','tmux')),
    -- alive: window listed, pane running. dead: window listed, pane exited
    -- (remain-on-exit keeps it). killed: wsj kill/clean reported it. gone:
    -- a refresh no longer found the window. unknown: never refreshed.
    status      text NOT NULL DEFAULT 'unknown' CHECK (status IN ('alive','dead','killed','gone','unknown')),
    started_at  timestamptz NOT NULL DEFAULT now(),
    seen_at     timestamptz,                            -- last refresh that listed the window
    ended_at    timestamptz,                            -- first seen dead, or killed/gone
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cc_jobs_started_idx ON cc_jobs(started_at DESC);
CREATE INDEX cc_jobs_open_idx ON cc_jobs(target) WHERE status IN ('alive','dead','unknown');

-- +goose Down
DROP TABLE cc_jobs;
