-- name: CreateMediaJob :one
INSERT INTO media_jobs (user_id, project_id, conversation_id, kind, selector, inputs)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetMediaJob :one
SELECT * FROM media_jobs WHERE id = $1;

-- name: ListMediaJobsForProject :many
SELECT * FROM media_jobs WHERE project_id = $1 AND (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind')::text)
ORDER BY created_at DESC LIMIT $2 OFFSET $3;

-- name: ClaimMediaJob :one
UPDATE media_jobs SET status = 'running', started_at = now(), progress = 0, error = NULL
WHERE id = $1 AND status = 'queued' RETURNING *;

-- name: SetMediaJobProgress :exec
UPDATE media_jobs SET progress = $2, provider_job_id = COALESCE(sqlc.narg('provider_job_id'), provider_job_id)
WHERE id = $1 AND status = 'running';

-- name: FinishMediaJob :exec
UPDATE media_jobs SET status = 'done', progress = 1, endpoint_id = $2, output_attachment_ids = $3, cost_usd = $4, ended_at = now()
WHERE id = $1;

-- name: FailMediaJob :exec
UPDATE media_jobs SET status = 'failed', endpoint_id = COALESCE(sqlc.narg('endpoint_id'), endpoint_id), error = $2, ended_at = now()
WHERE id = $1;

-- name: CancelMediaJob :one
UPDATE media_jobs SET status = 'cancelled', ended_at = now() WHERE id = $1 AND status = 'queued' RETURNING *;

-- name: DeleteMediaJob :exec
DELETE FROM media_jobs WHERE id = $1 AND status IN ('done','failed','cancelled');

-- name: ListAttachmentsByIDs :many
SELECT * FROM attachments WHERE id = ANY($1::uuid[]);

-- name: AttachmentMediaProjects :many
SELECT project_id FROM media_jobs WHERE $1::uuid = ANY(output_attachment_ids);
