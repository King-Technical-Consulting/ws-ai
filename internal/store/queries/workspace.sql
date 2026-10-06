-- name: CreateProject :one
INSERT INTO projects (owner_id, name, kind, settings) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetProject :one
SELECT * FROM projects WHERE id = $1 AND archived_at IS NULL;

-- name: ListProjectsForUser :many
SELECT p.* FROM projects p
LEFT JOIN project_members m ON m.project_id = p.id AND m.user_id = $1
WHERE p.archived_at IS NULL AND (p.owner_id = $1 OR m.user_id IS NOT NULL)
ORDER BY p.updated_at DESC;

-- name: UserCanAccessProject :one
SELECT EXISTS (
  SELECT 1 FROM projects p
  LEFT JOIN project_members m ON m.project_id = p.id AND m.user_id = $2
  WHERE p.id = $1 AND p.archived_at IS NULL AND (p.owner_id = $2 OR m.user_id IS NOT NULL)
);

-- name: UpdateProject :one
UPDATE projects SET name = $2, kind = $3, settings = $4 WHERE id = $1 RETURNING *;

-- name: ArchiveProject :exec
UPDATE projects SET archived_at = now() WHERE id = $1;

-- name: CreateConversation :one
INSERT INTO conversations (project_id, user_id, title, mode, model_selector, settings)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetConversation :one
SELECT * FROM conversations WHERE id = $1 AND archived_at IS NULL;

-- name: ListConversationsForProject :many
SELECT * FROM conversations WHERE project_id = $1 AND archived_at IS NULL
ORDER BY updated_at DESC LIMIT $2 OFFSET $3;

-- name: ListRecentConversationsForUser :many
SELECT * FROM conversations WHERE user_id = $1 AND archived_at IS NULL
ORDER BY updated_at DESC LIMIT $2;

-- name: UpdateConversationTitle :exec
UPDATE conversations SET title = $2 WHERE id = $1;

-- name: UpdateConversationSelector :exec
UPDATE conversations SET model_selector = $2 WHERE id = $1;

-- name: TouchConversation :exec
UPDATE conversations SET updated_at = now() WHERE id = $1;

-- name: ArchiveConversation :exec
UPDATE conversations SET archived_at = now() WHERE id = $1;

-- name: SetCompactionHead :exec
UPDATE conversations SET compaction_head_message_seq = $2 WHERE id = $1;

-- name: NextMessageSeq :one
SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE conversation_id = $1;

-- name: InsertMessage :one
INSERT INTO messages (conversation_id, seq, role, parts, endpoint_id, model, usage, finish_reason, parent_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING *;

-- name: UpdateMessageParts :exec
UPDATE messages SET parts = $2, endpoint_id = $3, model = $4, usage = $5, finish_reason = $6 WHERE id = $1;

-- name: ListMessages :many
SELECT * FROM messages WHERE conversation_id = $1 ORDER BY seq;

-- name: ListMessagesFromSeq :many
SELECT * FROM messages WHERE conversation_id = $1 AND seq >= $2 ORDER BY seq;

-- name: ListMessagesAfterSeq :many
SELECT * FROM messages WHERE conversation_id = $1 AND seq > $2 ORDER BY seq;

-- name: GetMessage :one
SELECT * FROM messages WHERE id = $1;

-- name: DeleteMessagesFromSeq :exec
DELETE FROM messages WHERE conversation_id = $1 AND seq >= $2;

-- name: CreateAttachment :one
INSERT INTO attachments (user_id, blob_key, mime, bytes, sha256, filename, width, height)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING *;

-- name: GetAttachment :one
SELECT * FROM attachments WHERE id = $1;

-- name: LinkMessageAttachment :exec
INSERT INTO message_attachments (message_id, attachment_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: ListAttachmentsForMessage :many
SELECT a.* FROM attachments a JOIN message_attachments ma ON ma.attachment_id = a.id WHERE ma.message_id = $1;

-- name: InsertCompaction :one
INSERT INTO compactions (conversation_id, covers_through_seq, block, token_count, endpoint_id)
VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: LatestCompaction :one
SELECT * FROM compactions WHERE conversation_id = $1 ORDER BY covers_through_seq DESC LIMIT 1;

-- name: CreateArtifact :one
INSERT INTO artifacts (conversation_id, kind, title, language) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetArtifact :one
SELECT * FROM artifacts WHERE id = $1;

-- name: ListArtifactsForConversation :many
SELECT * FROM artifacts WHERE conversation_id = $1 ORDER BY created_at;

-- name: InsertArtifactVersion :one
INSERT INTO artifact_versions (artifact_id, version, content, blob_key, design_context, created_by_message_id)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: BumpArtifactVersion :one
UPDATE artifacts SET current_version = current_version + 1, title = COALESCE(NULLIF($2, ''), title)
WHERE id = $1 RETURNING current_version;

-- name: GetArtifactVersion :one
SELECT * FROM artifact_versions WHERE artifact_id = $1 AND version = $2;

-- name: GetArtifactVersionByID :one
SELECT * FROM artifact_versions WHERE id = $1;

-- name: ListArtifactVersions :many
SELECT id, artifact_id, version, created_by_message_id, created_at FROM artifact_versions WHERE artifact_id = $1 ORDER BY version;

-- name: GetArtifactConversation :one
SELECT c.* FROM conversations c JOIN artifacts a ON a.conversation_id = c.id WHERE a.id = $1;

-- name: UpdateConversationSettings :exec
UPDATE conversations SET settings = $2 WHERE id = $1;
