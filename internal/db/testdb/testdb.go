// Package testdb provides a real PostgreSQL instance for tests.
//
// We do not mock the database. The core of this product is an idempotent
// upsert under concurrency, and a mock cannot tell you whether that upsert
// actually converges.
//
// One container is started per test binary and shared by every test in it.
// Isolation is not weakened by that sharing: each New call gets its own
// database inside the container, cloned from a template that already carries
// the migrations. Cloning a template is a file copy inside the server, which
// costs single-digit milliseconds against roughly 1.7 seconds to start a
// container.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ebellefontaine/ai-inventory/internal/db"
)

const (
	// adminDatabase is the database the container is created with. Nothing
	// under test uses it. It exists to give CREATE DATABASE a session to run
	// from, which can be neither the database being created nor the template
	// being copied.
	adminDatabase = "aiinv"

	// templateDatabase carries the migrated schema every test database is
	// cloned from.
	templateDatabase = "aiinv_template"
)

// server is the single PostgreSQL container this test binary uses.
type server struct {
	baseURL *url.URL      // connection string with the database name stripped
	admin   *pgxpool.Pool // sessions on adminDatabase, for CREATE DATABASE
}

var (
	startOnce sync.Once
	shared    *server
	startErr  error

	// cloneMu serializes CREATE DATABASE ... TEMPLATE. PostgreSQL refuses to
	// copy a template that another session is touching, and the counter that
	// names the clones needs the same protection. Tests do not currently run
	// in parallel; this keeps New correct if they ever do.
	cloneMu sync.Mutex
	cloneN  int
)

// New returns a pool onto a freshly migrated, empty database that no other
// test can see. The pool is closed when the test finishes.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	startOnce.Do(func() { shared, startErr = start(ctx) })
	require.NoError(t, startErr, "start shared postgres")

	name := clone(t)

	pool, err := pgxpool.New(ctx, shared.urlFor(name))
	require.NoError(t, err, "connect to %s", name)
	t.Cleanup(pool.Close)

	return pool
}

// start brings up the container and prepares the template database. It runs
// once per test binary.
func start(ctx context.Context) (*server, error) {
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase(adminDatabase),
		tcpostgres.WithUsername("aiinv"),
		tcpostgres.WithPassword("aiinv"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres container: %w", err)
	}
	// The container is deliberately not terminated from a t.Cleanup: it has to
	// outlive every test in the binary, and testdb cannot own a TestMain
	// because it is a helper imported by other packages' tests. testcontainers'
	// Ryuk reaper removes it when the test process exits.

	raw, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("container connection string: %w", err)
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	s := &server{baseURL: base}

	s.admin, err = pgxpool.New(ctx, s.urlFor(adminDatabase))
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", adminDatabase, err)
	}

	if _, err := s.admin.Exec(ctx,
		"CREATE DATABASE "+quoteIdent(templateDatabase)); err != nil {
		return nil, fmt.Errorf("create template database: %w", err)
	}
	if err := db.Migrate(ctx, s.urlFor(templateDatabase)); err != nil {
		return nil, fmt.Errorf("apply migrations to template: %w", err)
	}
	if err := s.sealTemplate(ctx); err != nil {
		return nil, err
	}

	return s, nil
}

// sealTemplate closes the template to connections for good. CREATE DATABASE
// ... TEMPLATE fails while any other session is connected to the source, and
// the migration's connections are only asked to close, not waited for. Barring
// new sessions and then evicting the ones left removes that race rather than
// hoping it does not fire. The template is never read directly afterwards, only
// copied, which does not need a connection.
func (s *server) sealTemplate(ctx context.Context) error {
	if _, err := s.admin.Exec(ctx,
		"ALTER DATABASE "+quoteIdent(templateDatabase)+" WITH ALLOW_CONNECTIONS false"); err != nil {
		return fmt.Errorf("bar connections to template: %w", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		var live int
		if err := s.admin.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE datname = $1`,
			templateDatabase).Scan(&live); err != nil {
			return fmt.Errorf("count template sessions: %w", err)
		}
		if live == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("template still has %d open session(s)", live)
		}
		if _, err := s.admin.Exec(ctx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			  WHERE datname = $1 AND pid <> pg_backend_pid()`,
			templateDatabase); err != nil {
			return fmt.Errorf("evict template sessions: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// clone copies the template into a database of its own and returns its name.
//
// The clone is not dropped afterwards. It lives inside a container that exists
// only for this test binary and is reaped with it, so dropping would buy
// nothing and would add a teardown step that can fail while the pool's server
// backends are still winding down.
func clone(t *testing.T) string {
	t.Helper()

	cloneMu.Lock()
	defer cloneMu.Unlock()

	cloneN++
	name := fmt.Sprintf("test_%04d", cloneN)

	_, err := shared.admin.Exec(context.Background(),
		"CREATE DATABASE "+quoteIdent(name)+" TEMPLATE "+quoteIdent(templateDatabase))
	require.NoError(t, err, "clone template into %s", name)

	return name
}

// urlFor points the container's connection string at one database.
func (s *server) urlFor(database string) string {
	u := *s.baseURL
	u.Path = "/" + database
	return u.String()
}

// quoteIdent quotes an identifier for interpolation into DDL. CREATE DATABASE
// takes no parameters, so the name has to be part of the statement text.
func quoteIdent(name string) string {
	return pgx.Identifier{name}.Sanitize()
}
