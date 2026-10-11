-- name: UpsertUserMemory :one
INSERT INTO user_memory (user_id, kind, content, content_hash, embedding, source_conversation_id, source, importance)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (user_id, content_hash) DO UPDATE SET
  importance = GREATEST(user_memory.importance, EXCLUDED.importance),
  embedding = COALESCE(EXCLUDED.embedding, user_memory.embedding),
  source_conversation_id = COALESCE(EXCLUDED.source_conversation_id, user_memory.source_conversation_id)
RETURNING *;

-- name: ListUserMemories :many
SELECT * FROM user_memory WHERE user_id = $1 ORDER BY importance DESC, created_at DESC LIMIT $2 OFFSET $3;

-- name: CountUserMemories :one
SELECT count(*) FROM user_memory WHERE user_id = $1;

-- name: GetUserMemory :one
SELECT * FROM user_memory WHERE id = $1 AND user_id = $2;

-- name: UpdateUserMemory :one
UPDATE user_memory SET content = $3, content_hash = $4, kind = $5, importance = $6, embedding = $7
WHERE id = $1 AND user_id = $2 RETURNING *;

-- name: DeleteUserMemory :exec
DELETE FROM user_memory WHERE id = $1 AND user_id = $2;

-- name: DeleteUserMemories :exec
DELETE FROM user_memory WHERE user_id = $1;

-- name: SearchUserMemories :many
SELECT *, (embedding <=> $2::vector)::float8 AS distance FROM user_memory
WHERE user_id = $1 AND embedding IS NOT NULL
ORDER BY embedding <=> $2::vector LIMIT $3;

-- name: TopUserMemories :many
SELECT * FROM user_memory WHERE user_id = $1 ORDER BY importance DESC, created_at DESC LIMIT $2;

-- name: TouchUserMemories :exec
UPDATE user_memory SET last_used_at = now() WHERE id = ANY(sqlc.arg(ids)::uuid[]) AND user_id = sqlc.arg(user_id);

-- Importance decays for memories nobody has used since the cutoff; the
-- floor keeps them recallable by search without outranking live ones.
-- name: DecayUserMemories :execrows
UPDATE user_memory SET importance = GREATEST(importance * sqlc.arg(factor)::real, 0.05)
WHERE user_id = sqlc.arg(user_id) AND importance > 0.05 AND COALESCE(last_used_at, created_at) < sqlc.arg(cutoff)::timestamptz;

-- name: GetMemorySettings :one
SELECT * FROM memory_settings WHERE user_id = $1;

-- name: UpsertMemorySettings :one
INSERT INTO memory_settings (user_id, enabled, reflect_enabled, reflect_at, timezone, profile)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id) DO UPDATE SET enabled = EXCLUDED.enabled, reflect_enabled = EXCLUDED.reflect_enabled,
  reflect_at = EXCLUDED.reflect_at, timezone = EXCLUDED.timezone, profile = EXCLUDED.profile, updated_at = now()
RETURNING *;

-- name: MarkMemoryReflected :exec
INSERT INTO memory_settings (user_id, last_reflected_at) VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET last_reflected_at = EXCLUDED.last_reflected_at;

-- name: ListReflectingMembers :many
SELECT s.* FROM memory_settings s JOIN users u ON u.id = s.user_id
WHERE s.enabled AND s.reflect_enabled AND u.disabled_at IS NULL;

-- Conversations of one member with activity since a time, for reflection.
-- name: ListConversationsActiveSince :many
SELECT c.id, c.title, c.updated_at FROM conversations c
WHERE c.user_id = $1 AND c.updated_at > $2 AND c.archived_at IS NULL AND c.mode <> 'agent'
ORDER BY c.updated_at DESC LIMIT $3;
