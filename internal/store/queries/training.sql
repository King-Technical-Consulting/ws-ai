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

-- name: CreateUploadedDataset :one
INSERT INTO datasets (owner_id, name, task_class, filters, kind, mapping, source_blob_key, source_name, bytes)
VALUES ($1, $2, $3, $4, 'uploaded', $5, $6, $7, $8) RETURNING *;

-- name: RequeueDataset :one
-- Sets a new mapping and queues a rebuild; only a finished build can be redone.
UPDATE datasets SET mapping = $2, status = 'queued', error = NULL
WHERE id = $1 AND kind = 'uploaded' AND status IN ('ready', 'failed') RETURNING *;

-- name: SetDatasetReport :exec
UPDATE datasets SET report = $2 WHERE id = $1;

-- name: CreateMergedDataset :one
-- The dataset a recipe job builds from the recipe's datasets (M12 cut c).
INSERT INTO datasets (owner_id, name, filters, kind, report, status)
VALUES ($1, $2, '{}', 'merged', $3, 'building') RETURNING *;

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
INSERT INTO finetune_jobs (owner_id, dataset_id, base_model, base_endpoint_id, adapter_name, config, recipe_id, spec)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING *;

-- name: SetFinetuneDataset :exec
-- A recipe job learns its dataset when it has merged the recipe's.
UPDATE finetune_jobs SET dataset_id = $2 WHERE id = $1;

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
INSERT INTO adapters (name, base_model, base_endpoint_id, finetune_job_id, blob_key, bytes, endpoint_id, recipe_id, system_prompt, rubric)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING *;

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

-- Recipes (M12 cut b).

-- name: CreateRecipe :one
INSERT INTO training_recipes (owner_id, name, base_model, base_endpoint_id, instructions, dataset_ids, target, config, spec_model)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING *;

-- name: GetRecipe :one
SELECT * FROM training_recipes WHERE id = $1;

-- name: ListRecipes :many
SELECT * FROM training_recipes ORDER BY created_at DESC LIMIT $1;

-- name: UpdateRecipe :one
-- The owner's fields; the spec and its bookkeeping have their own queries.
UPDATE training_recipes SET name = $2, base_model = $3, base_endpoint_id = $4, instructions = $5, dataset_ids = $6, target = $7, config = $8, spec_model = $9, updated_at = now()
WHERE id = $1 RETURNING *;

-- name: SetRecipeSpec :one
-- The owner's hand edit of the spec: the new spec and the keys now marked edited.
UPDATE training_recipes SET spec = $2, edited = $3, updated_at = now() WHERE id = $1 RETURNING *;

-- name: QueueRecipeSpec :one
-- Flesh out was asked for; refused while a run is queued or running.
UPDATE training_recipes SET spec_status = 'queued', spec_error = NULL, spec_model = $2, updated_at = now()
WHERE id = $1 AND spec_status NOT IN ('queued', 'running') RETURNING *;

-- name: ClaimRecipeSpec :one
UPDATE training_recipes SET spec_status = 'running', updated_at = now()
WHERE id = $1 AND spec_status = 'queued' RETURNING *;

-- name: FinishRecipeSpec :exec
UPDATE training_recipes SET spec = $2, edited = $3, spec_draft = $4, spec_used_model = $5, spec_status = 'ready', spec_error = NULL, spec_at = now(), updated_at = now()
WHERE id = $1;

-- name: FailRecipeSpec :exec
UPDATE training_recipes SET spec_status = 'failed', spec_error = $2, updated_at = now() WHERE id = $1;

-- name: DeleteRecipe :exec
DELETE FROM training_recipes WHERE id = $1;
