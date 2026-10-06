-- +goose Up
-- Agent memory (PLAN.md M7): facts, episodes and preferences an agent
-- keeps between runs, with an embedding for recall by similarity. The
-- reflect job writes rows after each run; the next run's start recalls
-- the nearest and the most important ones into its system prompt.
-- Dimensions are fixed at 768 (nomic-embed-text, bge-base; OpenAI's
-- text-embedding-3 models accept dimensions=768), because an HNSW index
-- needs a fixed width.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE agent_memory (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id       uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    kind           text NOT NULL CHECK (kind IN ('fact','episode','preference')),
    content        text NOT NULL,
    content_hash   bytea NOT NULL,                     -- sha256(content) for dedupe
    embedding      vector(768),                        -- NULL when no embedding model was reachable
    source_run_id  uuid REFERENCES agent_runs(id) ON DELETE SET NULL,
    importance     real NOT NULL DEFAULT 0.5,          -- 0..1
    last_used_at   timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (agent_id, content_hash)
);
CREATE INDEX agent_memory_agent_idx ON agent_memory(agent_id, created_at DESC);
CREATE INDEX agent_memory_importance_idx ON agent_memory(agent_id, importance DESC);
CREATE INDEX agent_memory_embedding_idx ON agent_memory USING hnsw (embedding vector_cosine_ops);

-- +goose Down
DROP TABLE agent_memory;
