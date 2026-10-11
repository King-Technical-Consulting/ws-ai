-- +goose Up
-- Personal memory (PLAN.md M13, cut 3): what ws keeps about one member
-- between chats, with an embedding for recall by similarity, plus the
-- member's switches and the personal-context card. Per member and never
-- shared: every query filters by user_id. agent_memory (0011) stays as it
-- is; this is a separate table so the agent code is untouched.
CREATE TABLE user_memory (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind                   text NOT NULL CHECK (kind IN ('fact','preference','decision')),
    content                text NOT NULL,
    content_hash           bytea NOT NULL,                  -- sha256(lower(content)) for dedupe
    embedding              vector(768),                     -- NULL when no embedding model was reachable
    source_conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL,
    source                 text NOT NULL DEFAULT 'chat' CHECK (source IN ('chat','reflection','manual')),
    importance             real NOT NULL DEFAULT 0.5,       -- 0..1; reflection demotes unused ones
    last_used_at           timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, content_hash)
);
CREATE INDEX user_memory_user_idx ON user_memory(user_id, created_at DESC);
CREATE INDEX user_memory_importance_idx ON user_memory(user_id, importance DESC);
CREATE INDEX user_memory_embedding_idx ON user_memory USING hnsw (embedding vector_cosine_ops);

-- One row per member who has touched memory settings; no row means the
-- defaults (memory on, reflection off).
CREATE TABLE memory_settings (
    user_id           uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    enabled           boolean NOT NULL DEFAULT true,       -- remember and recall in chats, and the card
    reflect_enabled   boolean NOT NULL DEFAULT false,      -- the nightly memory.reflect job
    reflect_at        text NOT NULL DEFAULT '03:00',       -- HH:MM in timezone
    timezone          text NOT NULL DEFAULT 'UTC',         -- IANA name
    profile           jsonb NOT NULL DEFAULT '{}',         -- the personal-context card
    last_reflected_at timestamptz,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE memory_settings;
DROP TABLE user_memory;
