-- +goose Up
-- Step-up (PLAN.md M5): a sensitive action from outside the home networks needs
-- a passkey assertion made in the last few minutes. The assertion is
-- remembered on the session: one tap unlocks the gated actions for the window
-- (WS_STEPUP_WINDOW). A session starts with none, except one made by a passkey
-- login, where the assertion has just happened.
ALTER TABLE sessions ADD COLUMN stepped_up_at timestamptz;

-- The assertion ceremony keeps its state between begin and finish like login
-- and registration do, under its own kind.
ALTER TABLE webauthn_sessions DROP CONSTRAINT webauthn_sessions_kind_check;
ALTER TABLE webauthn_sessions ADD CONSTRAINT webauthn_sessions_kind_check CHECK (kind IN ('register', 'login', 'stepup'));

-- +goose Down
DELETE FROM webauthn_sessions WHERE kind = 'stepup';
ALTER TABLE webauthn_sessions DROP CONSTRAINT webauthn_sessions_kind_check;
ALTER TABLE webauthn_sessions ADD CONSTRAINT webauthn_sessions_kind_check CHECK (kind IN ('register', 'login'));
ALTER TABLE sessions DROP COLUMN stepped_up_at;
