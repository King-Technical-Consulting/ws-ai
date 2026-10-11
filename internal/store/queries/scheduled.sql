-- name: CreateScheduledPrompt :one
INSERT INTO scheduled_prompts (user_id, project_id, title, prompt, model_selector, kind, cron_expr, timezone, run_at,
  enabled, allow_web_fetch, notify_email, reuse_conversation, next_run_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING *;

-- name: GetScheduledPrompt :one
SELECT * FROM scheduled_prompts WHERE id = $1 AND user_id = $2;

-- name: GetScheduledPromptByID :one
SELECT * FROM scheduled_prompts WHERE id = $1;

-- name: ListScheduledPrompts :many
SELECT * FROM scheduled_prompts WHERE user_id = $1 ORDER BY created_at DESC;

-- name: CountScheduledPrompts :one
SELECT count(*) FROM scheduled_prompts WHERE user_id = $1;

-- name: UpdateScheduledPrompt :one
UPDATE scheduled_prompts SET project_id = $3, title = $4, prompt = $5, model_selector = $6, kind = $7, cron_expr = $8,
  timezone = $9, run_at = $10, enabled = $11, allow_web_fetch = $12, notify_email = $13, reuse_conversation = $14,
  next_run_at = $15, updated_at = now()
WHERE id = $1 AND user_id = $2 RETURNING *;

-- name: DeleteScheduledPrompt :exec
DELETE FROM scheduled_prompts WHERE id = $1 AND user_id = $2;

-- Due rows of members who can still sign in.
-- name: ListDueScheduledPrompts :many
SELECT s.* FROM scheduled_prompts s JOIN users u ON u.id = s.user_id
WHERE s.enabled AND s.next_run_at IS NOT NULL AND s.next_run_at <= sqlc.arg(now)::timestamptz AND u.disabled_at IS NULL
ORDER BY s.next_run_at LIMIT 50;

-- The claim: whoever moves next_run_at first fires the run. A row that is
-- no longer due (another worker, an edit) returns no row.
-- name: ClaimScheduledPrompt :one
UPDATE scheduled_prompts SET next_run_at = sqlc.narg(next_run_at)::timestamptz, last_run_at = sqlc.arg(now)::timestamptz, last_status = 'queued', last_error = ''
WHERE id = sqlc.arg(id) AND enabled AND next_run_at IS NOT NULL AND next_run_at <= sqlc.arg(now)::timestamptz
RETURNING *;

-- name: SetScheduledPromptResult :exec
UPDATE scheduled_prompts SET last_status = $2, last_error = $3, conversation_id = COALESCE($4, conversation_id) WHERE id = $1;

-- name: InsertScheduledPromptRun :one
INSERT INTO scheduled_prompt_runs (prompt_id, run_id, conversation_id, status, error, ended_at)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: ListScheduledPromptRuns :many
SELECT * FROM scheduled_prompt_runs WHERE prompt_id = $1 ORDER BY started_at DESC LIMIT $2;

-- Runs that have not ended, with the run's own status.
-- name: ListOpenScheduledPromptRuns :many
SELECT r.id, r.prompt_id, r.run_id, r.conversation_id, r.status, r.notified, r.started_at,
       a.status AS run_status, COALESCE(a.error, '') AS run_error
FROM scheduled_prompt_runs r LEFT JOIN agent_runs a ON a.id = r.run_id
WHERE r.ended_at IS NULL ORDER BY r.started_at LIMIT 200;

-- name: FinishScheduledPromptRun :exec
UPDATE scheduled_prompt_runs SET status = $2, error = $3, ended_at = now(), notified = $4 WHERE id = $1;

-- name: SetScheduledPromptRunStatus :exec
UPDATE scheduled_prompt_runs SET status = $2 WHERE id = $1;
