-- +goose Up
-- Approved skill folders on an agent (OpenClaw plan, step 1b). Each entry
-- holds the SKILL.md instructions, the findings shown at approval, and an
-- inventory of files (path, kind, size, sha256). The file bytes live in the
-- content-addressed blob store under sha256/<hex>, not in this column.
-- A skill is untrusted text: nothing here grants a tool or a secret.
ALTER TABLE agents ADD COLUMN skills jsonb NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE agents DROP COLUMN skills;
