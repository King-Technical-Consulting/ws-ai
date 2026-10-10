-- +goose Up
-- What the gateway has measured about each endpoint's speed, so a restart
-- starts from last week's numbers instead of the declared throughput_class.
-- One row per endpoint id, written after each counted reply; no foreign key,
-- because endpoint ids come and go with the seed and a stale row is harmless.
CREATE TABLE endpoint_throughput (
    endpoint_id    text PRIMARY KEY,
    tokens_per_sec double precision NOT NULL,
    ttft_ms        double precision NOT NULL,
    samples        integer NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE endpoint_throughput;
