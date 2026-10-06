-- +goose Up
-- Task router decision log (docs/CLAUDE_CODE_JOBS.md §6.4, spec M3). One
-- row per spawn_job call, dry run or not, so a routing can be explained
-- and the rules tuned later. The prompt itself is never stored: only its
-- sha256 and the features the rules saw. No Claude Code output and no
-- credential can land here; job_id is the tmux handle id, run_id the ws
-- agent run for the non-subscription lanes.
CREATE TABLE cc_route_decisions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid REFERENCES users(id) ON DELETE SET NULL,
    conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL,
    prompt_sha256   text NOT NULL,
    features        jsonb NOT NULL DEFAULT '{}',
    lane            text NOT NULL CHECK (lane IN ('claude-subscription','api','openrouter','local')),
    rule            text NOT NULL,
    reason          text NOT NULL,
    target          text,
    model           text,
    dry_run         boolean NOT NULL DEFAULT false,
    job_id          text,
    run_id          uuid REFERENCES agent_runs(id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cc_route_decisions_created_idx ON cc_route_decisions(created_at DESC);
CREATE INDEX cc_route_decisions_lane_idx ON cc_route_decisions(lane, created_at DESC);

-- +goose Down
DROP TABLE cc_route_decisions;
