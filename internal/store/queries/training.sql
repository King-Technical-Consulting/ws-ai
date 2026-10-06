-- Training flywheel (PLAN.md M10): consent, ratings, datasets, fine-tune
-- jobs and adapters.

-- name: SetTrainingConsent :exec
UPDATE users SET training_consent = $2 WHERE id = $1;

-- name: UpsertMessageRating :exec
INSERT INTO message_ratings (message_id, user_id, score, note) VALUES ($1, $2, $3, $4)
ON CONFLICT (message_id, user_id) DO UPDATE SET score = EXCLUDED.score, note = EXCLUDED.note, created_at = now();

-- name: DeleteMessageRating :exec
DELETE FROM message_ratings WHERE message_id = $1 AND user_id = $2;

-- name: ListRatingsForConversation :many
SELECT r.message_id, r.user_id, r.score, r.note, r.created_at
FROM message_ratings r JOIN messages m ON m.id = r.message_id
WHERE m.conversation_id = $1;

-- name: ListTrainingConversations :many
-- Conversations exportable into a dataset: the user opted in, the
-- conversation is not archived, it is in one of the modes (an empty list
-- takes every mode) and was last active since the cutoff.
SELECT c.* FROM conversations c JOIN users u ON u.id = c.user_id
WHERE u.training_consent AND c.archived_at IS NULL
  AND (cardinality(sqlc.arg(modes)::text[]) = 0 OR c.mode = ANY(sqlc.arg(modes)::text[]))
  AND c.updated_at >= sqlc.arg(since)
ORDER BY c.created_at
LIMIT sqlc.arg(max_conversations);

-- name: CreateDataset :one
INSERT INTO datasets (owner_id, name, task_class, filters) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetDataset :one
SELECT * FROM datasets WHERE id = $1;

-- name: ListDatasets :many
SELECT * FROM datasets ORDER BY created_at DESC LIMIT $1;

-- name: SetDatasetStatus :exec
UPDATE datasets SET status = $2, error = $3 WHERE id = $1;

-- name: FinishDataset :exec
UPDATE datasets SET status = 'ready', error = NULL, blob_key = $2, eval_blob_key = $3, bytes = $4, examples = $5, eval_examples = $6, built_at = now()
WHERE id = $1;

-- name: DeleteDataset :exec
DELETE FROM datasets WHERE id = $1;

-- name: CreateFinetuneJob :one
INSERT INTO finetune_jobs (owner_id, dataset_id, base_model, base_endpoint_id, adapter_name, config)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetFinetuneJob :one
SELECT * FROM finetune_jobs WHERE id = $1;

-- name: ListFinetuneJobs :many
SELECT * FROM finetune_jobs ORDER BY created_at DESC LIMIT $1;

-- name: ClaimFinetuneJob :one
UPDATE finetune_jobs SET status = 'running', started_at = now() WHERE id = $1 AND status = 'queued' RETURNING *;

-- name: SetFinetuneProgress :exec
UPDATE finetune_jobs SET progress = $2, log = $3 WHERE id = $1;

-- name: FinishFinetuneJob :exec
UPDATE finetune_jobs SET status = $2, error = $3, adapter_id = $4, progress = CASE WHEN $2 = 'done' THEN 1 ELSE progress END, log = $5, ended_at = now()
WHERE id = $1;

-- name: CancelFinetuneJob :exec
UPDATE finetune_jobs SET status = 'cancelled', ended_at = now() WHERE id = $1 AND status IN ('queued','running');

-- name: CreateAdapter :one
INSERT INTO adapters (name, base_model, base_endpoint_id, finetune_job_id, blob_key, bytes, endpoint_id)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: GetAdapter :one
SELECT * FROM adapters WHERE id = $1;

-- name: ListAdapters :many
SELECT * FROM adapters ORDER BY created_at DESC LIMIT $1;

-- name: SetAdapterEval :exec
UPDATE adapters SET eval_score = $2, baseline_score = $3, eval = $4, evaluated_at = now() WHERE id = $1;

-- name: SetAdapterPromoted :exec
UPDATE adapters SET promoted = $2 WHERE id = $1;

-- name: DeleteAdapter :exec
DELETE FROM adapters WHERE id = $1;
