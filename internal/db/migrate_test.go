package db_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
)

func TestMigrationsCreateOrgsAndTeams(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name IN ('organizations','teams')`,
	).Scan(&n)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}
