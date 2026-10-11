-- name: UpsertSkill :one
INSERT INTO skills (owner_id, name, title, description, instructions, has_scripts, files, refused_secrets, findings, warnings, approved_by, approved_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
ON CONFLICT (owner_id, name) DO UPDATE SET
  title = EXCLUDED.title, description = EXCLUDED.description, instructions = EXCLUDED.instructions,
  has_scripts = EXCLUDED.has_scripts, files = EXCLUDED.files, refused_secrets = EXCLUDED.refused_secrets,
  findings = EXCLUDED.findings, warnings = EXCLUDED.warnings, approved_by = EXCLUDED.approved_by,
  approved_at = now(), updated_at = now()
RETURNING *;

-- name: GetSkill :one
SELECT * FROM skills WHERE id = $1;

-- name: GetSkillByName :one
SELECT * FROM skills WHERE owner_id = $1 AND name = $2;

-- name: ListSkills :many
SELECT s.*, (SELECT count(*) FROM agent_skills x WHERE x.skill_id = s.id)::int AS agent_count
FROM skills s WHERE s.owner_id = $1 ORDER BY s.title, s.name;

-- name: DeleteSkill :exec
DELETE FROM skills WHERE id = $1 AND owner_id = $2;

-- name: ListSkillsForAgent :many
SELECT s.* FROM skills s JOIN agent_skills x ON x.skill_id = s.id
WHERE x.agent_id = $1 ORDER BY x.added_at, s.name;

-- name: AttachSkillToAgent :exec
INSERT INTO agent_skills (agent_id, skill_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: DetachSkillFromAgent :exec
DELETE FROM agent_skills WHERE agent_id = $1 AND skill_id = $2;

-- name: CountSkillsForAgent :one
SELECT count(*)::int FROM agent_skills WHERE agent_id = $1;

-- name: ListAgentsUsingSkill :many
SELECT a.id, a.name FROM agents a JOIN agent_skills x ON x.agent_id = a.id WHERE x.skill_id = $1 ORDER BY a.name;
