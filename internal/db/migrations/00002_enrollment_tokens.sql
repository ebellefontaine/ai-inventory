-- +goose Up
CREATE TABLE enrollment_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    team_id      uuid NOT NULL REFERENCES teams(id) ON DELETE RESTRICT,
    environment  text NOT NULL,
    name         text NOT NULL,
    token_hash   text NOT NULL UNIQUE,
    created_by   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    revoked_at   timestamptz,
    last_used_at timestamptz,
    CONSTRAINT enrollment_tokens_environment_check
        CHECK (environment IN ('development','staging','production'))
);

CREATE INDEX enrollment_tokens_hash_idx ON enrollment_tokens (token_hash)
    WHERE revoked_at IS NULL;

-- +goose Down
DROP TABLE enrollment_tokens;
