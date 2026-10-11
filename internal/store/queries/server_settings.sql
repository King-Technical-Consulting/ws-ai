-- name: GetServerSetting :one
SELECT * FROM server_settings WHERE key = $1;

-- name: SetServerSetting :exec
INSERT INTO server_settings (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: DeleteServerSetting :exec
DELETE FROM server_settings WHERE key = $1;

-- What the verdict-routing classifier has cost since a time, summed on its
-- own task class so spawn_job's classify calls do not count against it.
-- name: SumRouteSpendSince :one
SELECT COALESCE(SUM(cost_usd), 0)::float8 AS cost_usd FROM usage_ledger WHERE task_class = 'route' AND created_at >= $1;
