-- +goose Up
-- Uploaded datasets (PLAN.md M12, cut a): a dataset can come from a file
-- the owner uploads instead of from conversations. The upload is kept in
-- the blob store (source_blob_key) and the training.build job maps it
-- into chat-format JSONL under the owner's column mapping.
ALTER TABLE datasets
    ADD COLUMN kind            text  NOT NULL DEFAULT 'chat' CHECK (kind IN ('chat', 'uploaded')),
    -- mapping: {mode, input, output, system, split, chunk_chars} (upload.go)
    ADD COLUMN mapping         jsonb NOT NULL DEFAULT '{}',
    ADD COLUMN source_blob_key text,
    ADD COLUMN source_name     text,
    -- report: what the last build found (files read and skipped, counts,
    -- warnings), shown beside the preview.
    ADD COLUMN report          jsonb NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE datasets
    DROP COLUMN report,
    DROP COLUMN source_name,
    DROP COLUMN source_blob_key,
    DROP COLUMN mapping,
    DROP COLUMN kind;
