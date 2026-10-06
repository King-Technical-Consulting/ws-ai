-- name: CreateRun :one
INSERT INTO agent_runs (agent_id, conversation_id, user_id, trigger_id, status, request, tool_policies, max_steps, first_message_id, owner_pid, started_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, CASE WHEN $5 = 'running' THEN now() ELSE NULL END)
RETURNING *;

-- name: GetRun :one
SELECT * FROM agent_runs WHERE id = $1;

-- name: ListRunsForConversation :many
SELECT * FROM agent_runs WHERE conversation_id = $1 ORDER BY created_at DESC LIMIT $2;

-- name: ListRunsForAgent :many
SELECT * FROM agent_runs WHERE agent_id = $1 ORDER BY created_at DESC LIMIT $2;

-- name: LatestRunForConversation :one
SELECT * FROM agent_runs WHERE conversation_id = $1 ORDER BY created_at DESC LIMIT 1;

-- name: SetRunStatus :exec
UPDATE agent_runs SET status = $2, error = $3,
    ended_at = CASE WHEN $2 IN ('done','failed','cancelled') THEN now() ELSE ended_at END,
    started_at = COALESCE(started_at, CASE WHEN $2 = 'running' THEN now() ELSE NULL END)
WHERE id = $1;

-- name: ClaimRun :one
-- Atomically take a queued/paused/stale-running run for this process.
UPDATE agent_runs SET status = 'running', owner_pid = $2, heartbeat_at = now(), started_at = COALESCE(started_at, now())
WHERE id = $1 AND (status IN ('queued','paused_steer') OR (status = 'running' AND heartbeat_at < now() - interval '90 seconds'))
RETURNING *;

-- name: ResumeRunAfterApproval :one
UPDATE agent_runs SET status = 'running', owner_pid = $2, heartbeat_at = now()
WHERE id = $1 AND status = 'paused_approval'
RETURNING *;

-- name: HeartbeatRun :exec
UPDATE agent_runs SET heartbeat_at = now() WHERE id = $1 AND owner_pid = $2;

-- name: BumpRunStep :exec
UPDATE agent_runs SET step_count = step_count + 1, cost_usd = cost_usd + $2 WHERE id = $1;

-- name: ListStaleRuns :many
SELECT * FROM agent_runs WHERE status = 'running' AND heartbeat_at < now() - interval '90 seconds' ORDER BY heartbeat_at LIMIT 20;

-- name: ListQueuedRuns :many
SELECT * FROM agent_runs WHERE status = 'queued' ORDER BY created_at LIMIT 20;

-- name: CancelRun :exec
UPDATE agent_runs SET status = 'cancelled', ended_at = now() WHERE id = $1 AND status NOT IN ('done','failed','cancelled');

-- name: InsertStep :one
INSERT INTO agent_steps (run_id, seq, kind, input, checkpoint) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: FinishStep :exec
UPDATE agent_steps SET output = $2, usage = $3, error = $4, checkpoint = COALESCE($5, checkpoint), ended_at = now() WHERE id = $1;

-- name: ListSteps :many
SELECT * FROM agent_steps WHERE run_id = $1 ORDER BY seq;

-- name: LastStep :one
SELECT * FROM agent_steps WHERE run_id = $1 ORDER BY seq DESC LIMIT 1;

-- name: NextStepSeq :one
SELECT COALESCE(MAX(seq), 0) + 1 FROM agent_steps WHERE run_id = $1;

-- name: CreateApproval :one
INSERT INTO approvals (run_id, step_seq, tool_call_id, tool_name, args) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: GetApproval :one
SELECT * FROM approvals WHERE id = $1;

-- name: ListPendingApprovals :many
SELECT * FROM approvals WHERE run_id = $1 AND status = 'pending' ORDER BY created_at;

-- name: ListPendingApprovalsForUser :many
SELECT a.* FROM approvals a JOIN agent_runs r ON r.id = a.run_id
WHERE r.user_id = $1 AND a.status = 'pending' ORDER BY a.created_at DESC;

-- name: DecideApproval :one
UPDATE approvals SET status = $2, decided_by = $3, decided_at = now(), note = $4
WHERE id = $1 AND status = 'pending' RETURNING *;

-- name: DecideApprovalByCall :one
UPDATE approvals SET status = $3, decided_by = $4, decided_at = now()
WHERE run_id = $1 AND tool_call_id = $2 AND status = 'pending' RETURNING *;

-- name: CreateAgent :one
INSERT INTO agents (owner_id, project_id, name, goal, system_prompt, model_policy, tool_allowlist, tool_policies, max_steps)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING *;

-- name: GetAgent :one
SELECT * FROM agents WHERE id = $1;

-- name: ListAgentsForUser :many
SELECT * FROM agents WHERE owner_id = $1 ORDER BY created_at DESC;

-- name: UpdateAgent :one
UPDATE agents SET name = $2, goal = $3, system_prompt = $4, model_policy = $5, tool_allowlist = $6, tool_policies = $7, max_steps = $8, enabled = $9, project_id = $10
WHERE id = $1 RETURNING *;

-- name: SetAgentEnabled :exec
UPDATE agents SET enabled = $2 WHERE id = $1;

-- name: TouchAgentRun :exec
UPDATE agents SET last_run_at = now() WHERE id = $1;

-- name: CountOpenRunsForAgent :one
SELECT count(*) FROM agent_runs WHERE agent_id = $1 AND status IN ('queued','running','paused_approval','paused_steer','paused_manual');

-- name: ListPendingApprovalsForAgent :many
SELECT a.* FROM approvals a JOIN agent_runs r ON r.id = a.run_id
WHERE r.agent_id = $1 AND a.status = 'pending' ORDER BY a.created_at DESC;

-- name: SetConversationAgent :exec
UPDATE conversations SET agent_id = $2 WHERE id = $1;

-- name: CreateTrigger :one
INSERT INTO agent_triggers (agent_id, kind, name, spec, secret_hash, next_run_at)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetTrigger :one
SELECT * FROM agent_triggers WHERE id = $1;

-- name: ListTriggersForAgent :many
SELECT * FROM agent_triggers WHERE agent_id = $1 ORDER BY created_at;

-- name: DeleteTrigger :exec
DELETE FROM agent_triggers WHERE id = $1 AND agent_id = $2;

-- name: SetTriggerEnabled :exec
UPDATE agent_triggers SET enabled = $3 WHERE id = $1 AND agent_id = $2;

-- name: ListDueCronTriggers :many
SELECT t.* FROM agent_triggers t JOIN agents a ON a.id = t.agent_id
WHERE t.enabled AND a.enabled AND t.kind = 'cron' AND t.next_run_at IS NOT NULL AND t.next_run_at <= now()
ORDER BY t.next_run_at LIMIT 50;

-- name: SetTriggerFired :exec
UPDATE agent_triggers SET last_run_at = now(), next_run_at = $2, last_error = $3 WHERE id = $1;

-- name: DeleteAgent :exec
DELETE FROM agents WHERE id = $1;

-- name: ListMessagesBySeq :many
SELECT * FROM messages WHERE conversation_id = $1 AND seq > $2 AND seq <= $3 ORDER BY seq;

-- name: SetRunFirstMessage :exec
UPDATE agent_runs SET first_message_id = $2 WHERE id = $1 AND first_message_id IS NULL;

-- name: UpdateStepCheckpoint :exec
UPDATE agent_steps SET checkpoint = $2 WHERE id = $1;

-- name: ListApprovalsForRun :many
SELECT * FROM approvals WHERE run_id = $1 ORDER BY created_at;

-- name: FindToolStep :one
SELECT * FROM agent_steps WHERE run_id = $1 AND kind = 'tool' AND input->>'call_id' = sqlc.arg(call_id)::text ORDER BY seq DESC LIMIT 1;
