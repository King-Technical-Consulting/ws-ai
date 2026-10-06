-- +goose Up
-- Rented GPUs (PLAN.md M9): one row per machine started on a provider
-- from a template (infra/rental/templates). The endpoint it serves is an
-- ordinary endpoints row (provider rental-<template>) that is enabled
-- when the machine answers and removed when it stops; the ledger gets a
-- usage_ledger row per instance-hour under task_class rental.
CREATE TABLE rental_instances (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider             text NOT NULL,                  -- runpod | lambda | vast
    template             text NOT NULL,                  -- template name
    gpu                  text NOT NULL DEFAULT '',
    provider_instance_id text,
    endpoint_id          text NOT NULL,                  -- the endpoints row this instance serves
    base_url             text,                           -- the OpenAI-compatible URL once known
    hourly_usd           numeric(10,4) NOT NULL DEFAULT 0,
    status               text NOT NULL DEFAULT 'provisioning'
                         CHECK (status IN ('provisioning','warming','ready','stopping','stopped','failed')),
    started_by           uuid REFERENCES users(id) ON DELETE SET NULL,
    started_at           timestamptz NOT NULL DEFAULT now(),
    ready_at             timestamptz,
    last_request_at      timestamptz,
    stopped_at           timestamptz,
    hours_used           real NOT NULL DEFAULT 0,
    billed_hours         real NOT NULL DEFAULT 0,        -- hours already written to the ledger
    stop_reason          text,
    error                text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rental_instances_started_idx ON rental_instances(started_at DESC);
CREATE INDEX rental_instances_open_idx ON rental_instances(status) WHERE status IN ('provisioning','warming','ready','stopping');
CREATE TRIGGER rental_instances_updated_at BEFORE UPDATE ON rental_instances FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE rental_instances;
