# AI Inventory Phase 1 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Ship a self-hostable AI agent inventory where agents self-register via HTTP or MCP, ownership is inherited from scoped enrollment tokens, and a CIO can export a defensible list.

**Architecture:** One Go binary containing the HTTP API, an MCP server, an embedded React SPA, and background workers — all thin adapters over a single service layer, backed by PostgreSQL. Agents are keyed on `(org, environment, instance_key)` and registration is an idempotent upsert, so replicas and redeploys never duplicate records.

**Tech Stack:** Go 1.24+, PostgreSQL 16, `pgx/v5`, `goose` (embedded migrations), `testcontainers-go`, `testify`, official MCP Go SDK, Vite + React + TypeScript, GitHub Actions, goreleaser.

**Reference documents:** [SYSTEM.md](../../SYSTEM.md) · [FUNCTIONAL.md](../../FUNCTIONAL.md) · [design doc](2026-08-15-ai-inventory-design.md)

---

## Scope of this document

This plan covers **Milestones 0–3** in full step-by-step detail: repository foundation, data layer, service layer, and the HTTP API with its contract suite. That is the load-bearing half — every decision in it is expensive to change later.

**Milestones 4–8 are specified at task level, not step level, and each will be expanded into its own plan document when reached.** This is deliberate. A fully-detailed 45-task plan written before any code exists is wrong by task 20, and pretending otherwise produces a document that gets ignored rather than followed.

---

## Conventions for every task

- **TDD is not optional.** Write the failing test, run it and watch it fail, write the minimum to pass, run it and watch it pass, commit. If a step says "verify it fails," you must actually run it and see the failure — a test that has never failed has never been proven to test anything.
- **Commit after every task.** Small commits. Every commit message ends with the DCO sign-off (`git commit -s`).
- **Never use mocks for PostgreSQL.** Use the testcontainers harness from Task 9. Concurrent upsert behaviour is exactly what a mock hides, and that behaviour is the core of this product.
- Run `make check` (lint + test) before every commit.

---

# Milestone 0 — Repository foundation

### Task 1: Go module and directory skeleton

**Files:**
- Create: `go.mod`, `Makefile`, `cmd/aiinv/main.go`
- Create empty dirs with `.gitkeep`: `internal/config`, `internal/db/migrations`, `internal/domain`, `internal/service`, `internal/httpapi`, `internal/mcpapi`, `internal/worker`, `internal/auth`, `web`, `sdk/python`, `sdk/typescript`, `spec/conformance`

**Step 1: Initialise the module**

```bash
go mod init github.com/ebellefontaine/ai-inventory
```

This string appears in every import path in the repo. It is fixed for this project — do not substitute anything else.

**Step 2: Create `cmd/aiinv/main.go`**

```go
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "aiinv: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("aiinv")
	return nil
}
```

**Step 3: Create `Makefile`**

```makefile
.PHONY: build test lint check run db-up db-down

build:
	go build -o bin/aiinv ./cmd/aiinv

test:
	go test ./... -race -count=1

lint:
	go vet ./...
	gofmt -l -d .

check: lint test

run: build
	./bin/aiinv

db-up:
	docker compose up -d postgres

db-down:
	docker compose down -v
```

**Step 4: Verify it builds and runs**

Run: `make build && ./bin/aiinv`
Expected: prints `aiinv`, exit 0.

**Step 5: Commit**

```bash
git add go.mod Makefile cmd/ internal/ web/ sdk/ spec/
git commit -s -m "chore: scaffold go module and directory layout"
```

---

### Task 2: Licenses

The split is deliberate and is documented in [SYSTEM.md §10](../../SYSTEM.md#10-licensing-and-business-model): AGPL-3.0 for the server so a competitor cannot run it as a hosted service without publishing changes, Apache-2.0 for the SDKs so nobody hesitates to embed a client in a production agent.

**Files:**
- Create: `LICENSE` — verbatim AGPL-3.0 text from https://www.gnu.org/licenses/agpl-3.0.txt
- Create: `sdk/python/LICENSE` and `sdk/typescript/LICENSE` — verbatim Apache-2.0 text from https://www.apache.org/licenses/LICENSE-2.0.txt
- Create: `sdk/README.md`

**Step 1: Fetch the license texts verbatim**

Do not paraphrase, retype, or summarise license text. Download it.

**Step 2: Create `sdk/README.md`**

```markdown
# AI Inventory SDKs

The SDKs and MCP client in this directory are licensed **Apache-2.0**, not
AGPL-3.0. They are intended to be embedded directly in your agents, and a
copyleft client library would make that impossible for most organisations.

The server, UI, and connectors in the rest of this repository are AGPL-3.0.
See the root LICENSE file.
```

**Step 3: Add a license section to `README.md`** (created in Task 3) stating both licenses and which directories each covers.

**Step 4: Commit**

```bash
git add LICENSE sdk/
git commit -s -m "docs: add AGPL-3.0 server and Apache-2.0 SDK licenses"
```

---

### Task 3: Product documentation

`SYSTEM.md`, `FUNCTIONAL.md`, and the design doc are already drafted in the working tree. This task reviews and commits them, and writes the README.

**Files:**
- Create: `README.md`
- Commit existing: `SYSTEM.md`, `FUNCTIONAL.md`, `docs/plans/2026-08-15-ai-inventory-design.md`

**Step 1: Read the three existing documents.** Confirm they still describe what you are about to build. If any decision has changed, change the document before writing code against it.

**Step 2: Write `README.md`** covering, in this order: one-sentence description, the problem in three sentences, quickstart (`docker compose up`, mint a token, register an agent), the three-line SDK example, links to SYSTEM.md and FUNCTIONAL.md, the license split, and a contributing pointer.

Keep it short. The README's job is to get someone to a running instance in under five minutes; SYSTEM.md carries the depth.

**Step 3: Commit**

```bash
git add README.md SYSTEM.md FUNCTIONAL.md docs/
git commit -s -m "docs: add system spec, functional requirements, and design"
```

---

### Task 4: Contribution and security policy

**Files:**
- Create: `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `.github/pull_request_template.md`

**Step 1: `CONTRIBUTING.md`** — must cover: DCO sign-off is required (`git commit -s`) and why we chose DCO over a CLA, how to run tests locally, the TDD expectation, the commit message convention, and that server changes are AGPL while SDK changes are Apache.

**Step 2: `SECURITY.md`** — a private disclosure address, a response-time commitment, and supported versions. State explicitly that AI Inventory never handles prompts, completions, or end-user data ([SYSTEM.md §7](../../SYSTEM.md#7-privacy-boundary)), because that materially changes a reporter's threat model.

**Step 3: `CODE_OF_CONDUCT.md`** — Contributor Covenant 2.1, verbatim, with a real contact address filled in.

**Step 4: Commit**

```bash
git add CONTRIBUTING.md SECURITY.md CODE_OF_CONDUCT.md .github/
git commit -s -m "docs: add contribution, security, and conduct policies"
```

---

### Task 5: Local PostgreSQL

**Files:**
- Create: `docker-compose.yml`, `.env.example`

**Step 1: Create `docker-compose.yml`**

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: aiinv
      POSTGRES_PASSWORD: aiinv
      POSTGRES_DB: aiinv
    ports:
      - "5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U aiinv"]
      interval: 5s
      timeout: 5s
      retries: 10
    volumes:
      - pgdata:/var/lib/postgresql/data

volumes:
  pgdata:
```

**Step 2: Create `.env.example`**

```
AIINV_DATABASE_URL=postgres://aiinv:aiinv@localhost:5432/aiinv?sslmode=disable
AIINV_LISTEN_ADDR=:8080
AIINV_LOG_LEVEL=info
```

`.env` is already in `.gitignore`. Never commit a real `.env`.

**Step 3: Verify**

Run: `make db-up && docker compose ps`
Expected: `postgres` reports healthy within ~10 seconds.

**Step 4: Commit**

```bash
git add docker-compose.yml .env.example
git commit -s -m "chore: add local postgres via docker compose"
```

---

### Task 6: Continuous integration

**Files:**
- Create: `.github/workflows/ci.yml`

**Step 1: Write the workflow.** It must run on push and pull request, and must:

- Set up Go from the version in `go.mod`
- Run `make lint`
- Run `make test` with a PostgreSQL service container available (testcontainers needs a working Docker daemon; GitHub-hosted runners have one)
- Fail on any `gofmt` diff

**Step 2: Verify.** Push the branch and confirm the workflow goes green before continuing. A CI pipeline added after the code exists is a CI pipeline that gets skipped.

**Step 3: Commit**

```bash
git add .github/workflows/ci.yml
git commit -s -m "ci: add lint and test workflow"
```

---

# Milestone 1 — Data layer

### Task 7: Configuration

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`

**Step 1: Write the failing test**

```go
package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadRequiresDatabaseURL(t *testing.T) {
	_, err := Load(func(string) (string, bool) { return "", false })
	require.ErrorContains(t, err, "AIINV_DATABASE_URL")
}

func TestLoadAppliesDefaults(t *testing.T) {
	env := map[string]string{"AIINV_DATABASE_URL": "postgres://x"}
	cfg, err := Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err)
	require.Equal(t, ":8080", cfg.ListenAddr)
	require.Equal(t, 60, cfg.HeartbeatIntervalSeconds)
}
```

The `func(string) (string, bool)` parameter is `os.LookupEnv`'s signature. Injecting it keeps the tests from mutating global process state, which is what makes them safe under `-race` and parallel execution.

**Step 2: Run and verify it fails**

Run: `go test ./internal/config/ -v`
Expected: FAIL — `undefined: Load`.

**Step 3: Implement**

```go
package config

import "fmt"

type Config struct {
	DatabaseURL              string
	ListenAddr               string
	LogLevel                 string
	HeartbeatIntervalSeconds int
	IdleAfterMissedBeats     int
	StaleAfterDays           int
}

type LookupFunc func(string) (string, bool)

func Load(lookup LookupFunc) (Config, error) {
	cfg := Config{
		ListenAddr:               ":8080",
		LogLevel:                 "info",
		HeartbeatIntervalSeconds: 60,
		IdleAfterMissedBeats:     3,
		StaleAfterDays:           7,
	}

	url, ok := lookup("AIINV_DATABASE_URL")
	if !ok || url == "" {
		return Config{}, fmt.Errorf("AIINV_DATABASE_URL is required")
	}
	cfg.DatabaseURL = url

	if v, ok := lookup("AIINV_LISTEN_ADDR"); ok && v != "" {
		cfg.ListenAddr = v
	}
	if v, ok := lookup("AIINV_LOG_LEVEL"); ok && v != "" {
		cfg.LogLevel = v
	}
	return cfg, nil
}
```

**Step 4: Run and verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS, both tests.

**Step 5: Commit**

```bash
git add internal/config/
git commit -s -m "feat: add configuration loading with defaults"
```

---

### Task 8: Embedded migrations

**Files:**
- Create: `internal/db/migrate.go`, `internal/db/migrations/.gitkeep`

**Step 1: Add dependencies**

```bash
go get github.com/jackc/pgx/v5/pgxpool github.com/pressly/goose/v3 github.com/stretchr/testify
```

**Step 2: Write `internal/db/migrate.go`**

```go
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies all pending migrations. It runs on every start, which is
// what makes self-hosting a single binary plus a connection string (NFR-7).
func Migrate(ctx context.Context, databaseURL string) error {
	conn, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open db for migration: %w", err)
	}
	defer conn.Close()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
```

**Step 3: Verify it compiles**

Run: `go build ./...`
Expected: success. There are no migrations yet; `embed` requires at least the directory to exist, which `.gitkeep` provides. If `go:embed` complains about no matching files, add the first migration in Task 10 before building.

**Step 4: Commit**

```bash
git add internal/db/ go.mod go.sum
git commit -s -m "feat: add embedded migration runner"
```

---

### Task 9: Test harness with real PostgreSQL

This task is the foundation for every test that follows. Get it right.

**Files:**
- Create: `internal/db/testdb/testdb.go`

**Step 1: Add the dependency**

```bash
go get github.com/testcontainers/testcontainers-go github.com/testcontainers/testcontainers-go/modules/postgres
```

**Step 2: Implement the harness**

```go
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
```

**Step 3: Verify.** You cannot test this until a migration exists. Task 10 provides one; return here and confirm the harness starts if it fails then.

**Step 4: Commit**

```bash
git add internal/db/testdb/
git commit -s -m "test: add real postgres test harness via testcontainers"
```

---

### Task 10: Migration — organisations and teams

Every table carries `org_id` from day one, even though a self-hosted install has exactly one organisation. Retrofitting tenancy is a rewrite; carrying an unused column is free.

**Files:**
- Create: `internal/db/migrations/00001_orgs_and_teams.sql`
- Create: `internal/db/migrate_test.go`

**Step 1: Write the failing test**

```go
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
```

**Step 2: Run and verify it fails**

Run: `go test ./internal/db/ -run TestMigrationsCreate -v`
Expected: FAIL — the tables do not exist (count is 0).

**Step 3: Write the migration**

```sql
-- +goose Up
CREATE TABLE organizations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL,
    slug        text NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE teams (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name         text NOT NULL,
    slug         text NOT NULL,
    owner_emails text[] NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, slug)
);

CREATE INDEX teams_org_id_idx ON teams (org_id);

-- +goose Down
DROP TABLE teams;
DROP TABLE organizations;
```

**Step 4: Run and verify it passes**

Run: `go test ./internal/db/ -run TestMigrationsCreate -v`
Expected: PASS. First run pulls the postgres image and may take a minute.

**Step 5: Commit**

```bash
git add internal/db/
git commit -s -m "feat: add organizations and teams schema"
```

---

### Task 11: Migration — enrollment tokens, and token hashing

Tokens are stored as hashes only and displayed exactly once ([FUNCTIONAL.md OWN-1](../../FUNCTIONAL.md#own-1--mint-scoped-enrollment-tokens)).

**Files:**
- Create: `internal/db/migrations/00002_enrollment_tokens.sql`
- Create: `internal/auth/token.go`, `internal/auth/token_test.go`

**Step 1: Write the failing test**

```go
package auth

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerateProducesVerifiableToken(t *testing.T) {
	plain, hash, err := GenerateToken("aiinv_et")
	require.NoError(t, err)
	require.True(t, len(plain) > 32)
	require.Contains(t, plain, "aiinv_et_")
	require.NotContains(t, hash, plain, "hash must not embed the plaintext")
	require.True(t, VerifyToken(plain, hash))
}

func TestVerifyRejectsWrongToken(t *testing.T) {
	_, hash, err := GenerateToken("aiinv_et")
	require.NoError(t, err)
	require.False(t, VerifyToken("aiinv_et_wrong", hash))
}

func TestGenerateIsUnique(t *testing.T) {
	a, _, _ := GenerateToken("aiinv_et")
	b, _, _ := GenerateToken("aiinv_et")
	require.NotEqual(t, a, b)
}
```

**Step 2: Run and verify it fails**

Run: `go test ./internal/auth/ -v`
Expected: FAIL — `undefined: GenerateToken`.

**Step 3: Implement**

```go
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// GenerateToken returns the plaintext token (shown to the user exactly once)
// and the hash to store. The plaintext is never persisted anywhere.
//
// SHA-256 is correct here rather than bcrypt or argon2: these are 256-bit
// random secrets, not user-chosen passwords, so there is nothing to brute
// force and we need verification to be fast enough to run on every request.
func GenerateToken(prefix string) (plain, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	plain = fmt.Sprintf("%s_%s", prefix, base64.RawURLEncoding.EncodeToString(raw))
	return plain, HashToken(plain), nil
}

func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// VerifyToken compares in constant time to avoid leaking the hash by timing.
func VerifyToken(plain, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(HashToken(plain)), []byte(hash)) == 1
}
```

**Step 4: Run and verify it passes**

Run: `go test ./internal/auth/ -v`
Expected: PASS, all three.

**Step 5: Write the migration `00002_enrollment_tokens.sql`**

```sql
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
```

**Step 6: Commit**

```bash
git add internal/auth/ internal/db/migrations/
git commit -s -m "feat: add enrollment tokens with hashed storage"
```

---

### Task 12: Migration — agents, and the idempotent upsert

**This is the most important task in the plan.** If registration is not idempotent on `(org, environment, instance_key)`, the inventory fills with duplicates within a week and the product is worthless. Test it under real concurrency.

**Files:**
- Create: `internal/db/migrations/00003_agents.sql`
- Create: `internal/db/agents.go`, `internal/db/agents_test.go`

**Step 1: Write the failing tests**

```go
package db_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/db"
	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
)

func TestUpsertAgentIsIdempotent(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewAgentStore(pool)
	org, team := seedOrgAndTeam(t, pool)

	in := db.UpsertAgentInput{
		OrgID: org, TeamID: team,
		Environment: "production",
		InstanceKey: "lambda:arn:aws:lambda:us-east-1:123:function:invoice-triage",
		DisplayName: "Invoice triage",
		Purpose:     "Triages AP invoices",
	}

	first, err := store.Upsert(ctx, in)
	require.NoError(t, err)
	require.True(t, first.Created)

	second, err := store.Upsert(ctx, in)
	require.NoError(t, err)
	require.False(t, second.Created, "second registration must update, not insert")
	require.Equal(t, first.ID, second.ID, "same instance key must map to same agent")

	require.Equal(t, 1, countAgents(t, pool))
}

// Twenty replicas of one deployment starting simultaneously must converge to
// exactly one record, with no error surfaced to any caller.
func TestUpsertAgentConvergesUnderConcurrency(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewAgentStore(pool)
	org, team := seedOrgAndTeam(t, pool)

	in := db.UpsertAgentInput{
		OrgID: org, TeamID: team,
		Environment: "production",
		InstanceKey: "k8s:default/invoice-triage",
		DisplayName: "Invoice triage",
		Purpose:     "Triages AP invoices",
	}

	const replicas = 20
	var wg sync.WaitGroup
	errs := make([]error, replicas)
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.Upsert(ctx, in)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "replica %d must not error", i)
	}
	require.Equal(t, 1, countAgents(t, pool))
}

// Same instance key in a different environment is a different agent.
func TestUpsertAgentScopesByEnvironment(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewAgentStore(pool)
	org, team := seedOrgAndTeam(t, pool)

	base := db.UpsertAgentInput{
		OrgID: org, TeamID: team,
		InstanceKey: "repo:github.com/acme/invoice#main.py",
		DisplayName: "Invoice triage",
		Purpose:     "Triages AP invoices",
	}

	prod := base
	prod.Environment = "production"
	dev := base
	dev.Environment = "development"

	a, err := store.Upsert(ctx, prod)
	require.NoError(t, err)
	b, err := store.Upsert(ctx, dev)
	require.NoError(t, err)

	require.NotEqual(t, a.ID, b.ID)
	require.Equal(t, 2, countAgents(t, pool))
}
```

You will also need small helpers `seedOrgAndTeam` and `countAgents` in a `helpers_test.go`. Write them as plain inserts and counts — no abstraction.

**Step 2: Run and verify they fail**

Run: `go test ./internal/db/ -run TestUpsertAgent -v`
Expected: FAIL — `undefined: NewAgentStore`.

**Step 3: Write the migration `00003_agents.sql`**

```sql
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
```

**Step 4: Implement `internal/db/agents.go`**

```go
package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type UpsertAgentInput struct {
	OrgID       uuid.UUID
	TeamID      uuid.UUID
	InstanceKey string
	Environment string
	DisplayName string
	Purpose     string
	Platform    []byte // json
	Hosting     []byte // json
	Source      []byte // json
	Telemetry   []byte // json
	Attributes  []byte // json
	Flags       []string
}

type UpsertAgentResult struct {
	ID      uuid.UUID
	Created bool
}

type AgentStore struct{ pool *pgxpool.Pool }

func NewAgentStore(pool *pgxpool.Pool) *AgentStore { return &AgentStore{pool: pool} }

const upsertAgentSQL = `
INSERT INTO agents (
    org_id, team_id, instance_key, environment,
    display_name, purpose,
    platform, hosting, source, telemetry_links, attributes, flags
) VALUES (
    $1, $2, $3, $4, $5, $6,
    COALESCE($7,'{}')::jsonb, COALESCE($8,'{}')::jsonb, COALESCE($9,'{}')::jsonb,
    COALESCE($10,'[]')::jsonb, COALESCE($11,'{}')::jsonb, $12
)
ON CONFLICT (org_id, environment, instance_key) DO UPDATE SET
    team_id         = EXCLUDED.team_id,
    display_name    = EXCLUDED.display_name,
    purpose         = EXCLUDED.purpose,
    platform        = EXCLUDED.platform,
    hosting         = EXCLUDED.hosting,
    source          = EXCLUDED.source,
    telemetry_links = EXCLUDED.telemetry_links,
    attributes      = EXCLUDED.attributes,
    flags           = EXCLUDED.flags,
    lifecycle_state = CASE
        WHEN agents.lifecycle_state IN ('idle','stale') THEN 'active'
        ELSE agents.lifecycle_state
    END,
    last_heartbeat_at = now(),
    updated_at        = now()
RETURNING id, (xmax = 0) AS created;
`

// Upsert registers or updates an agent. It is idempotent on
// (org_id, environment, instance_key) and safe under concurrency.
//
// The (xmax = 0) expression is a PostgreSQL idiom for "this row was inserted
// rather than updated by this statement" — xmax is zero on a fresh insert.
//
// Note the lifecycle CASE: re-registering revives an idle or stale agent, but
// never resurrects one a human deliberately retired or quarantined.
func (s *AgentStore) Upsert(ctx context.Context, in UpsertAgentInput) (UpsertAgentResult, error) {
	var out UpsertAgentResult
	err := s.pool.QueryRow(ctx, upsertAgentSQL,
		in.OrgID, in.TeamID, in.InstanceKey, in.Environment,
		in.DisplayName, in.Purpose,
		in.Platform, in.Hosting, in.Source, in.Telemetry, in.Attributes, in.Flags,
	).Scan(&out.ID, &out.Created)
	if err != nil {
		return UpsertAgentResult{}, fmt.Errorf("upsert agent: %w", err)
	}
	return out, nil
}
```

**Step 5: Run and verify all three pass**

Run: `go test ./internal/db/ -run TestUpsertAgent -race -v`
Expected: PASS. The `-race` flag is not optional on the concurrency test.

**Step 6: Commit**

```bash
git add internal/db/
git commit -s -m "feat: add agents schema with idempotent identity upsert"
```

---

### Task 13: Migration — revisions, credentials, and audit log

All three are append-only. Nothing in this product deletes history — see [SYSTEM.md §6](../../SYSTEM.md#6-two-principles-that-shape-the-product).

**Files:**
- Create: `internal/db/migrations/00004_history.sql`
- Create: `internal/db/revisions.go`, `internal/db/revisions_test.go`

**Step 1: Write the failing test** — assert that writing a revision preserves prior revisions, that revisions are returned newest-first, and that `UPDATE` on `agent_revisions` is rejected by the database.

**Step 2: Run and verify it fails.**

**Step 3: Write the migration**

```sql
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION reject_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'table % is append-only', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER agent_revisions_append_only
    BEFORE UPDATE OR DELETE ON agent_revisions
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER audit_log_append_only
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- +goose Down
DROP TRIGGER audit_log_append_only ON audit_log;
DROP TRIGGER agent_revisions_append_only ON agent_revisions;
DROP FUNCTION reject_mutation();
DROP TABLE audit_log;
DROP TABLE agent_credentials;
DROP TABLE agent_revisions;
```

Enforcing append-only in the database rather than in application code means a future bug, a migration script, or a well-meaning operator with `psql` cannot quietly rewrite audit history. The `ON DELETE CASCADE` is intentional — history dies only with a deliberate hard delete of the agent, which is itself an audited administrator action.

**Step 4: Run and verify the tests pass.**

**Step 5: Commit**

```bash
git add internal/db/
git commit -s -m "feat: add append-only revisions, credentials, and audit log"
```

---

# Milestone 2 — Service layer

All business logic lives here. The HTTP and MCP layers call into it and contain no logic of their own — that is what stops the two surfaces from drifting apart.

### Task 14: Domain types and record-and-flag validation

**Files:** `internal/domain/agent.go`, `internal/domain/descriptor.go`, `internal/domain/descriptor_test.go`

Implements [FUNCTIONAL.md OWN-5](../../FUNCTIONAL.md#own-5--record-and-flag-rather-than-reject). `Validate` returns **flags, not errors** for anything parseable. Tests must cover: a descriptor missing `purpose` is accepted with a `missing_purpose` flag; an unknown field lands in `attributes` rather than being dropped; only an unparseable body or an absent `name` is a hard rejection.

### Task 15: Token service

**Files:** `internal/service/tokens.go`, `internal/service/tokens_test.go`

Mint (returns plaintext exactly once), resolve a plaintext token to its `(org, team, environment)` scope, reject expired and revoked tokens, and update `last_used_at`. Tests must cover the revoked and expired paths explicitly — those are the security-relevant ones.

### Task 16: Registration service

**Files:** `internal/service/register.go`, `internal/service/register_test.go`

The centrepiece. Resolves the enrollment token to a scope, **takes team and environment from the token and ignores any value in the descriptor**, validates into flags, upserts, issues a per-agent credential, writes a revision and an audit entry, and returns the server-chosen heartbeat interval.

Tests must include one that submits a descriptor claiming a different team and asserts the token's team wins. That test is the product's audit story in executable form.

### Task 17: Heartbeat service

**Files:** `internal/service/heartbeat.go`, `internal/service/heartbeat_test.go`

Authenticates the per-agent credential, updates `last_heartbeat_at` and `replica_count`, revives `idle`/`stale` but never `retired`/`quarantined`, and writes **no** revision — a heartbeat is not a change.

### Task 18: Update service

**Files:** `internal/service/update.go`, `internal/service/update_test.go`

Partial descriptor update, writes a revision capturing before and after, and refuses to overwrite fields a human has explicitly edited unless released back to auto-management.

### Task 19: Lifecycle worker

**Files:** `internal/worker/lifecycle.go`, `internal/worker/lifecycle_test.go`

Transitions `active → idle` after missed heartbeats and `idle → stale` after the configured days. **Never deletes.** Tests inject a clock rather than sleeping.

---

# Milestone 3 — HTTP API

### Task 20: Router and authentication middleware

**Files:** `internal/httpapi/router.go`, `internal/httpapi/auth.go`, plus tests

Uses `net/http.ServeMux` with method-and-wildcard patterns; no third-party router. Two middlewares: enrollment-token auth (register only) and agent-credential auth (heartbeat and update). Tests cover missing header, malformed header, unknown token, and revoked token — each must return 401 with no detail that distinguishes them, since a distinguishable error is a token-enumeration oracle.

### Task 21: `POST /api/v1/agents:register`
### Task 22: `POST /api/v1/agents/{id}/heartbeat`
### Task 23: `PATCH /api/v1/agents/{id}`

**Files:** `internal/httpapi/agents.go`, `internal/httpapi/agents_test.go`

Thin handlers. Decode, call the service, encode. Any conditional beyond error mapping belongs in Milestone 2 instead.

### Task 24: Contract fixture set

**Files:** `spec/conformance/cases.json`, `internal/httpapi/contract_test.go`

A declarative set of request/response cases covering every acceptance criterion in FUNCTIONAL.md §5. The runner executes them against the HTTP surface here, and against the MCP surface in Milestone 4 — **from the same file**. This is the mechanism that stops the two surfaces from telling agents different things.

### Task 25: Wire `main.go` and smoke test

**Files:** `cmd/aiinv/main.go`, `cmd/aiinv/main_test.go`

Load config, migrate, open the pool, start the worker, serve, handle graceful shutdown. End-to-end test: start the binary against a testcontainer, mint a token, register, heartbeat, read it back.

**Milestone 3 exit criteria:** an agent can be registered with `curl` and appears correctly. Demo it before continuing.

---

# Milestones 4–8 — task-level outline

Each of these gets its own detailed plan document, written when the milestone starts.

### Milestone 4 — MCP surface
MCP server over streamable HTTP exposing `register_agent`, `heartbeat`, `update_agent`, `get_agent`, `search_agents`, all delegating to Milestone 2 services. The Task 24 contract suite runs against it unchanged.

**Verify the official MCP Go SDK's current import path and server API before writing this milestone's plan** — do not assume it from memory.

### Milestone 5 — Admin API, auth, and export
OIDC plus bootstrap admin; the four roles; team and token management endpoints; agent list with filters; the gaps view; CSV and JSON export with golden-file tests. The export must record the filter applied, the user, and the timestamp, and must carry the unclaimed, stale, and flagged counts required by [FUNCTIONAL.md AUD-4](../../FUNCTIONAL.md#aud-4--be-honest-about-coverage).

### Milestone 6 — Web UI
Vite + React + TypeScript, built into `web/dist` and embedded via `embed.FS` so `make build` produces one binary that serves everything. Screens: agent list with filters, agent detail with revision history and telemetry links, gaps view, token management, export. Auto-detected values must be visually distinguishable from agent-reported ones.

### Milestone 7 — Python SDK *(Apache-2.0)*
Three-line happy path. Bounded queue plus a single background worker; drops oldest when full; **never raises into the caller**; exponential backoff with full jitter; circuit breaker. Detectors for framework, hosting, and source. `instance_key` derivation.

Includes the **NFR-1 harness**: run a host agent loop while the server returns 500s, hangs, and drops TCP mid-request, and assert p99 is statistically unchanged versus no client installed. This test is the SDK's reason to be trusted; write it before the detectors, not after.

**Blocked on an open question:** `instance_key` derivation for Lambda and Cloudflare Workers, where versions and aliases make deployment identity ambiguous. Decide per platform before starting.

### Milestone 8 — TypeScript SDK and release
TypeScript SDK passing the same conformance spec. goreleaser for cross-platform binaries, a multi-arch container image, and a quickstart that gets someone from zero to a registered agent in five minutes.

---

## Risks

| Risk | Mitigation |
|---|---|
| `instance_key` ambiguity on serverless | Decide per platform before Milestone 7. Blocking. |
| MCP Go SDK API drift | Verify against current docs when Milestone 4 starts; never from memory. |
| Contract suite drifts from FUNCTIONAL.md | Every acceptance criterion maps to a named case in `cases.json`. Review together. |
| NFR-1 regressing silently | It is a test in CI, not a code-review convention. |
| Scope creep back into phase 1 | `model_ids` is the likely vector. It is a connection, and connections are phase 2. |
