-- name: CreateRentalInstance :one
INSERT INTO rental_instances (provider, template, gpu, provider_instance_id, endpoint_id, hourly_usd, started_by)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: GetRentalInstance :one
SELECT * FROM rental_instances WHERE id = $1;

-- name: ListRentalInstances :many
SELECT * FROM rental_instances ORDER BY started_at DESC LIMIT $1;

-- name: ListOpenRentalInstances :many
SELECT * FROM rental_instances WHERE status IN ('provisioning','warming','ready','stopping') ORDER BY started_at;

-- name: OpenRentalInstanceForTemplate :one
SELECT * FROM rental_instances WHERE template = $1 AND status IN ('provisioning','warming','ready','stopping') ORDER BY started_at DESC LIMIT 1;

-- name: SetRentalStatus :exec
UPDATE rental_instances SET status = $2, error = COALESCE(sqlc.narg('error'), error), stop_reason = COALESCE(sqlc.narg('stop_reason'), stop_reason),
  stopped_at = CASE WHEN $2 IN ('stopped','failed') THEN COALESCE(stopped_at, now()) ELSE stopped_at END
WHERE id = $1;

-- name: SetRentalReady :exec
UPDATE rental_instances SET status = 'ready', base_url = $2, ready_at = COALESCE(ready_at, now()),
  hourly_usd = CASE WHEN sqlc.arg(hourly_usd)::numeric > 0 THEN sqlc.arg(hourly_usd)::numeric ELSE hourly_usd END WHERE id = $1;

-- name: SetRentalProviderID :exec
UPDATE rental_instances SET provider_instance_id = $2 WHERE id = $1;

-- name: SetRentalUsage :exec
UPDATE rental_instances SET hours_used = $2, billed_hours = $3, last_request_at = COALESCE(sqlc.narg('last_request_at'), last_request_at) WHERE id = $1;

-- name: SumRentalHoursSince :one
SELECT COALESCE(SUM(hours_used), 0)::float8 FROM rental_instances WHERE started_at >= $1 OR status IN ('provisioning','warming','ready','stopping');

-- name: LastUsageForEndpoint :one
SELECT COALESCE(MAX(created_at), '1970-01-01'::timestamptz)::timestamptz FROM usage_ledger WHERE endpoint_id = $1 AND task_class <> 'rental';
