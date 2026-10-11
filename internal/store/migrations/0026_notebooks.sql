-- +goose Up
-- Research notebooks (PLAN.md M13, cut 6): sources a member collects, the
-- notes written about them, and one chat whose context is the sources.
-- Everything is per member; every query filters by the notebook's user_id.
CREATE TABLE notebooks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            text NOT NULL,
    conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL,  -- the notebook's chat, made on first use
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notebooks_user_idx ON notebooks(user_id, updated_at DESC);

CREATE TABLE notebook_sources (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    notebook_id          uuid NOT NULL REFERENCES notebooks(id) ON DELETE CASCADE,
    kind                 text NOT NULL CHECK (kind IN ('document','page','text','conversation')),
    title                text NOT NULL,
    url                  text NOT NULL DEFAULT '',                 -- kind page
    attachment_id        uuid REFERENCES attachments(id) ON DELETE SET NULL,   -- kind document: the original
    from_conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL, -- kind conversation
    pages                int NOT NULL DEFAULT 1,
    chars                int NOT NULL DEFAULT 0,
    created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notebook_sources_nb_idx ON notebook_sources(notebook_id, created_at);

CREATE TABLE notebook_chunks (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id uuid NOT NULL REFERENCES notebook_sources(id) ON DELETE CASCADE,
    seq       int NOT NULL,
    page      int NOT NULL,
    content   text NOT NULL,
    embedding vector(768),                                          -- NULL when no embedding model answered
    UNIQUE (source_id, seq)
);
CREATE INDEX notebook_chunks_page_idx ON notebook_chunks(source_id, page, seq);
CREATE INDEX notebook_chunks_embedding_idx ON notebook_chunks USING hnsw (embedding vector_cosine_ops);

CREATE TABLE notebook_notes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    notebook_id uuid NOT NULL REFERENCES notebooks(id) ON DELETE CASCADE,
    kind        text NOT NULL CHECK (kind IN ('summary','open_questions','note')),
    title       text NOT NULL DEFAULT '',
    body        text NOT NULL,
    author      text NOT NULL DEFAULT 'user' CHECK (author IN ('user','model')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
-- One summary and one open-questions note per notebook; any number of notes.
CREATE UNIQUE INDEX notebook_notes_singleton_idx ON notebook_notes(notebook_id, kind) WHERE kind IN ('summary','open_questions');
CREATE INDEX notebook_notes_nb_idx ON notebook_notes(notebook_id, created_at);

-- +goose Down
DROP TABLE notebook_notes;
DROP TABLE notebook_chunks;
DROP TABLE notebook_sources;
DROP TABLE notebooks;
