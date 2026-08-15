-- +goose Up
CREATE TABLE agents (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id              uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    team_id             uuid REFERENCES teams(id) ON DELETE SET NULL,
    instance_key        text NOT NULL,
    environment         text NOT NULL,

    display_name        text NOT NULL,
    purpose             text NOT NULL DEFAULT '',
    lifecycle_state     text NOT NULL DEFAULT 'active',
    registration_source text NOT NULL DEFAULT 'self_registered',
    replica_count       integer NOT NULL DEFAULT 1,

    platform            jsonb NOT NULL DEFAULT '{}'::jsonb,
    hosting             jsonb NOT NULL DEFAULT '{}'::jsonb,
    source              jsonb NOT NULL DEFAULT '{}'::jsonb,
    telemetry_links     jsonb NOT NULL DEFAULT '[]'::jsonb,
    attributes          jsonb NOT NULL DEFAULT '{}'::jsonb,
    flags               text[] NOT NULL DEFAULT '{}',

    first_seen_at       timestamptz NOT NULL DEFAULT now(),
    last_heartbeat_at   timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT agents_environment_check
        CHECK (environment IN ('development','staging','production')),
    CONSTRAINT agents_lifecycle_check
        CHECK (lifecycle_state IN ('active','idle','stale','retired','quarantined')),
    CONSTRAINT agents_registration_source_check
        CHECK (registration_source IN ('self_registered','discovered','manual'))
);

-- The identity guarantee. Without this unique index, concurrent replica
-- registration produces duplicate rows and the inventory is worthless.
CREATE UNIQUE INDEX agents_identity_idx
    ON agents (org_id, environment, instance_key);

CREATE INDEX agents_team_idx      ON agents (org_id, team_id);
CREATE INDEX agents_lifecycle_idx ON agents (org_id, lifecycle_state);
CREATE INDEX agents_last_seen_idx ON agents (org_id, last_heartbeat_at DESC);

-- +goose Down
DROP TABLE agents;
