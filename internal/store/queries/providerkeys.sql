-- name: UpsertUserProviderKey :exec
INSERT INTO user_provider_keys (user_id, provider_id, sealed, last4)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, provider_id) DO UPDATE SET sealed = EXCLUDED.sealed, last4 = EXCLUDED.last4, updated_at = now();

-- name: DeleteUserProviderKey :exec
DELETE FROM user_provider_keys WHERE user_id = $1 AND provider_id = $2;

-- name: GetUserProviderKey :one
SELECT * FROM user_provider_keys WHERE user_id = $1 AND provider_id = $2;

-- name: ListUserProviderKeys :many
-- Metadata only; the sealed key is never listed.
SELECT provider_id, last4, created_at, updated_at FROM user_provider_keys WHERE user_id = $1 ORDER BY provider_id;

-- name: ListUserProviderKeysSealed :many
-- For the gateway only: the sealed keys of one user, opened in memory per call.
SELECT provider_id, sealed FROM user_provider_keys WHERE user_id = $1;

-- name: SetUserSharedProviderKeys :exec
UPDATE users SET shared_provider_keys = $2 WHERE id = $1;
