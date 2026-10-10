-- Task router decision log (docs/CLAUDE_CODE_JOBS.md §6.4).

-- name: InsertCCRouteDecision :one
INSERT INTO cc_route_decisions (user_id, conversation_id, prompt_sha256, features, lane, rule, reason, target, model, dry_run, job_id, run_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING *;

-- name: ListCCRouteDecisions :many
SELECT * FROM cc_route_decisions ORDER BY created_at DESC LIMIT $1;

-- Job handles for the read-only web tab (§6.5). A report of a job that is
-- already known refreshes its handle fields and liveness; it never moves
-- the job to another user or source.

-- inserted is true when this call created the row (xmax is 0 on a fresh
-- tuple, non-zero on one ON CONFLICT updated), decided in the same
-- statement so two reports of the same new job cannot both count it.
-- name: UpsertCCJob :one
INSERT INTO cc_jobs (id, user_id, target, session, "window", cwd, lane, model, source, status, started_at, seen_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
ON CONFLICT (id) DO UPDATE SET
    target = EXCLUDED.target, session = EXCLUDED.session, "window" = EXCLUDED."window",
    cwd = EXCLUDED.cwd, lane = EXCLUDED.lane, model = COALESCE(EXCLUDED.model, cc_jobs.model),
    status = EXCLUDED.status, seen_at = now(),
    ended_at = CASE WHEN EXCLUDED.status = 'dead' THEN COALESCE(cc_jobs.ended_at, now()) ELSE NULL END,
    updated_at = now()
RETURNING sqlc.embed(cc_jobs), (xmax = 0) AS inserted;

-- name: GetCCJob :one
SELECT * FROM cc_jobs WHERE id = $1;

-- name: ListCCJobs :many
SELECT * FROM cc_jobs ORDER BY started_at DESC LIMIT $1;

-- name: ListCCJobsOpen :many
SELECT * FROM cc_jobs WHERE status IN ('alive','dead','unknown') ORDER BY started_at DESC;

-- MarkCCJobSeen records what a refresh found: the window is listed and its
-- pane is running (alive) or has exited (dead).

-- name: MarkCCJobSeen :exec
UPDATE cc_jobs SET status = $2, seen_at = now(),
    ended_at = CASE WHEN $2 = 'dead' THEN COALESCE(ended_at, now()) ELSE NULL END,
    updated_at = now()
WHERE id = $1;

-- MarkCCJobEnded closes a job: killed (reported by wsj) or gone (a refresh
-- no longer found the window). A job that already ended keeps its status.

-- name: MarkCCJobEnded :exec
UPDATE cc_jobs SET status = $2, ended_at = COALESCE(ended_at, now()), updated_at = now()
WHERE id = $1 AND status NOT IN ('killed','gone');

-- Weekly launch counter (§6.4, spec M6). week is the Monday 00:00 UTC
-- the launch falls in.

-- name: IncrementCCLaunchCounter :one
INSERT INTO cc_launch_counter (week, count) VALUES ($1, 1)
ON CONFLICT (week) DO UPDATE SET count = cc_launch_counter.count + 1, updated_at = now()
RETURNING count;

-- name: GetCCLaunchCount :one
SELECT count FROM cc_launch_counter WHERE week = $1;
