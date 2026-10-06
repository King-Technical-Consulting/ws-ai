-- +goose Up
-- Agents are durable first-class entities. Runs are resumable across
-- process restarts and model swaps because every step checkpoints the
-- canonical (provider-neutral) state. Chat turns are runs with agent_id NULL.

CREATE TABLE agents (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            text NOT NULL,
    goal            text NOT NULL DEFAULT '',
    system_prompt   text NOT NULL DEFAULT '',
    -- model_policy: {selector, task_class, reasoning}
    model_policy    jsonb NOT NULL DEFAULT '{}',
    tool_allowlist  text[] NOT NULL DEFAULT '{}',          -- empty = all built-ins
    -- tool_policies: {"tool_name": "auto"|"ask"|"deny"}
    tool_policies   jsonb NOT NULL DEFAULT '{}',
    mcp_servers     jsonb NOT NULL DEFAULT '[]',
    memory_config   jsonb NOT NULL DEFAULT '{}',
    max_steps       int NOT NULL DEFAULT 50,
    enabled         boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX agents_owner_idx ON agents(owner_id);

CREATE TABLE agent_triggers (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id    uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    kind        text NOT NULL CHECK (kind IN ('cron','webhook','repo_push','manual')),
    spec        jsonb NOT NULL DEFAULT '{}',              -- cron: {expr}, webhook: {}, repo_push: {repo, branch}
    secret_hash bytea,
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX agent_triggers_agent_idx ON agent_triggers(agent_id);

CREATE TABLE agent_runs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id        uuid REFERENCES agents(id) ON DELETE CASCADE,          -- NULL for plain chat turns
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    user_id         uuid REFERENCES users(id) ON DELETE SET NULL,
    trigger_id      uuid REFERENCES agent_triggers(id) ON DELETE SET NULL,
    status          text NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued','running','paused_approval','paused_steer','done','failed','cancelled')),
    -- request: the canonical request skeleton (selector, system, tools, reasoning) minus messages
    request         jsonb NOT NULL DEFAULT '{}',
    -- tool_policies resolved for this run
    tool_policies   jsonb NOT NULL DEFAULT '{}',
    max_steps       int NOT NULL DEFAULT 12,
    step_count      int NOT NULL DEFAULT 0,
    first_message_id uuid REFERENCES messages(id) ON DELETE SET NULL,     -- the assistant message the UI streams into
    cost_usd        numeric(12,6) NOT NULL DEFAULT 0,
    error           text,
    -- heartbeat lets a reaper detect a run whose process died mid-step
    heartbeat_at    timestamptz NOT NULL DEFAULT now(),
    owner_pid       text,
    started_at      timestamptz,
    ended_at        timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX agent_runs_conv_idx ON agent_runs(conversation_id, created_at DESC);
CREATE INDEX agent_runs_agent_idx ON agent_runs(agent_id, created_at DESC) WHERE agent_id IS NOT NULL;
CREATE INDEX agent_runs_active_idx ON agent_runs(status, heartbeat_at) WHERE status IN ('queued','running');

CREATE TABLE agent_steps (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id      uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    seq         int NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('llm','tool','approval','compaction')),
    -- llm: {endpoint, model, usage, finish_reason}; tool: {name, call_id, duration_ms, externalized}
    input       jsonb,
    output      jsonb,
    -- checkpoint: {message_seq (last persisted), pending_tool_calls:[ids], budget_used}
    checkpoint  jsonb,
    usage       jsonb,
    error       text,
    started_at  timestamptz NOT NULL DEFAULT now(),
    ended_at    timestamptz,
    UNIQUE (run_id, seq)
);

CREATE TABLE approvals (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id      uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    step_seq    int NOT NULL,
    tool_call_id text NOT NULL,
    tool_name   text NOT NULL,
    args        jsonb NOT NULL DEFAULT '{}',
    status      text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','denied','expired')),
    decided_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    decided_at  timestamptz,
    note        text,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX approvals_run_idx ON approvals(run_id, status);

ALTER TABLE conversations
    ADD CONSTRAINT conversations_agent_fk FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE SET NULL;

-- Per-message sequence pointer for compaction: which rows the latest block covers
-- already lives on conversations.compaction_head_message_seq (0001).

CREATE TRIGGER agents_updated_at BEFORE UPDATE ON agents FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER agent_runs_updated_at BEFORE UPDATE ON agent_runs FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
ALTER TABLE conversations DROP CONSTRAINT IF EXISTS conversations_agent_fk;
DROP TABLE IF EXISTS approvals, agent_steps, agent_runs, agent_triggers, agents CASCADE;
