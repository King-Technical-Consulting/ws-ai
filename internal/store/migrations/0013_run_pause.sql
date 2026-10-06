-- +goose Up
-- A run paused by hand from the agent monitor (PLAN.md M7): the runtime
-- stops between steps and the run keeps its place until it is resumed,
-- when it goes back to queued and the worker picks it up again.
ALTER TABLE agent_runs DROP CONSTRAINT IF EXISTS agent_runs_status_check;
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_status_check
    CHECK (status IN ('queued','running','paused_approval','paused_steer','paused_manual','done','failed','cancelled'));

-- +goose Down
UPDATE agent_runs SET status = 'cancelled', ended_at = now() WHERE status = 'paused_manual';
ALTER TABLE agent_runs DROP CONSTRAINT IF EXISTS agent_runs_status_check;
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_status_check
    CHECK (status IN ('queued','running','paused_approval','paused_steer','done','failed','cancelled'));
