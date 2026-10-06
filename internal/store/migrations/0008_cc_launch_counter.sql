-- +goose Up
-- Weekly count of Claude Code launches on the subscription lane
-- (docs/CLAUDE_CODE_JOBS.md §6.4, spec M6). Claude's own usage is never
-- read, so the pool is approximated by counting launches ws knows of: a
-- row per week (Monday 00:00 UTC), incremented when a new cc_jobs row is
-- created, whoever reported it. The router reads it to keep low-value work
-- off the subscription as the week's soft cap (WS_CC_WEEKLY_CAP) fills.
CREATE TABLE cc_launch_counter (
    week        timestamptz PRIMARY KEY,
    count       integer NOT NULL DEFAULT 0,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE cc_launch_counter;
