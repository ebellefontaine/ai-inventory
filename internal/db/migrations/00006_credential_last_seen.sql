-- +goose Up

-- The column is nullable and has no default on purpose. A credential is issued
-- at registration and may never be used — the process it was cut for might have
-- failed to start — and that gap is precisely what an operator needs to see.
-- DEFAULT now() would have manufactured a heartbeat that never happened, which
-- is the one thing an inventory built on observed evidence must not do.
ALTER TABLE agent_credentials ADD COLUMN last_seen_at timestamptz;

COMMENT ON COLUMN agent_credentials.last_seen_at IS
    'When this credential last heartbeated; NULL means it never has. '
    'Registration issues one credential per call, so counting an agent''s '
    'credentials whose last_seen_at falls inside the liveness window is how '
    'replica_count is derived from evidence rather than taken on the agent''s '
    'word.';

-- Supports the replica count recomputed on every heartbeat: for one agent_id,
-- count the unrevoked credentials whose last_seen_at is recent enough.
--
-- Unlike the two partial indexes 00005 dropped, this one duplicates nothing.
-- agent_credentials.agent_id is a foreign key and PostgreSQL does not index
-- those, so the only alternative plan is a sequential scan over every
-- credential in the installation, run once per heartbeat per agent — on the
-- one code path that is required to stay cheap.
--
-- The revoked_at predicate mirrors the query's own filter and keeps revoked
-- credentials, which only ever accumulate, out of the index entirely.
CREATE INDEX agent_credentials_liveness_idx
    ON agent_credentials (agent_id, last_seen_at DESC)
    WHERE revoked_at IS NULL;

-- +goose Down
DROP INDEX agent_credentials_liveness_idx;
ALTER TABLE agent_credentials DROP COLUMN last_seen_at;
