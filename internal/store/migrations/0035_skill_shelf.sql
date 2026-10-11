-- +goose Up
-- The skill shelf (OpenClaw plan, step 2). A member's approved skill
-- folders are kept once, with their findings and file hashes and who
-- approved them and when; an agent picks skills from the shelf. Keeping a
-- skill grants no tool, secret or network. File bytes are in the blob store
-- under sha256/<hex>, not in the row.
CREATE TABLE skills (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            text NOT NULL,                        -- slug, unique per member
    title           text NOT NULL,
    description     text NOT NULL DEFAULT '',
    instructions    text NOT NULL,                        -- the cleaned SKILL.md body
    has_scripts     boolean NOT NULL DEFAULT false,
    files           jsonb NOT NULL DEFAULT '[]',          -- inventory: path, kind, size, sha256, interpreter, refused, findings
    refused_secrets jsonb NOT NULL DEFAULT '[]',          -- front matter keys that named credentials (keys only)
    findings        jsonb NOT NULL DEFAULT '[]',
    warnings        jsonb NOT NULL DEFAULT '[]',
    approved_by     uuid REFERENCES users(id) ON DELETE SET NULL,
    approved_at     timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id, name)
);

CREATE TABLE agent_skills (
    agent_id  uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    skill_id  uuid NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    added_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, skill_id)
);
CREATE INDEX agent_skills_skill_idx ON agent_skills(skill_id);

-- Skills approved on an agent before the shelf (0033) move to their owner's
-- shelf and stay attached to the same agents.
INSERT INTO skills (owner_id, name, title, description, instructions, has_scripts, files, refused_secrets, findings, warnings, approved_by, approved_at)
SELECT DISTINCT ON (a.owner_id, k->>'name')
       a.owner_id, k->>'name', COALESCE(k->>'title', k->>'name'), COALESCE(k->>'description', ''),
       COALESCE(k->>'instructions', ''), COALESCE((k->>'has_scripts')::boolean, false),
       COALESCE(k->'files', '[]'::jsonb), COALESCE(k->'refused_secrets', '[]'::jsonb),
       COALESCE(k->'findings', '[]'::jsonb), COALESCE(k->'warnings', '[]'::jsonb),
       NULLIF(k->>'approved_by', '')::uuid, COALESCE(NULLIF(k->>'approved_at', '')::timestamptz, now())
  FROM agents a, jsonb_array_elements(a.skills) k
 WHERE k->>'name' IS NOT NULL
 ORDER BY a.owner_id, k->>'name', NULLIF(k->>'approved_at', '')::timestamptz DESC NULLS LAST;

INSERT INTO agent_skills (agent_id, skill_id)
SELECT DISTINCT a.id, s.id
  FROM agents a, jsonb_array_elements(a.skills) k
  JOIN skills s ON s.name = k->>'name'
 WHERE s.owner_id = a.owner_id;

ALTER TABLE agents DROP COLUMN skills;

-- +goose Down
ALTER TABLE agents ADD COLUMN skills jsonb NOT NULL DEFAULT '[]';
UPDATE agents a SET skills = COALESCE((
    SELECT jsonb_agg(jsonb_build_object(
        'name', s.name, 'title', s.title, 'description', s.description, 'instructions', s.instructions,
        'has_scripts', s.has_scripts, 'files', s.files, 'refused_secrets', s.refused_secrets,
        'findings', s.findings, 'warnings', s.warnings, 'approved_by', s.approved_by, 'approved_at', s.approved_at))
      FROM agent_skills x JOIN skills s ON s.id = x.skill_id WHERE x.agent_id = a.id), '[]'::jsonb);
DROP TABLE agent_skills;
DROP TABLE skills;
