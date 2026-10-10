-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: CreateUser :one
INSERT INTO users (email, display_name, role) VALUES ($1, $2, $3) RETURNING *;

-- name: ListUsers :many
SELECT * FROM users ORDER BY created_at;

-- SetUserDisabled parks or restores a member. The owner is never disabled,
-- whatever the caller says; an already-disabled user keeps the original
-- time. Sessions and API keys are refused while disabled_at is set
-- (GetSessionByTokenHash, GetAPIKeyByHash).
-- name: SetUserDisabled :one
UPDATE users SET disabled_at = CASE WHEN sqlc.arg(disabled)::boolean THEN COALESCE(disabled_at, now()) ELSE NULL END
WHERE id = $1 AND role <> 'owner'
RETURNING *;

-- name: SetDisplayName :exec
UPDATE users SET display_name = $2 WHERE id = $1;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: CreateInvite :one
INSERT INTO invites (email, token_hash, invited_by, role, expires_at)
VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: GetInviteByTokenHash :one
SELECT * FROM invites WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now();

-- name: UseInvite :exec
UPDATE invites SET used_at = now(), used_by = $2 WHERE id = $1;

-- name: ListInvites :many
SELECT * FROM invites ORDER BY created_at DESC;

-- name: GetInvite :one
SELECT * FROM invites WHERE id = $1;

-- DeletePendingInvite revokes an invite that was not accepted: the row goes
-- and with it the only copy of the token hash, so the link stops working.
-- name: DeletePendingInvite :execrows
DELETE FROM invites WHERE id = $1 AND used_at IS NULL;

-- name: CreatePasskey :one
INSERT INTO passkeys (user_id, credential_id, public_key, attestation_type, transports, aaguid, sign_count, backup_eligible, backup_state, name)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING *;

-- name: ListPasskeysByUser :many
SELECT * FROM passkeys WHERE user_id = $1 ORDER BY created_at;

-- name: GetPasskeyByCredentialID :one
SELECT * FROM passkeys WHERE credential_id = $1;

-- name: UpdatePasskeyAfterLogin :exec
UPDATE passkeys SET sign_count = $2, backup_state = $3, last_used_at = now() WHERE id = $1;

-- name: DeletePasskey :exec
DELETE FROM passkeys WHERE id = $1 AND user_id = $2;

-- name: CreateWebAuthnSession :one
INSERT INTO webauthn_sessions (user_id, kind, data, expires_at) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetWebAuthnSession :one
SELECT * FROM webauthn_sessions WHERE id = $1 AND expires_at > now();

-- name: DeleteWebAuthnSession :exec
DELETE FROM webauthn_sessions WHERE id = $1;

-- name: PurgeExpiredWebAuthnSessions :execrows
DELETE FROM webauthn_sessions WHERE expires_at <= now();

-- name: CreateMagicLink :one
INSERT INTO magic_links (user_id, token_hash, expires_at) VALUES ($1, $2, $3) RETURNING *;

-- name: ConsumeMagicLink :one
UPDATE magic_links SET used_at = now()
WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
RETURNING *;

-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, expires_at, user_agent, ip) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: GetSessionByTokenHash :one
SELECT s.*, u.email, u.display_name, u.role
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now() AND u.disabled_at IS NULL;

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = now() WHERE id = $1;

-- name: RevokeAllSessionsForUser :exec
UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL;

-- name: CreateAPIKey :one
INSERT INTO api_keys (user_id, name, prefix, key_hash, scopes, default_policy, budget_id)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: GetAPIKeyByHash :one
SELECT k.*, u.email, u.role
FROM api_keys k JOIN users u ON u.id = k.user_id
WHERE k.key_hash = $1 AND k.revoked_at IS NULL AND u.disabled_at IS NULL;

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now() WHERE id = $1;

-- name: ListAPIKeysByUser :many
SELECT * FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC;

-- name: RevokeAPIKey :exec
UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND user_id = $2;

-- name: PurgeMagicLinks :execrows
-- Expired links a day after they stopped working; used ones go with them.
DELETE FROM magic_links WHERE expires_at < now() - interval '1 day';

-- name: PurgeInvites :execrows
-- Invites that expired unused 30 days ago.
DELETE FROM invites WHERE used_at IS NULL AND expires_at < now() - interval '30 days';

-- name: PurgeSessions :execrows
-- Sessions that expired or were revoked 30 days ago (kept that long for the audit trail).
DELETE FROM sessions WHERE expires_at < now() - interval '30 days' OR revoked_at < now() - interval '30 days';

-- name: RevokeUserAPIKey :execrows
-- The owner revoking one of a person's keys: the key must be that person's.
UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;
