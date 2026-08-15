// Package testdb provides a real PostgreSQL instance for tests.
//
// We do not mock the database. The core of this product is an idempotent
// upsert under concurrency, and a mock cannot tell you whether that upsert
// actually converges.
package testdb

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ebellefontaine/ai-inventory/internal/db"
)

// New starts a PostgreSQL container, applies migrations, and returns a pool.
// The container is terminated when the test finishes.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("aiinv"),
		tcpostgres.WithUsername("aiinv"),
		tcpostgres.WithPassword("aiinv"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "start postgres container")
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	require.NoError(t, db.Migrate(ctx, url), "apply migrations")

	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return pool
}
