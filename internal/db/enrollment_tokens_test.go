package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
)

// insertOrgAndTeam creates one organization and one team and returns their ids.
func insertOrgAndTeam(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) (orgID, teamID string) {
	t.Helper()

	err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ($1, $2) RETURNING id`,
		"Acme", slug,
	).Scan(&orgID)
	require.NoError(t, err)

	err = pool.QueryRow(ctx,
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, $2, $3) RETURNING id`,
		orgID, "Platform", "platform",
	).Scan(&teamID)
	require.NoError(t, err)

	return orgID, teamID
}

func TestMigrationsCreateEnrollmentTokens(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	t.Run("table exists", func(t *testing.T) {
		var n int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			 WHERE table_schema = 'public' AND table_name = 'enrollment_tokens'`,
		).Scan(&n)
		require.NoError(t, err)
		require.Equal(t, 1, n)
	})

	t.Run("environment check rejects invalid value", func(t *testing.T) {
		orgID, teamID := insertOrgAndTeam(t, ctx, pool, "env-check")

		_, err := pool.Exec(ctx,
			`INSERT INTO enrollment_tokens (org_id, team_id, environment, name, token_hash, created_by)
			 VALUES ($1, $2, 'wat', 'bad env', 'hash-env-check', 'admin@example.com')`,
			orgID, teamID,
		)
		require.Error(t, err, "environment 'wat' must be rejected")
		require.Contains(t, err.Error(), "enrollment_tokens_environment_check")

		// A valid environment on the same row must succeed.
		_, err = pool.Exec(ctx,
			`INSERT INTO enrollment_tokens (org_id, team_id, environment, name, token_hash, created_by)
			 VALUES ($1, $2, 'production', 'good env', 'hash-env-check', 'admin@example.com')`,
			orgID, teamID,
		)
		require.NoError(t, err)
	})

	t.Run("token hash is unique", func(t *testing.T) {
		orgID, teamID := insertOrgAndTeam(t, ctx, pool, "unique-hash")

		const insert = `INSERT INTO enrollment_tokens (org_id, team_id, environment, name, token_hash, created_by)
			 VALUES ($1, $2, 'staging', 'dup', 'hash-duplicate', 'admin@example.com')`

		_, err := pool.Exec(ctx, insert, orgID, teamID)
		require.NoError(t, err)

		_, err = pool.Exec(ctx, insert, orgID, teamID)
		require.Error(t, err, "duplicate token_hash must be rejected")
		require.Contains(t, err.Error(), "enrollment_tokens_token_hash_key")
	})

	t.Run("team delete is restricted while a token references it", func(t *testing.T) {
		orgID, teamID := insertOrgAndTeam(t, ctx, pool, "restrict-team")

		_, err := pool.Exec(ctx,
			`INSERT INTO enrollment_tokens (org_id, team_id, environment, name, token_hash, created_by)
			 VALUES ($1, $2, 'development', 'holds team', 'hash-restrict', 'admin@example.com')`,
			orgID, teamID,
		)
		require.NoError(t, err)

		_, err = pool.Exec(ctx, `DELETE FROM teams WHERE id = $1`, teamID)
		require.Error(t, err, "deleting a team with an enrollment token must be refused")
		require.Contains(t, err.Error(), "enrollment_tokens")

		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM teams WHERE id = $1`, teamID).Scan(&n))
		require.Equal(t, 1, n, "team must still exist")
	})
}
