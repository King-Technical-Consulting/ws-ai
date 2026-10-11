-- +goose Up
-- Budgets, M7 third cut (PLAN.md M7): default budgets, a cap per agent run,
-- and a record of the warning mails.
--
-- A budget row with scope user or agent and a NULL scope_id is the default
-- for every member or agent that has no row of its own. The UNIQUE from
-- 0001 treated NULLs as distinct, so the global budget (also NULL) could be
-- inserted twice; the constraint is rebuilt with NULLS NOT DISTINCT after
-- the duplicates are folded into the newest row.
DELETE FROM budgets b USING budgets newer
WHERE b.scope = newer.scope AND b.period = newer.period
  AND b.scope_id IS NULL AND newer.scope_id IS NULL
  AND b.created_at < newer.created_at;
ALTER TABLE budgets DROP CONSTRAINT budgets_scope_scope_id_period_key;
ALTER TABLE budgets ADD CONSTRAINT budgets_scope_scope_id_period_key UNIQUE NULLS NOT DISTINCT (scope, scope_id, period);

-- A cap on one run's spend, in dollars; 0 is none. Copied onto the run
-- when it starts, as max_steps is, so the runtime needs no agent lookup.
ALTER TABLE agents ADD COLUMN max_run_usd numeric(12,4) NOT NULL DEFAULT 0;
ALTER TABLE agent_runs ADD COLUMN max_run_usd numeric(12,4) NOT NULL DEFAULT 0;

-- One row per warning sent: a budget at pct (80 or 100) of its limit for
-- that scope and period. The hourly budget.warn job sends again only when
-- the row is older than the period's window.
CREATE TABLE budget_warnings (
    scope       text NOT NULL,
    scope_id    uuid,
    period      text NOT NULL,
    pct         int NOT NULL,
    spent_usd   numeric(12,4) NOT NULL DEFAULT 0,
    limit_usd   numeric(12,4) NOT NULL DEFAULT 0,
    sent_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (scope, scope_id, period, pct)
);

-- +goose Down
DROP TABLE IF EXISTS budget_warnings;
ALTER TABLE agent_runs DROP COLUMN max_run_usd;
ALTER TABLE agents DROP COLUMN max_run_usd;
ALTER TABLE budgets DROP CONSTRAINT budgets_scope_scope_id_period_key;
ALTER TABLE budgets ADD CONSTRAINT budgets_scope_scope_id_period_key UNIQUE (scope, scope_id, period);
