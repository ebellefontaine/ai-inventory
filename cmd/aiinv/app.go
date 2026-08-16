package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ebellefontaine/ai-inventory/internal/config"
	"github.com/ebellefontaine/ai-inventory/internal/db"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/httpapi"
	"github.com/ebellefontaine/ai-inventory/internal/service"
	"github.com/ebellefontaine/ai-inventory/internal/worker"
)

// databaseConnectTimeout bounds the first connection attempt.
//
// pgxpool.New only parses the connection string; it does not connect, so a
// server built from it would come up cleanly against a database that is not
// there and report the failure one registration at a time. The explicit ping is
// what turns "the database is unreachable" into a start-up failure an
// orchestrator can see, and the timeout is what stops a start hanging forever on
// a host that swallows packets.
const databaseConnectTimeout = 10 * time.Second

// tokenCreatedBy is the actor recorded against a token minted from the command
// line. There is no operator identity to record yet — the CLI authenticates by
// holding the database connection string — so it records the surface rather
// than inventing a person. The admin API brings a real actor with it.
const tokenCreatedBy = "cli"

// app is the composition root: everything constructed once, wired together, and
// shared by whichever command asked for it.
//
// It exists so that the wiring can be tested without starting a process. serve
// is a shell around it, and every test in this package builds the same object a
// real start does rather than a rehearsal of it.
type app struct {
	cfg    config.Config
	logger *slog.Logger

	pool      *pgxpool.Pool
	handler   http.Handler
	worker    *worker.LifecycleWorker
	tokens    *service.TokenService
	bootstrap *service.BootstrapService

	// orgID is the single organisation this installation owns. It is resolved
	// at start rather than per request because there is exactly one, and a
	// lookup per operation would be a query answering a question that cannot
	// change while the process runs.
	orgID uuid.UUID

	closeOnce sync.Once
}

// newApp brings up everything below the transport: schema, pool, services,
// handler, worker.
//
// The order is not arbitrary. Migrations run before anything opens a pool, so a
// binary newer than its schema fixes that before it serves a request against
// columns that do not exist yet. The organisation is created before the handler
// is built, because a server that accepts registrations for an installation
// that has no organisation would fail every one of them at the foreign key.
func newApp(ctx context.Context, cfg config.Config, logger *slog.Logger) (*app, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// Migrating on every start is what makes self-hosting "one binary plus a
	// connection string" (NFR-7): there is no separate schema step to forget,
	// and no version skew between a deploy and a migration job.
	logger.InfoContext(ctx, "applying database migrations")
	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("configure database pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, databaseConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	logger.InfoContext(ctx, "connected to database")

	bootstrap := service.NewBootstrapService(pool)
	orgID, err := bootstrap.EnsureOrg(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}

	tokens := service.NewTokenService(pool)
	registration := service.NewRegistrationService(pool, tokens, cfg.HeartbeatIntervalSeconds)
	heartbeat := service.NewHeartbeatService(pool, cfg.HeartbeatIntervalSeconds)
	update := service.NewUpdateService(pool)

	return &app{
		cfg:       cfg,
		logger:    logger,
		pool:      pool,
		handler:   httpapi.NewServer(logger, registration, heartbeat, update).Handler(),
		tokens:    tokens,
		bootstrap: bootstrap,
		orgID:     orgID,
		worker: worker.New(pool, worker.Config{
			HeartbeatIntervalSeconds: cfg.HeartbeatIntervalSeconds,
			IdleAfterMissedBeats:     cfg.IdleAfterMissedBeats,
			StaleAfterDays:           cfg.StaleAfterDays,
		}),
	}, nil
}

// close releases what newApp acquired. It is idempotent because the shutdown
// path and a caller's defer both reach it on a normal exit, and a second
// pool.Close is not something to rely on being harmless.
func (a *app) close() {
	a.closeOnce.Do(func() {
		a.logger.Info("closing database pool")
		a.pool.Close()
	})
}

// createToken is the one path that mints an enrollment token, shared by the CLI
// and by the tests that act as an operator. The team is created on demand,
// because on a fresh install no team exists yet and requiring one to be
// declared first would make the first useful command the second one.
//
// The environment is validated before the team is touched. Mint would refuse it
// anyway, but only after this call had founded a team for a typo — a row that
// then exists forever and that nobody meant to create.
func (a *app) createToken(ctx context.Context, teamSlug, environment, name string) (service.MintResult, error) {
	env := domain.Environment(strings.ToLower(strings.TrimSpace(environment)))
	if !env.Valid() {
		return service.MintResult{}, fmt.Errorf(
			"create token: %w: %q (want development, staging or production)",
			service.ErrInvalidEnvironment, environment)
	}

	teamID, err := a.bootstrap.EnsureTeam(ctx, a.orgID, teamSlug)
	if err != nil {
		return service.MintResult{}, err
	}

	return a.tokens.Mint(ctx, service.MintInput{
		OrgID:       a.orgID,
		TeamID:      teamID,
		Environment: env,
		Name:        name,
		CreatedBy:   tokenCreatedBy,
	})
}
