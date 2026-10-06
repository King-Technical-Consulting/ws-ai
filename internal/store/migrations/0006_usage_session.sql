-- +goose Up
-- The client's own session id on ledger rows: Claude Code sends
-- x-claude-code-session-id on every call, so a coding session's spend can
-- be grouped and shown. Free text, set only by the external API.
ALTER TABLE usage_ledger ADD COLUMN session_id text;
CREATE INDEX usage_ledger_session_idx ON usage_ledger(session_id, created_at DESC) WHERE session_id IS NOT NULL;

-- +goose Down
DROP INDEX usage_ledger_session_idx;
ALTER TABLE usage_ledger DROP COLUMN session_id;
