-- +goose Up
-- Long-lived agents (PLAN.md M7). An agent belongs to a project so its
-- runs have somewhere to put their conversations; a trigger keeps its
-- schedule state here so the worker's minute tick can find what is due.
ALTER TABLE agents
    ADD COLUMN project_id   uuid REFERENCES projects(id) ON DELETE CASCADE,
    ADD COLUMN last_run_at  timestamptz;
CREATE INDEX agents_project_idx ON agents(project_id);

ALTER TABLE agent_triggers
    ADD COLUMN name         text NOT NULL DEFAULT '',
    ADD COLUMN next_run_at  timestamptz,
    ADD COLUMN last_run_at  timestamptz,
    ADD COLUMN last_error   text;
CREATE INDEX agent_triggers_due_idx ON agent_triggers(next_run_at) WHERE enabled AND kind = 'cron';

-- +goose Down
DROP INDEX IF EXISTS agent_triggers_due_idx;
ALTER TABLE agent_triggers DROP COLUMN name, DROP COLUMN next_run_at, DROP COLUMN last_run_at, DROP COLUMN last_error;
DROP INDEX IF EXISTS agents_project_idx;
ALTER TABLE agents DROP COLUMN project_id, DROP COLUMN last_run_at;
