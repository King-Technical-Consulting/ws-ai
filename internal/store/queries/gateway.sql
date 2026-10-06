-- name: UpsertProvider :exec
INSERT INTO providers (id, kind, name, base_url, api_key_env, headers)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (id) DO UPDATE SET
  kind = EXCLUDED.kind, name = EXCLUDED.name, base_url = EXCLUDED.base_url,
  api_key_env = EXCLUDED.api_key_env, headers = EXCLUDED.headers;

-- name: ListProviders :many
SELECT * FROM providers ORDER BY id;

-- name: DeleteProvider :exec
DELETE FROM providers WHERE id = $1;

-- name: UpsertEndpoint :exec
INSERT INTO endpoints (id, provider_id, model_name, display_name, capabilities, pricing, throughput_class, latency_class, is_local, enabled, extra_body)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (id) DO UPDATE SET
  provider_id = EXCLUDED.provider_id, model_name = EXCLUDED.model_name, display_name = EXCLUDED.display_name,
  capabilities = EXCLUDED.capabilities, pricing = EXCLUDED.pricing,
  throughput_class = EXCLUDED.throughput_class, latency_class = EXCLUDED.latency_class,
  is_local = EXCLUDED.is_local, extra_body = EXCLUDED.extra_body;

-- name: ListEndpoints :many
SELECT * FROM endpoints ORDER BY id;

-- name: GetEndpoint :one
SELECT * FROM endpoints WHERE id = $1;

-- name: SetEndpointEnabled :exec
UPDATE endpoints SET enabled = $2 WHERE id = $1;

-- name: UpdateEndpointHealth :exec
UPDATE endpoints SET health_status = $2, health_checked_at = now(), health_error = $3, p50_latency_ms = $4, error_rate = $5
WHERE id = $1;

-- name: DeleteEndpoint :exec
DELETE FROM endpoints WHERE id = $1;

-- name: UpsertRoutingPolicy :one
INSERT INTO routing_policies (name, yaml, priority, enabled) VALUES ($1, $2, $3, $4)
ON CONFLICT (name) DO UPDATE SET yaml = EXCLUDED.yaml, priority = EXCLUDED.priority, enabled = EXCLUDED.enabled
RETURNING *;

-- name: ListRoutingPolicies :many
SELECT * FROM routing_policies WHERE enabled ORDER BY priority, name;

-- name: ListAllRoutingPolicies :many
SELECT * FROM routing_policies ORDER BY priority, name;

-- name: DeleteRoutingPolicy :exec
DELETE FROM routing_policies WHERE id = $1;

-- name: InsertUsage :one
INSERT INTO usage_ledger (
  user_id, conversation_id, agent_id, agent_run_id, api_key_id, endpoint_id, model, task_class, policy_name, decision,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, latency_ms, ttft_ms, finish_reason, error,
  session_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
RETURNING id;

-- name: SumUsageForUserSince :one
SELECT COALESCE(SUM(cost_usd), 0)::numeric AS cost_usd FROM usage_ledger WHERE user_id = $1 AND created_at >= $2;

-- name: SumUsageForAgentSince :one
SELECT COALESCE(SUM(cost_usd), 0)::numeric AS cost_usd FROM usage_ledger WHERE agent_id = $1 AND created_at >= $2;

-- name: SumUsageForAPIKeySince :one
SELECT COALESCE(SUM(cost_usd), 0)::numeric AS cost_usd FROM usage_ledger WHERE api_key_id = $1 AND created_at >= $2;

-- name: SumUsageSince :one
SELECT COALESCE(SUM(cost_usd), 0)::numeric AS cost_usd FROM usage_ledger WHERE created_at >= $1;

-- name: ListUsageForUser :many
SELECT * FROM usage_ledger WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3;

-- name: ListUsageAll :many
SELECT * FROM usage_ledger ORDER BY created_at DESC LIMIT $1 OFFSET $2;

-- name: UsageByEndpointSince :many
SELECT endpoint_id, count(*) AS calls,
       SUM(input_tokens)::bigint AS input_tokens, SUM(output_tokens)::bigint AS output_tokens,
       SUM(cost_usd)::numeric AS cost_usd, AVG(latency_ms)::int AS avg_latency_ms
FROM usage_ledger WHERE created_at >= $1 GROUP BY endpoint_id ORDER BY cost_usd DESC;

-- name: UsageByUserSince :many
SELECT user_id, count(*) AS calls, SUM(cost_usd)::numeric AS cost_usd
FROM usage_ledger WHERE created_at >= $1 GROUP BY user_id ORDER BY cost_usd DESC;

-- name: GetBudget :one
SELECT * FROM budgets WHERE scope = $1 AND scope_id IS NOT DISTINCT FROM $2 AND period = $3;

-- name: ListBudgetsForScope :many
SELECT * FROM budgets WHERE scope = $1 AND scope_id IS NOT DISTINCT FROM $2;

-- name: UpsertBudget :one
INSERT INTO budgets (scope, scope_id, period, limit_usd, on_exceed) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (scope, scope_id, period) DO UPDATE SET limit_usd = EXCLUDED.limit_usd, on_exceed = EXCLUDED.on_exceed
RETURNING *;

-- name: DeleteBudget :exec
DELETE FROM budgets WHERE id = $1;

-- name: ListAllBudgets :many
SELECT * FROM budgets ORDER BY scope, period;

-- name: GetRoutingPolicyByName :one
SELECT * FROM routing_policies WHERE name = $1;

-- name: DeleteRoutingPolicyByName :exec
DELETE FROM routing_policies WHERE name = $1;
