-- +goose Up
-- Document reading (PLAN.md M13, cut 4): the text of an uploaded document,
-- kept per attachment so the model can page through or search it with
-- read_document. The original stays in the blob store through the
-- attachments row; chunks are page-bound and embedded like memories.
CREATE TABLE documents (
    attachment_id   uuid PRIMARY KEY REFERENCES attachments(id) ON DELETE CASCADE,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL,
    kind            text NOT NULL,                       -- pdf, docx, xlsx, pptx, text, image
    method          text NOT NULL CHECK (method IN ('text','ocr')),
    pages           int NOT NULL,
    chars           int NOT NULL,
    truncated       boolean NOT NULL DEFAULT false,      -- the page limit cut it short
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX documents_user_idx ON documents(user_id, created_at DESC);

CREATE TABLE document_chunks (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    attachment_id uuid NOT NULL REFERENCES documents(attachment_id) ON DELETE CASCADE,
    seq           int NOT NULL,                          -- reading order
    page          int NOT NULL,                          -- page, sheet or slide number
    content       text NOT NULL,
    embedding     vector(768),                           -- NULL when no embedding model answered
    UNIQUE (attachment_id, seq)
);
CREATE INDEX document_chunks_page_idx ON document_chunks(attachment_id, page, seq);
CREATE INDEX document_chunks_embedding_idx ON document_chunks USING hnsw (embedding vector_cosine_ops);

-- +goose Down
DROP TABLE document_chunks;
DROP TABLE documents;
