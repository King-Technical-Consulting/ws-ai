-- name: CreateNotebook :one
INSERT INTO notebooks (user_id, name) VALUES ($1, $2) RETURNING *;

-- name: GetNotebook :one
SELECT * FROM notebooks WHERE id = $1 AND user_id = $2;

-- name: ListNotebooks :many
SELECT n.*, (SELECT count(*) FROM notebook_sources s WHERE s.notebook_id = n.id)::int AS source_count
FROM notebooks n WHERE n.user_id = $1 ORDER BY n.updated_at DESC;

-- name: CountNotebooks :one
SELECT count(*) FROM notebooks WHERE user_id = $1;

-- name: RenameNotebook :one
UPDATE notebooks SET name = $3, updated_at = now() WHERE id = $1 AND user_id = $2 RETURNING *;

-- name: TouchNotebook :exec
UPDATE notebooks SET updated_at = now() WHERE id = $1;

-- name: SetNotebookConversation :exec
UPDATE notebooks SET conversation_id = $2 WHERE id = $1;

-- name: DeleteNotebook :exec
DELETE FROM notebooks WHERE id = $1 AND user_id = $2;

-- name: GetNotebookByConversation :one
SELECT * FROM notebooks WHERE conversation_id = $1 AND user_id = $2;

-- name: CreateNotebookSource :one
INSERT INTO notebook_sources (notebook_id, kind, title, url, attachment_id, from_conversation_id, pages, chars)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING *;

-- name: ListNotebookSources :many
SELECT * FROM notebook_sources WHERE notebook_id = $1 ORDER BY created_at;

-- name: CountNotebookSources :one
SELECT count(*) FROM notebook_sources WHERE notebook_id = $1;

-- name: GetNotebookSource :one
SELECT * FROM notebook_sources WHERE id = $1 AND notebook_id = $2;

-- name: DeleteNotebookSource :exec
DELETE FROM notebook_sources WHERE id = $1 AND notebook_id = $2;

-- name: InsertNotebookChunk :exec
INSERT INTO notebook_chunks (source_id, seq, page, content, embedding) VALUES ($1, $2, $3, $4, $5);

-- name: ListNotebookChunks :many
SELECT seq, page, content FROM notebook_chunks WHERE source_id = $1 ORDER BY seq;

-- name: ListNotebookChunksFirst :many
SELECT c.source_id, c.seq, c.page, c.content FROM notebook_chunks c
JOIN notebook_sources s ON s.id = c.source_id
WHERE s.notebook_id = $1 AND c.seq = 0 ORDER BY s.created_at;

-- name: SearchNotebookChunks :many
SELECT c.source_id, c.seq, c.page, c.content, (c.embedding <=> sqlc.arg(query_embedding)::vector)::float8 AS distance
FROM notebook_chunks c JOIN notebook_sources s ON s.id = c.source_id
WHERE s.notebook_id = sqlc.arg(notebook_id) AND c.embedding IS NOT NULL
ORDER BY c.embedding <=> sqlc.arg(query_embedding)::vector LIMIT sqlc.arg(max_rows);

-- name: ListNotebookNotes :many
SELECT * FROM notebook_notes WHERE notebook_id = $1 ORDER BY CASE kind WHEN 'summary' THEN 0 WHEN 'open_questions' THEN 1 ELSE 2 END, created_at;

-- name: CountNotebookNotes :one
SELECT count(*) FROM notebook_notes WHERE notebook_id = $1;

-- name: GetNotebookNote :one
SELECT * FROM notebook_notes WHERE id = $1 AND notebook_id = $2;

-- name: CreateNotebookNote :one
INSERT INTO notebook_notes (notebook_id, kind, title, body, author) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- A singleton note (summary, open questions) is replaced in place.
-- name: UpsertNotebookSingletonNote :one
INSERT INTO notebook_notes (notebook_id, kind, title, body, author) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (notebook_id, kind) WHERE kind IN ('summary','open_questions')
DO UPDATE SET body = EXCLUDED.body, title = EXCLUDED.title, author = EXCLUDED.author, updated_at = now()
RETURNING *;

-- name: UpdateNotebookNote :one
UPDATE notebook_notes SET title = $3, body = $4, author = 'user', updated_at = now() WHERE id = $1 AND notebook_id = $2 RETURNING *;

-- name: DeleteNotebookNote :exec
DELETE FROM notebook_notes WHERE id = $1 AND notebook_id = $2;
