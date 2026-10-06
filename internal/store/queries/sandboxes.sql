-- name: GetSandbox :one
SELECT * FROM sandboxes WHERE project_id = $1 AND user_id = $2;

-- name: GetSandboxByID :one
SELECT * FROM sandboxes WHERE id = $1;

-- name: InsertSandbox :one
INSERT INTO sandboxes (project_id, user_id, volume_name, image, runtime)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: SetSandboxContainer :exec
UPDATE sandboxes SET container_id = $2, status = $3, error = NULL, last_used_at = now(), updated_at = now() WHERE id = $1;

-- name: SetSandboxStatus :exec
UPDATE sandboxes SET status = $2, error = $3, updated_at = now() WHERE id = $1;

-- name: TouchSandbox :exec
UPDATE sandboxes SET last_used_at = now() WHERE id = $1;

-- name: ListSandboxesRunningIdleSince :many
SELECT * FROM sandboxes WHERE status = 'running' AND last_used_at < $1 ORDER BY last_used_at;

-- name: ListSandboxesStoppedSince :many
SELECT * FROM sandboxes WHERE status IN ('stopped','failed') AND container_id IS NOT NULL AND last_used_at < $1 ORDER BY last_used_at;

-- name: ListSandboxesForProject :many
SELECT * FROM sandboxes WHERE project_id = $1 ORDER BY created_at;

-- name: DeleteSandbox :exec
DELETE FROM sandboxes WHERE id = $1;

-- name: InsertSandboxEvent :exec
INSERT INTO sandbox_events (sandbox_id, kind, detail) VALUES ($1, $2, $3);

-- name: SetProjectRepo :exec
UPDATE projects SET repo_url = $2, default_branch = $3, updated_at = now() WHERE id = $1;

-- name: GetSandboxByShortID :one
SELECT * FROM sandboxes WHERE left(replace(id::text, '-', ''), 12) = sqlc.arg(short)::text;

-- name: SetProjectInstallation :exec
UPDATE projects SET github_installation_id = $2, updated_at = now() WHERE id = $1;
