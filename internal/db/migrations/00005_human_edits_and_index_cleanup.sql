-- +goose Up

-- Both of these are partial indexes on a token_hash column that already
-- carries UNIQUE, and a unique constraint builds its own btree. Measured at
-- 50,000 seeded rows, the planner picked enrollment_tokens_token_hash_key for
-- the real lookup, and even a hypothetical "... AND revoked_at IS NULL" query
-- cost the same (8.43) with the same 4 buffer hits through either index. The
-- partial index bought 5.4 MB and write amplification for nothing.
--
-- It could not have helped the query we actually run, either: Resolve
-- deliberately omits a revoked_at IS NULL predicate, because filtering revoked
-- rows in SQL makes a revoked token indistinguishable from a nonexistent one
-- and destroys the ErrTokenRevoked / ErrTokenNotFound distinction the audit
-- trail depends on. These indexes were built for a query we must never write.
DROP INDEX enrollment_tokens_hash_idx;
DROP INDEX agent_credentials_hash_idx;

-- The columns an administrator has explicitly set. Self-registration must not
-- overwrite any column named here, until a human releases it back to
-- auto-management by removing the name (FUNCTIONAL.md, OWN-4).
--
-- The names are column names of this table, held as data rather than as a
-- column-per-flag, so the set of protectable fields can grow without a schema
-- change. Only self-reported columns belong here; server-managed columns
-- (lifecycle_state, last_heartbeat_at, updated_at, flags) are never protected
-- because no self-report sets them in the first place.
ALTER TABLE agents
    ADD COLUMN human_edited_fields text[] NOT NULL DEFAULT '{}';

COMMENT ON COLUMN agents.human_edited_fields IS
    'Names of columns an administrator has explicitly set. Self-registration '
    'preserves these values instead of overwriting them, until a human '
    'releases the column back to auto-management (OWN-4).';

-- +goose Down
ALTER TABLE agents DROP COLUMN human_edited_fields;

CREATE INDEX agent_credentials_hash_idx ON agent_credentials (token_hash)
    WHERE revoked_at IS NULL;

CREATE INDEX enrollment_tokens_hash_idx ON enrollment_tokens (token_hash)
    WHERE revoked_at IS NULL;
