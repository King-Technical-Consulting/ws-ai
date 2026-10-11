-- name: CreateDocument :one
INSERT INTO documents (attachment_id, user_id, conversation_id, kind, method, pages, chars, truncated)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (attachment_id) DO UPDATE SET method = EXCLUDED.method, pages = EXCLUDED.pages, chars = EXCLUDED.chars, truncated = EXCLUDED.truncated
RETURNING *;

-- name: GetDocument :one
SELECT * FROM documents WHERE attachment_id = $1 AND user_id = $2;

-- name: DeleteDocumentChunks :exec
DELETE FROM document_chunks WHERE attachment_id = $1;

-- name: InsertDocumentChunk :exec
INSERT INTO document_chunks (attachment_id, seq, page, content, embedding) VALUES ($1, $2, $3, $4, $5);

-- name: ListDocumentChunksByPages :many
SELECT seq, page, content FROM document_chunks
WHERE attachment_id = $1 AND page BETWEEN sqlc.arg(first_page) AND sqlc.arg(last_page)
ORDER BY seq;

-- name: SearchDocumentChunks :many
SELECT seq, page, content, (embedding <=> sqlc.arg(query_embedding)::vector)::float8 AS distance FROM document_chunks
WHERE attachment_id = $1 AND embedding IS NOT NULL
ORDER BY embedding <=> sqlc.arg(query_embedding)::vector LIMIT sqlc.arg(max_rows);
