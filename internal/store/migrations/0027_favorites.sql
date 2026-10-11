-- +goose Up
-- Favorites and saved prompts (PLAN.md M13, cut 7). A favorite points at a
-- message, an artifact or a media job of any of the member's projects; the
-- reference is polymorphic, so there is no foreign key on it and listing
-- re-checks that the target still exists. Saved prompts are the member's
-- own reusable texts, offered in the composer's slash list.
CREATE TABLE favorites (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind            text NOT NULL CHECK (kind IN ('message','artifact','media')),
    ref_id          uuid NOT NULL,
    title           text NOT NULL DEFAULT '',
    snippet         text NOT NULL DEFAULT '',
    conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL,
    project_id      uuid REFERENCES projects(id) ON DELETE CASCADE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, kind, ref_id)
);
CREATE INDEX favorites_user_idx ON favorites(user_id, created_at DESC);

CREATE TABLE saved_prompts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       text NOT NULL,
    text       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX saved_prompts_user_idx ON saved_prompts(user_id, name);

-- +goose Down
DROP TABLE saved_prompts;
DROP TABLE favorites;
