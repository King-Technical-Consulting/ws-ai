-- +goose Up
-- Server-wide settings the owner edits in Admin, as key and JSON value.
-- First user: the daily ceiling on verdict-routing classifier spend
-- (key route_classifier_max_usd_day). An absent key means the default from
-- the environment.
CREATE TABLE server_settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE server_settings;
