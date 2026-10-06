-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;
-- pgvector is enabled in the agent-memory migration (M7); plain Postgres is
-- enough for everything before that.

-- ---------------------------------------------------------------------------
-- Identity
-- ---------------------------------------------------------------------------
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         citext UNIQUE NOT NULL,
    display_name  text NOT NULL DEFAULT '',
    role          text NOT NULL DEFAULT 'member' CHECK (role IN ('owner','member')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    disabled_at   timestamptz
);

CREATE TABLE invites (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email       citext NOT NULL,
    token_hash  bytea UNIQUE NOT NULL,
    invited_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    role        text NOT NULL DEFAULT 'member' CHECK (role IN ('owner','member')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    used_by     uuid REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX invites_email_idx ON invites(email);

CREATE TABLE passkeys (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id   bytea UNIQUE NOT NULL,
    public_key      bytea NOT NULL,
    attestation_type text NOT NULL DEFAULT '',
    transports      text[] NOT NULL DEFAULT '{}',
    aaguid          bytea,
    sign_count      bigint NOT NULL DEFAULT 0,
    backup_eligible boolean NOT NULL DEFAULT false,
    backup_state    boolean NOT NULL DEFAULT false,
    name            text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_used_at    timestamptz
);
CREATE INDEX passkeys_user_idx ON passkeys(user_id);

CREATE TABLE magic_links (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  bytea UNIQUE NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz
);

CREATE TABLE sessions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  bytea UNIQUE NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    user_agent  text NOT NULL DEFAULT '',
    ip          inet,
    revoked_at  timestamptz
);
CREATE INDEX sessions_user_idx ON sessions(user_id);

-- WebAuthn ceremonies need short-lived server state between begin/finish.
CREATE TABLE webauthn_sessions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid REFERENCES users(id) ON DELETE CASCADE,
    kind        text NOT NULL CHECK (kind IN ('register','login')),
    data        jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);

-- ---------------------------------------------------------------------------
-- Workspace
-- ---------------------------------------------------------------------------
CREATE TABLE projects (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id                uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name                    text NOT NULL,
    kind                    text NOT NULL DEFAULT 'chat' CHECK (kind IN ('chat','code','design','images')),
    settings                jsonb NOT NULL DEFAULT '{}',
    repo_url                text,
    default_branch          text,
    github_installation_id  bigint,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    archived_at             timestamptz
);
CREATE INDEX projects_owner_idx ON projects(owner_id);

CREATE TABLE project_members (
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role        text NOT NULL DEFAULT 'editor' CHECK (role IN ('viewer','editor','admin')),
    added_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, user_id)
);

-- ---------------------------------------------------------------------------
-- Conversations
-- ---------------------------------------------------------------------------
CREATE TABLE conversations (
    id                          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id                  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id                     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title                       text NOT NULL DEFAULT '',
    mode                        text NOT NULL DEFAULT 'chat' CHECK (mode IN ('chat','code','design','images','agent')),
    agent_id                    uuid,                       -- FK added in 0003
    model_selector              text NOT NULL DEFAULT 'auto',
    settings                    jsonb NOT NULL DEFAULT '{}',
    compaction_head_message_seq bigint,                     -- messages at or below this seq are covered by the latest compaction
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    archived_at                 timestamptz
);
CREATE INDEX conversations_project_idx ON conversations(project_id, updated_at DESC);
CREATE INDEX conversations_user_idx ON conversations(user_id, updated_at DESC);

CREATE TABLE messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    seq             bigint NOT NULL,
    role            text NOT NULL CHECK (role IN ('system','user','assistant','tool')),
    -- parts is the canonical gateway.Part array (provider-neutral).
    parts           jsonb NOT NULL DEFAULT '[]',
    endpoint_id     text,
    model           text,
    usage           jsonb,
    finish_reason   text,
    parent_id       uuid REFERENCES messages(id) ON DELETE SET NULL,  -- for branching
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, seq)
);

CREATE TABLE attachments (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    blob_key    text NOT NULL,
    mime        text NOT NULL,
    bytes       bigint NOT NULL,
    sha256      bytea NOT NULL,
    filename    text NOT NULL DEFAULT '',
    width       int,
    height      int,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE message_attachments (
    message_id      uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    attachment_id   uuid NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
    PRIMARY KEY (message_id, attachment_id)
);

CREATE TABLE compactions (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id     uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    covers_through_seq  bigint NOT NULL,
    -- block: {goals, decisions, open_threads, file_state, tool_state, summary}
    block               jsonb NOT NULL,
    token_count         int NOT NULL,
    endpoint_id         text,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX compactions_conv_idx ON compactions(conversation_id, covers_through_seq DESC);

-- ---------------------------------------------------------------------------
-- Artifacts
-- ---------------------------------------------------------------------------
CREATE TABLE artifacts (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id     uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    kind                text NOT NULL CHECK (kind IN ('html','react','svg','markdown','mermaid','design','code')),
    title               text NOT NULL DEFAULT '',
    language            text,
    current_version     int NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE artifact_versions (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    artifact_id             uuid NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    version                 int NOT NULL,
    content                 text,
    blob_key                text,
    design_context          jsonb,
    created_by_message_id   uuid REFERENCES messages(id) ON DELETE SET NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (artifact_id, version)
);

-- ---------------------------------------------------------------------------
-- Gateway: providers, endpoints, policies, ledger, budgets
-- ---------------------------------------------------------------------------
CREATE TABLE providers (
    id          text PRIMARY KEY,                       -- slug, e.g. 'anthropic', 'openrouter', 'orin-llama'
    kind        text NOT NULL CHECK (kind IN ('anthropic','openai_compat')),
    name        text NOT NULL,
    base_url    text NOT NULL,
    api_key_env text,                                   -- env var name holding the key; never store keys in DB
    headers     jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE endpoints (
    id               text PRIMARY KEY,                  -- slug, e.g. 'anthropic/claude-sonnet-4-5'
    provider_id      text NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    model_name       text NOT NULL,
    display_name     text NOT NULL,
    capabilities     jsonb NOT NULL DEFAULT '{}',
    pricing          jsonb NOT NULL DEFAULT '{}',
    throughput_class text NOT NULL DEFAULT 'medium',
    latency_class    text NOT NULL DEFAULT 'normal',
    is_local         boolean NOT NULL DEFAULT false,
    enabled          boolean NOT NULL DEFAULT true,
    -- health, maintained by the worker
    health_status    text NOT NULL DEFAULT 'unknown' CHECK (health_status IN ('unknown','healthy','degraded','down')),
    health_checked_at timestamptz,
    health_error     text,
    p50_latency_ms   int,
    error_rate       real,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE routing_policies (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text UNIQUE NOT NULL,
    yaml        text NOT NULL,
    priority    int NOT NULL DEFAULT 100,
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE budgets (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    scope       text NOT NULL CHECK (scope IN ('user','agent','api_key','global')),
    scope_id    uuid,
    period      text NOT NULL DEFAULT 'month' CHECK (period IN ('day','week','month','total')),
    limit_usd   numeric(12,4) NOT NULL,
    -- what happens at the limit: block, or downgrade to local-only
    on_exceed   text NOT NULL DEFAULT 'downgrade' CHECK (on_exceed IN ('block','downgrade')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope, scope_id, period)
);

CREATE TABLE usage_ledger (
    id              bigserial PRIMARY KEY,
    created_at      timestamptz NOT NULL DEFAULT now(),
    user_id         uuid REFERENCES users(id) ON DELETE SET NULL,
    conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL,
    agent_id        uuid,
    agent_run_id    uuid,
    api_key_id      uuid,
    endpoint_id     text,
    model           text,
    task_class      text,
    policy_name     text,
    -- decision: {selector, candidates:[...], chosen, fallbacks_tried:[...], reason}
    decision        jsonb,
    input_tokens    int NOT NULL DEFAULT 0,
    output_tokens   int NOT NULL DEFAULT 0,
    cache_read_tokens  int NOT NULL DEFAULT 0,
    cache_write_tokens int NOT NULL DEFAULT 0,
    cost_usd        numeric(12,6) NOT NULL DEFAULT 0,
    latency_ms      int,
    ttft_ms         int,
    finish_reason   text,
    error           text
);
CREATE INDEX usage_ledger_user_time_idx ON usage_ledger(user_id, created_at DESC);
CREATE INDEX usage_ledger_endpoint_time_idx ON usage_ledger(endpoint_id, created_at DESC);
CREATE INDEX usage_ledger_agent_idx ON usage_ledger(agent_id, created_at DESC) WHERE agent_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- External API keys (so Claude Code / Cursor / etc. can use the platform)
-- ---------------------------------------------------------------------------
CREATE TABLE api_keys (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            text NOT NULL,
    prefix          text NOT NULL,                      -- first 8 chars, for display
    key_hash        bytea UNIQUE NOT NULL,
    scopes          text[] NOT NULL DEFAULT '{chat}',
    default_policy  text NOT NULL DEFAULT 'auto',
    budget_id       uuid REFERENCES budgets(id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_used_at    timestamptz,
    revoked_at      timestamptz
);
CREATE INDEX api_keys_user_idx ON api_keys(user_id);

-- updated_at trigger
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['projects','conversations','artifacts','providers','endpoints','routing_policies']
    LOOP
        EXECUTE format('CREATE TRIGGER %I_updated_at BEFORE UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION set_updated_at()', t, t);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS api_keys, usage_ledger, budgets, routing_policies, endpoints, providers,
    artifact_versions, artifacts, compactions, message_attachments, attachments, messages,
    conversations, project_members, projects, webauthn_sessions, sessions, magic_links,
    passkeys, invites, users CASCADE;
DROP FUNCTION IF EXISTS set_updated_at;
