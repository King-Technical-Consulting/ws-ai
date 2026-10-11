-- name: UpsertFavorite :one
INSERT INTO favorites (user_id, kind, ref_id, title, snippet, conversation_id, project_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id, kind, ref_id) DO UPDATE SET title = EXCLUDED.title, snippet = EXCLUDED.snippet
RETURNING *;

-- Favorites whose target still exists and whose project the member can
-- still reach (owner or member of it, not archived).
-- name: ListFavorites :many
SELECT f.* FROM favorites f
WHERE f.user_id = $1
  AND (
    (f.kind = 'message' AND EXISTS (SELECT 1 FROM messages m WHERE m.id = f.ref_id)) OR
    (f.kind = 'artifact' AND EXISTS (SELECT 1 FROM artifacts a WHERE a.id = f.ref_id)) OR
    (f.kind = 'media' AND EXISTS (SELECT 1 FROM media_jobs j WHERE j.id = f.ref_id))
  )
  AND (f.project_id IS NULL OR EXISTS (
    SELECT 1 FROM projects p LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = $1
    WHERE p.id = f.project_id AND p.archived_at IS NULL AND (p.owner_id = $1 OR pm.user_id IS NOT NULL)))
ORDER BY f.created_at DESC LIMIT 500;

-- name: CountFavorites :one
SELECT count(*) FROM favorites WHERE user_id = $1;

-- name: DeleteFavorite :exec
DELETE FROM favorites WHERE user_id = $1 AND kind = $2 AND ref_id = $3;

-- name: CreateSavedPrompt :one
INSERT INTO saved_prompts (user_id, name, text) VALUES ($1, $2, $3) RETURNING *;

-- name: ListSavedPrompts :many
SELECT * FROM saved_prompts WHERE user_id = $1 ORDER BY lower(name), created_at;

-- name: CountSavedPrompts :one
SELECT count(*) FROM saved_prompts WHERE user_id = $1;

-- name: UpdateSavedPrompt :one
UPDATE saved_prompts SET name = $3, text = $4, updated_at = now() WHERE id = $1 AND user_id = $2 RETURNING *;

-- name: DeleteSavedPrompt :exec
DELETE FROM saved_prompts WHERE id = $1 AND user_id = $2;
