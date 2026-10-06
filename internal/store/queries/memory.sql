-- name: UpsertMemory :one
INSERT INTO agent_memory (agent_id, kind, content, content_hash, embedding, source_run_id, importance)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (agent_id, content_hash) DO UPDATE SET
  importance = GREATEST(agent_memory.importance, EXCLUDED.importance),
  embedding = COALESCE(EXCLUDED.embedding, agent_memory.embedding),
  source_run_id = COALESCE(EXCLUDED.source_run_id, agent_memory.source_run_id)
RETURNING *;

-- name: ListMemoriesForAgent :many
SELECT * FROM agent_memory WHERE agent_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3;

-- name: CountMemoriesForAgent :one
SELECT count(*) FROM agent_memory WHERE agent_id = $1;

-- name: DeleteMemory :exec
DELETE FROM agent_memory WHERE id = $1 AND agent_id = $2;

-- name: DeleteMemoriesForAgent :exec
DELETE FROM agent_memory WHERE agent_id = $1;

-- name: SearchMemories :many
SELECT *, (embedding <=> $2::vector)::float8 AS distance FROM agent_memory
WHERE agent_id = $1 AND embedding IS NOT NULL
ORDER BY embedding <=> $2::vector LIMIT $3;

-- name: TopMemoriesByImportance :many
SELECT * FROM agent_memory WHERE agent_id = $1 ORDER BY importance DESC, created_at DESC LIMIT $2;

-- name: TouchMemories :exec
UPDATE agent_memory SET last_used_at = now() WHERE id = ANY($1::uuid[]);
