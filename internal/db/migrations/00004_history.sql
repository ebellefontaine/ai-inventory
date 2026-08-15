-- +goose Up
CREATE TABLE agent_revisions (
    id         bigserial PRIMARY KEY,
    agent_id   uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    snapshot   jsonb NOT NULL,
    actor      text NOT NULL,
    changed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX agent_revisions_agent_idx ON agent_revisions (agent_id, changed_at DESC);

CREATE TABLE agent_credentials (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id   uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    token_hash text NOT NULL UNIQUE,
    issued_at  timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    revoked_at timestamptz
);
CREATE INDEX agent_credentials_hash_idx ON agent_credentials (token_hash)
    WHERE revoked_at IS NULL;

CREATE TABLE audit_log (
    id          bigserial PRIMARY KEY,
    org_id      uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    actor       text NOT NULL,
    action      text NOT NULL,
    target_type text NOT NULL,
    target_id   text NOT NULL,
    before      jsonb,
    after       jsonb,
    at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_org_idx ON audit_log (org_id, at DESC);

-- History is append-only, and that is enforced here rather than in Go: a future
-- bug, a migration script, or an operator with psql must not be able to quietly
-- rewrite it.
--
-- The one exception is the deliberate hard delete of the parent row, which the
-- ON DELETE CASCADE clauses above declare. PostgreSQL implements that cascade as
-- an ordinary DELETE on the child table, so it fires this BEFORE DELETE trigger
-- too and a naive version blocks the parent delete entirely.
--
-- So the rule is: a row may be deleted only once its parent is already gone.
-- A cascade satisfies that (referential integrity runs it AFTER the parent row
-- has been removed, inside the same transaction); a bare
-- "DELETE FROM agent_revisions WHERE ..." against a live agent does not.
-- History therefore dies only with the record it describes.
--
-- Arguments are (parent table, foreign key column). search_path is pinned so a
-- temporary table cannot shadow the parent table and fake its absence.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION reject_mutation() RETURNS trigger
    SET search_path = pg_catalog, public
AS $$
DECLARE
    parent_alive boolean;
BEGIN
    IF TG_OP = 'DELETE' THEN
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I WHERE id = $1)', TG_ARGV[0])
            INTO parent_alive
            USING (to_jsonb(OLD) ->> TG_ARGV[1])::uuid;

        IF NOT parent_alive THEN
            RETURN OLD;
        END IF;
    END IF;

    RAISE EXCEPTION 'table % is append-only', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER agent_revisions_append_only
    BEFORE UPDATE OR DELETE ON agent_revisions
    FOR EACH ROW EXECUTE FUNCTION reject_mutation('agents', 'agent_id');

CREATE TRIGGER audit_log_append_only
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION reject_mutation('organizations', 'org_id');

-- +goose Down
DROP TRIGGER audit_log_append_only ON audit_log;
DROP TRIGGER agent_revisions_append_only ON agent_revisions;
DROP FUNCTION reject_mutation();
DROP TABLE audit_log;
DROP TABLE agent_credentials;
DROP TABLE agent_revisions;
