# Milestone 4 — MCP Surface Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Expose `register_agent`, `heartbeat`, `update_agent`, `get_agent`, and `search_agents` as MCP tools over streamable HTTP, delegating to the exact same service layer the HTTP API uses, so an agent that already speaks MCP can be inventoried by adding a server to its configuration and nothing else (FUNCTIONAL.md AR-2).

**Architecture:** A new `internal/mcpapi` package builds an `*mcp.Server` from the official Go SDK, with one tool per service-layer operation. Two new read-only operations (`GetAgent`, `SearchAgents`) are added to the service layer first, since neither `get_agent` nor `search_agents` has an HTTP equivalent to delegate to yet. The MCP server is mounted alongside the existing REST API in the composition root, on its own path, over the same PostgreSQL-backed services and the same enrollment-token / per-agent-credential authentication model — just carried as a bearer header on each MCP tool call instead of an HTTP request header on each REST call, which turns out to be the same mechanism: streamable HTTP tool calls still arrive as HTTP requests, and the SDK exposes the original request's headers to every tool handler.

**Tech Stack:** `github.com/modelcontextprotocol/go-sdk/mcp` v1.7.0, added to the existing Go 1.26 / PostgreSQL 16 / pgx/v5 / testify stack. No other new dependency.

**Spec:** [FUNCTIONAL.md AR-2](../../FUNCTIONAL.md#ar-2--register-via-mcp-p1), [design doc §MCP surface](2026-08-15-ai-inventory-design.md), [SYSTEM.md](../../SYSTEM.md)

## Global Constraints

- TDD is not optional. Write the failing test, watch it fail, write the minimum to pass, watch it pass, commit.
- Commit after every task with `git commit -s` (DCO sign-off).
- Never mock PostgreSQL. Use `internal/db/testdb`.
- Run `go vet ./...`, `gofmt -l .`, and `go test ./...` before every commit (this box has no `make`; run the three commands directly).
- The MCP server and the HTTP server delegate to one service layer and never re-implement a rule. Any conditional beyond decode/call/encode belongs in `internal/service`, not in `internal/mcpapi`.
- Ownership (team, environment) is never accepted from a tool's arguments, for the same reason it is never accepted from the HTTP body: it comes from the credential.

## Research already done (do not re-derive)

The official SDK's current API was verified directly against the installed
v1.7.0 module (`go doc`) and against two runnable probes, not from memory or
scraped docs — the accuracy of this plan depends on the findings below:

1. **Server construction:** `mcp.NewServer(&mcp.Implementation{Name, Version}, nil)`. Tools are registered with the generic `mcp.AddTool[In, Out any](s *mcp.Server, t *mcp.Tool, h ToolHandlerFor[In, Out])`, where `ToolHandlerFor[In, Out] = func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error)`. `AddTool` infers the JSON input/output schema from `In`/`Out`'s struct tags and automatically unmarshals, validates, and populates `CallToolResult.StructuredContent`.
2. **`json.RawMessage` is the wrong type for a "nested JSON object" argument, and `uuid.UUID` is the wrong type for an id argument — both for the same reason.** Verified by two probes: the SDK's schema inference (`github.com/google/jsonschema-go`) walks a field's underlying Go type rather than respecting a custom `MarshalJSON`/`UnmarshalJSON`. `json.RawMessage` (`[]byte`) infers `{"type":"array","items":{"type":"integer",...}}` — a byte array, not an object. `uuid.UUID` (`[16]byte`) infers a 16-element integer array the same way, and a real call sending a UUID string then fails schema validation with `type: <uuid> has type "string", want "array"`. **Use `map[string]any` for a descriptor field** (infers `{"type":"object","additionalProperties":true}` correctly, round-trips through `json.Marshal` into the exact bytes `domain.DecodeDescriptor` already accepts) **and `string` for an agent-id field**, parsed with `uuid.Parse` inside the handler.
3. **The bearer credential travels as the `Authorization` HTTP header, not as a tool argument.** `mcp.CallToolRequest` embeds `Extra *mcp.RequestExtra`, and `RequestExtra.Header` is `http.Header` — "header from HTTP request, if any". Verified by probe: over a real `mcp.NewStreamableHTTPHandler`-served connection, a header set on the client's `http.Client.Transport` arrives intact at `req.Extra.Header.Get("Authorization")` inside the tool handler. This keeps the MCP surface's auth model identical to the HTTP surface's — same header, same token formats, same service-layer sentinels — rather than inventing a second credential-carrying convention.
4. **`req.Extra` is `nil` over the in-memory transport** (`mcp.NewInMemoryTransports`), because there is no real HTTP request behind it. Unit tests for tool handlers must therefore call the handler function value directly with a hand-built `&mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: ...}}` rather than going through a transport — verified working by probe. The full transport (real headers, real schema validation, real JSON wire format) is exercised by the end-to-end test in Task 4 and the contract suite in Task 5, both over real streamable HTTP.
5. **`mcp.NewStreamableHTTPHandler(getServer func(*http.Request) *mcp.Server, opts *mcp.StreamableHTTPOptions) *mcp.StreamableHTTPHandler`** implements `http.Handler` (`ServeHTTP`), so it mounts on `net/http.ServeMux` exactly like any other handler. `StreamableHTTPOptions.Stateless: true` is the right mode here: our auth is already per-call rather than per-session, so there is no server-side session state worth keeping, and statelessness sidesteps `Mcp-Session-Id` bookkeeping entirely — consistent with OWN-7's "self-host without ceremony."
6. **Module compatibility:** the SDK requires Go 1.25+; this project is on Go 1.26. No conflict.

## Design decisions this plan makes explicit

**Read-tool authorization scope.** `get_agent` and `search_agents` have no HTTP precedent to mirror (Milestone 5's admin API, with real operator roles, does not exist yet). Rather than block on that milestone or invent a permissive default, this plan gives both tools the narrowest scope the credentials that exist today can honestly support:
- `search_agents` requires an **enrollment token** and returns only agents within that token's own `(org, team, environment)` — the same scope registering with that token would inherit. An agent has no legitimate reason to search the fleet with its own per-agent credential, so that path is not supported.
- `get_agent` accepts either an **enrollment token** scoped to the agent's `(org, team, environment)`, or the agent's **own per-agent credential**. This lets an agent look up its own record (to confirm a change took effect) and lets whoever holds a team's enrollment token inspect any agent that token could have registered.

Milestone 5's admin API will supersede this with real operator identity and roles; this scope is the smallest thing that is honest about what a bare credential can prove today, not a permanent design.

**A new error class, one addition to an existing closed set.** A syntactically valid credential that resolves but names an agent outside its scope is not "not found," "revoked," or "expired" — none of the existing sentinels describe it truthfully. It is, however, exactly as security-sensitive as the rest of `service.AuthSentinels`: a caller who could distinguish "wrong scope" from "credential doesn't exist" would learn something about a token they were guessing at. `service.ErrAgentNotVisible` is added to the sentinel set for this reason, and nothing else changes about how the set is classified.

---

## Task 1: MCP Go SDK dependency, and one definition of a bearer credential

Two changes that have nothing to do with MCP tools yet, done first because every later task depends on both: the dependency has to resolve, and the auth-classification logic that Task 3's tools rely on must live somewhere both `internal/httpapi` and `internal/mcpapi` can share — otherwise the two surfaces' 401s could quietly drift apart, which is the exact failure `internal/httpapi/errors.go` was written to prevent for HTTP alone.

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `internal/auth/bearer.go`, `internal/auth/bearer_test.go`
- Create: `internal/service/auth_errors.go`
- Modify: `internal/httpapi/middleware.go`, `internal/httpapi/errors.go`, `internal/httpapi/agents.go`

**Interfaces:**
- Produces: `auth.BearerToken(header http.Header) (string, error)`, `auth.ErrNoBearerToken`, `service.AuthSentinels []error`

**Step 1: Add the dependency**

```bash
go get github.com/modelcontextprotocol/go-sdk/mcp@v1.7.0
```

Run: `go build ./...`
Expected: succeeds (nothing imports the package yet, but `go.mod`/`go.sum` must resolve cleanly before anything else in this task is testable).

**Step 2: Write the failing test for the shared bearer-token helper**

```go
// internal/auth/bearer_test.go
package auth

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBearerTokenExtractsTheCredential(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer aiinv_ac_something")

	token, err := BearerToken(h)

	require.NoError(t, err)
	require.Equal(t, "aiinv_ac_something", token)
}

func TestBearerTokenMatchesTheSchemeCaseInsensitively(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "bearer aiinv_ac_something")

	token, err := BearerToken(h)

	require.NoError(t, err)
	require.Equal(t, "aiinv_ac_something", token)
}

func TestBearerTokenRejectsWhatIsNotAUsableCredential(t *testing.T) {
	cases := map[string]string{
		"absent":       "",
		"wrong scheme": "Basic aGk6dGhlcmU=",
		"scheme only":  "Bearer",
		"empty token":  "Bearer ",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			h := http.Header{}
			if value != "" {
				h.Set("Authorization", value)
			}
			_, err := BearerToken(h)
			require.ErrorIs(t, err, ErrNoBearerToken)
		})
	}
}
```

**Step 2: Run and verify it fails**

Run: `go test ./internal/auth/ -run TestBearerToken -v`
Expected: FAIL — `undefined: BearerToken`.

**Step 3: Implement it**

```go
// internal/auth/bearer.go
package auth

import (
	"errors"
	"net/http"
	"strings"
)

// ErrNoBearerToken means the request carried no usable bearer credential: no
// Authorization header, the wrong scheme, or an empty token. It is exported
// so every surface that accepts a bearer credential — HTTP request headers
// today, MCP tool calls tomorrow, both carried as the same Authorization
// header — classifies the same failure the same way, rather than each
// surface inventing its own "you sent nothing" sentinel that the other does
// not recognise.
var ErrNoBearerToken = errors.New("no usable bearer credential in request")

// BearerToken extracts the credential from an Authorization header of the
// form "Bearer <token>". The scheme is matched without regard to case, per
// RFC 9110, and because a client that sends "bearer" is a client with a
// working credential.
//
// The token is returned to be handed to a service, never inspected here:
// this function does not know what a valid credential looks like, and the
// services authenticate inside their own transactions so that a refused call
// writes nothing.
func BearerToken(header http.Header) (string, error) {
	scheme, rest, found := strings.Cut(header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", ErrNoBearerToken
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", ErrNoBearerToken
	}
	return token, nil
}
```

**Step 4: Run and verify it passes**

Run: `go test ./internal/auth/ -v`
Expected: PASS, all tests including the pre-existing token tests.

**Step 5: Point `internal/httpapi` at the shared helper**

Delete `bearerToken` from `internal/httpapi/middleware.go` (the function itself; leave `limitBody` and the rest of the file alone).

In `internal/httpapi/errors.go`, replace the local sentinel with an alias so every existing reference to `errUnauthenticated` keeps compiling unchanged:

```go
// errUnauthenticated is what the request never got as far as a credential —
// no Authorization header, the wrong scheme, an empty token. It is an alias
// for auth.ErrNoBearerToken, not a second definition of the same idea: MCP
// tool calls hit the identical check through the identical sentinel.
var errUnauthenticated = auth.ErrNoBearerToken
```

Add the import (`"github.com/ebellefontaine/ai-inventory/internal/auth"`) to `errors.go`.

In `internal/httpapi/agents.go`, replace both call sites:

```go
token, err := bearerToken(r)
```

with:

```go
token, err := auth.BearerToken(r.Header)
```

(add the same import to `agents.go`).

**Step 6: Extract the closed sentinel set into `internal/service`**

```go
// internal/service/auth_errors.go
package service

// AuthSentinels is the closed set of errors that mean "this credential did
// not work," across every surface that authenticates one against this
// service layer.
//
// It exists so that HTTP and MCP cannot independently decide what counts as
// an authentication failure and drift apart. A caller who could tell one
// surface's rejection apart from the other's — or tell two of these apart
// from each other, on either surface — would learn something about a
// credential they were guessing at that this system exists to not reveal.
// Every surface's error classifier ranges over this slice rather than
// listing the sentinels itself.
var AuthSentinels = []error{
	ErrTokenNotFound,
	ErrTokenRevoked,
	ErrTokenExpired,
	ErrInvalidEnvironment,
	ErrCredentialNotFound,
	ErrCredentialRevoked,
	ErrCredentialExpired,
	ErrCredentialAgentMismatch,
}
```

In `internal/httpapi/errors.go`, replace the hand-listed `authFailures` slice:

```go
var authFailures = []error{
	errUnauthenticated,
	service.ErrTokenNotFound,
	service.ErrTokenRevoked,
	service.ErrTokenExpired,
	service.ErrInvalidEnvironment,
	service.ErrCredentialNotFound,
	service.ErrCredentialRevoked,
	service.ErrCredentialExpired,
	service.ErrCredentialAgentMismatch,
}
```

with:

```go
// authFailures is every error that must be indistinguishable on the wire:
// errUnauthenticated (this layer's own "sent nothing" case) plus the closed
// set every service-layer credential check can return. Sourcing the second
// half from service.AuthSentinels rather than listing it here again is what
// keeps a future addition to that set — Task 4 of the MCP surface plan adds
// one — from silently not reaching this classifier.
var authFailures = append([]error{errUnauthenticated}, service.AuthSentinels...)
```

**Step 7: Run the full existing suite to prove the refactor is behaviour-preserving**

Run: `go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: everything that passed before this task still passes, unchanged — `router_test.go`, `agents_test.go`, and `contract_test.go` in particular, since they assert the exact indistinguishable-401 behaviour this refactor must not touch.

**Step 8: Commit**

```bash
git add go.mod go.sum internal/auth/ internal/service/auth_errors.go internal/httpapi/
git commit -s -m "refactor: share bearer-token parsing and the auth-sentinel set

Both will be reused by the MCP surface in the next task; defining them once
here is what stops the HTTP and MCP surfaces' authentication failures from
being able to drift apart from each other."
```

---

## Task 2: Read service — `GetAgent` and `SearchAgents`

Neither `get_agent` nor `search_agents` has an HTTP endpoint to delegate to; Milestone 2 only ever built the write path (`RegistrationService`, `HeartbeatService`, `UpdateService`). This task adds the read path those two MCP tools need, scoped the way this plan's design-decisions section describes: an enrollment token sees its own `(org, team, environment)`; a per-agent credential sees only itself.

**Files:**
- Create: `internal/service/query.go`, `internal/service/query_test.go`
- Modify: `internal/service/auth_errors.go`

**Interfaces:**
- Produces: `service.NewQueryService(pool *pgxpool.Pool, tokens *TokenService) *QueryService`, `service.Agent` struct, `service.GetAgentInput{AgentID uuid.UUID, Credential string}`, `service.QueryService.GetAgent(ctx, GetAgentInput) (Agent, error)`, `service.SearchAgentsInput{EnrollmentToken string, LifecycleState string, Limit int}`, `service.QueryService.SearchAgents(ctx, SearchAgentsInput) ([]Agent, error)`, `service.ErrAgentNotVisible`, `service.ErrInvalidLifecycleState`

**Step 1: Write the failing tests**

```go
// internal/service/query_test.go
package service_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

// queryFixture registers one agent with a real enrollment token and returns
// everything a test needs to query for it: the query service, the token
// that scoped the registration, the credential registration issued, and the
// agent's id.
type queryFixture struct {
	queries    *service.QueryService
	enrollment string
	agentToken string
	agentID    uuid.UUID
}

func newQueryFixture(t *testing.T) queryFixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t)

	var orgID, teamID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ('Acme', 'acme') RETURNING id`).Scan(&orgID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, 'Platform', 'platform') RETURNING id`,
		orgID).Scan(&teamID))

	tokens := service.NewTokenService(pool)
	minted, err := tokens.Mint(ctx, service.MintInput{
		OrgID: orgID, TeamID: teamID, Environment: domain.EnvironmentProduction,
		Name: "platform enrollment", CreatedBy: "test",
	})
	require.NoError(t, err)

	registration := service.NewRegistrationService(pool, tokens, 60)
	registered, err := registration.Register(ctx, service.RegisterInput{
		EnrollmentToken: minted.Plaintext,
		RawDescriptor:   []byte(`{"name":"Invoice triage","purpose":"Triages AP invoices","instance_key":"query-fixture-1"}`),
	})
	require.NoError(t, err)

	return queryFixture{
		queries:    service.NewQueryService(pool, tokens),
		enrollment: minted.Plaintext,
		agentToken: registered.AgentToken,
		agentID:    registered.AgentID,
	}
}

func TestGetAgentByEnrollmentTokenInItsOwnScope(t *testing.T) {
	f := newQueryFixture(t)

	agent, err := f.queries.GetAgent(context.Background(),
		service.GetAgentInput{AgentID: f.agentID, Credential: f.enrollment})

	require.NoError(t, err)
	require.Equal(t, "Invoice triage", agent.DisplayName)
	require.Equal(t, string(domain.EnvironmentProduction), agent.Environment)
}

func TestGetAgentByItsOwnCredential(t *testing.T) {
	f := newQueryFixture(t)

	agent, err := f.queries.GetAgent(context.Background(),
		service.GetAgentInput{AgentID: f.agentID, Credential: f.agentToken})

	require.NoError(t, err)
	require.Equal(t, f.agentID, agent.ID)
}

func TestGetAgentRefusesAnEnrollmentTokenFromAnotherTeam(t *testing.T) {
	f := newQueryFixture(t)
	ctx := context.Background()

	pool := testdb.New(t) // a second, unrelated installation
	var otherOrg, otherTeam uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ('Other', 'other') RETURNING id`).Scan(&otherOrg))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, 'Other team', 'other-team') RETURNING id`,
		otherOrg).Scan(&otherTeam))
	otherTokens := service.NewTokenService(pool)
	otherMinted, err := otherTokens.Mint(ctx, service.MintInput{
		OrgID: otherOrg, TeamID: otherTeam, Environment: domain.EnvironmentProduction,
		Name: "other enrollment", CreatedBy: "test",
	})
	require.NoError(t, err)

	_, err = f.queries.GetAgent(ctx, service.GetAgentInput{AgentID: f.agentID, Credential: otherMinted.Plaintext})

	require.ErrorIs(t, err, service.ErrAgentNotVisible)
}

func TestGetAgentRefusesAnUnrecognisedCredential(t *testing.T) {
	f := newQueryFixture(t)

	_, err := f.queries.GetAgent(context.Background(),
		service.GetAgentInput{AgentID: f.agentID, Credential: "aiinv_ac_doesnotexist"})

	require.ErrorIs(t, err, service.ErrCredentialNotFound)
}

func TestSearchAgentsReturnsOnlyItsOwnScope(t *testing.T) {
	f := newQueryFixture(t)

	results, err := f.queries.SearchAgents(context.Background(),
		service.SearchAgentsInput{EnrollmentToken: f.enrollment})

	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, f.agentID, results[0].ID)
}

func TestSearchAgentsFiltersByLifecycleState(t *testing.T) {
	f := newQueryFixture(t)

	active, err := f.queries.SearchAgents(context.Background(),
		service.SearchAgentsInput{EnrollmentToken: f.enrollment, LifecycleState: "active"})
	require.NoError(t, err)
	require.Len(t, active, 1)

	stale, err := f.queries.SearchAgents(context.Background(),
		service.SearchAgentsInput{EnrollmentToken: f.enrollment, LifecycleState: "stale"})
	require.NoError(t, err)
	require.Empty(t, stale)
}

func TestSearchAgentsRefusesAnInvalidLifecycleFilter(t *testing.T) {
	f := newQueryFixture(t)

	_, err := f.queries.SearchAgents(context.Background(),
		service.SearchAgentsInput{EnrollmentToken: f.enrollment, LifecycleState: "not-a-real-state"})

	require.ErrorIs(t, err, service.ErrInvalidLifecycleState)
}

func TestSearchAgentsRequiresAnEnrollmentTokenNotAnAgentCredential(t *testing.T) {
	f := newQueryFixture(t)

	_, err := f.queries.SearchAgents(context.Background(),
		service.SearchAgentsInput{EnrollmentToken: f.agentToken})

	require.ErrorIs(t, err, service.ErrTokenNotFound)
}
```

**Step 2: Run and verify it fails**

Run: `go test ./internal/service/ -run "TestGetAgent|TestSearchAgents" -v`
Expected: FAIL — `undefined: service.NewQueryService`.

**Step 3: Implement**

```go
// internal/service/query.go
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ebellefontaine/ai-inventory/internal/domain"
)

// ErrAgentNotVisible means the credential presented is genuinely valid, but
// its scope does not include the agent asked for: an enrollment token from a
// different team, or a per-agent credential belonging to a different agent.
//
// It is part of AuthSentinels rather than a distinct kind of failure. A
// caller who could tell "wrong scope" apart from "credential doesn't exist"
// would learn something about a credential they were guessing at, which is
// exactly what the rest of that set exists to prevent.
var ErrAgentNotVisible = errors.New("agent not visible to this credential")

// ErrInvalidLifecycleState means a search filter named something that is not
// one of domain's five lifecycle states. Unlike the credential errors, this
// is an ordinary client mistake — it carries no information about anything
// secret — so it is reported plainly rather than folded into AuthSentinels.
var ErrInvalidLifecycleState = errors.New("invalid lifecycle state filter")

func init() {
	AuthSentinels = append(AuthSentinels, ErrAgentNotVisible)
}

const (
	defaultSearchLimit = 50
	maxSearchLimit     = 200
)

// Agent is the read model returned by GetAgent and SearchAgents: everything
// about a record a caller with a valid credential is allowed to see.
//
// It is a type of its own rather than db.UpsertAgentInput or a raw row scan,
// because a read model and a write model answer different questions — this
// one exists to be handed to an MCP client or, later, rendered in the web UI,
// and its shape should be free to diverge from storage without disturbing
// either.
type Agent struct {
	ID                 uuid.UUID
	TeamSlug           string
	Environment        string
	DisplayName        string
	Purpose            string
	LifecycleState     string
	RegistrationSource string
	ReplicaCount       int
	Platform           json.RawMessage
	Hosting            json.RawMessage
	Source             json.RawMessage
	TelemetryLinks     json.RawMessage
	Flags              []string
	FirstSeenAt        time.Time
	LastHeartbeatAt    time.Time
}

// QueryService answers read-only questions about agents already in the
// inventory. It never writes: no revision, no audit entry, no lifecycle
// transition — those all belong to the services that actually change a
// record.
type QueryService struct {
	pool   *pgxpool.Pool
	tokens *TokenService
}

func NewQueryService(pool *pgxpool.Pool, tokens *TokenService) *QueryService {
	return &QueryService{pool: pool, tokens: tokens}
}

// GetAgentInput is one lookup: which agent, and the credential that has to
// justify seeing it.
type GetAgentInput struct {
	AgentID    uuid.UUID
	Credential string
}

// GetAgent resolves Credential as an enrollment token first — the common
// case for an operator or an agent checking on a teammate — and falls back
// to treating it as the agent's own per-agent credential only if that fails.
// Two lookups on the unauthorized path cost nothing a caller can observe: the
// response is the same ErrAgentNotVisible-or-credential-error either way.
func (s *QueryService) GetAgent(ctx context.Context, in GetAgentInput) (Agent, error) {
	agent, err := s.readAgent(ctx, in.AgentID)
	if err != nil {
		return Agent{}, err
	}

	if scope, tokenErr := s.tokens.Resolve(ctx, in.Credential); tokenErr == nil {
		if agent.orgID != scope.OrgID || agent.teamID != scope.TeamID || agent.Environment != string(scope.Environment) {
			return Agent{}, ErrAgentNotVisible
		}
		return agent.Agent, nil
	}

	if _, err := authenticateAgent(ctx, s.pool, in.AgentID, in.Credential, "get agent"); err != nil {
		return Agent{}, err
	}
	return agent.Agent, nil
}

// SearchAgentsInput is one search: the enrollment token whose scope bounds
// the results, an optional lifecycle filter, and how many rows to return.
type SearchAgentsInput struct {
	EnrollmentToken string
	// LifecycleState, if non-empty, must be one of the five states domain
	// defines. Empty means no filter.
	LifecycleState string
	// Limit caps the result set. Zero or negative means defaultSearchLimit;
	// anything above maxSearchLimit is clamped to it.
	Limit int
}

// SearchAgents lists agents within the enrollment token's own scope, most
// recently heartbeated first.
func (s *QueryService) SearchAgents(ctx context.Context, in SearchAgentsInput) ([]Agent, error) {
	scope, err := s.tokens.Resolve(ctx, in.EnrollmentToken)
	if err != nil {
		return nil, err
	}

	if in.LifecycleState != "" && !domain.LifecycleState(in.LifecycleState).Valid() {
		return nil, fmt.Errorf("search agents: %w: %q", ErrInvalidLifecycleState, in.LifecycleState)
	}

	limit := in.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	rows, err := s.pool.Query(ctx, searchAgentsSQL, scope.OrgID, scope.TeamID, string(scope.Environment), in.LifecycleState, limit)
	if err != nil {
		return nil, fmt.Errorf("search agents: %w", err)
	}
	defer rows.Close()

	var results []Agent
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("search agents: %w", err)
		}
		results = append(results, agent.Agent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search agents: %w", err)
	}
	return results, nil
}

// agentWithScope pairs the public read model with the two columns GetAgent
// needs for its own scope check but that Agent has no reason to expose.
type agentWithScope struct {
	Agent
	orgID  uuid.UUID
	teamID uuid.UUID
}

func (s *QueryService) readAgent(ctx context.Context, agentID uuid.UUID) (agentWithScope, error) {
	row := s.pool.QueryRow(ctx, getAgentSQL, agentID)
	agent, err := scanAgent(row)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Deliberately the credential-not-found sentinel, not a distinct
		// "agent not found" one: a caller who has not yet proven a credential
		// is valid must not be able to tell a missing agent apart from an
		// agent it simply cannot see. Both eventually surface as whichever
		// authentication step below fails.
		return agentWithScope{}, ErrCredentialNotFound
	case err != nil:
		return agentWithScope{}, fmt.Errorf("get agent: %w", err)
	}
	return agent, nil
}

// row is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query), which is
// what lets GetAgent and SearchAgents share one Scan call and one column
// list instead of keeping two in sync by hand.
type row interface {
	Scan(dest ...any) error
}

func scanAgent(r row) (agentWithScope, error) {
	var a agentWithScope
	err := r.Scan(
		&a.ID, &a.orgID, &a.teamID, &a.TeamSlug, &a.Environment,
		&a.DisplayName, &a.Purpose, &a.LifecycleState, &a.RegistrationSource, &a.ReplicaCount,
		&a.Platform, &a.Hosting, &a.Source, &a.TelemetryLinks, &a.Flags,
		&a.FirstSeenAt, &a.LastHeartbeatAt,
	)
	return a, err
}

const agentColumnsSQL = `
	a.id, a.org_id, a.team_id, t.slug, a.environment,
	a.display_name, a.purpose, a.lifecycle_state, a.registration_source, a.replica_count,
	a.platform, a.hosting, a.source, a.telemetry_links, a.flags,
	a.first_seen_at, a.last_heartbeat_at`

const getAgentSQL = `
SELECT ` + agentColumnsSQL + `
  FROM agents a JOIN teams t ON t.id = a.team_id
 WHERE a.id = $1;
`

// The empty-string lifecycle filter matches every row via the OR clause
// rather than requiring a second query shape, since "no filter" and "filter
// on an empty column value" can never both be true for a NOT NULL column
// with a CHECK constraint on the state vocabulary.
const searchAgentsSQL = `
SELECT ` + agentColumnsSQL + `
  FROM agents a JOIN teams t ON t.id = a.team_id
 WHERE a.org_id = $1 AND a.team_id = $2 AND a.environment = $3
   AND ($4 = '' OR a.lifecycle_state = $4)
 ORDER BY a.last_heartbeat_at DESC
 LIMIT $5;
`
```

**Step 4: Run and verify it passes**

Run: `go test ./internal/service/ -run "TestGetAgent|TestSearchAgents" -v`
Expected: PASS, all nine tests.

**Step 5: Run the full suite**

Run: `go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: everything passes, including `internal/httpapi`'s tests — `service.AuthSentinels` gained a member in `init()`, and `TestMissingOrMalformedAuthorizationIsTheShared401` and friends must still see one indistinguishable 401, now including the new sentinel.

**Step 6: Commit**

```bash
git add internal/service/query.go internal/service/query_test.go internal/service/auth_errors.go
git commit -s -m "feat: add the read service get_agent and search_agents will use"
```

---

## Task 3: The `internal/mcpapi` package — all five tools

One file, one constructor, five tools, each a thin decode/call/encode wrapper exactly like `internal/httpapi/agents.go`'s handlers — the same discipline for the same reason: any logic beyond mapping belongs in Milestone 2 or Task 2's service layer, never here.

**Files:**
- Create: `internal/mcpapi/server.go`, `internal/mcpapi/server_test.go`

**Interfaces:**
- Consumes: `service.RegistrationService.Register`, `service.HeartbeatService.Heartbeat`, `service.UpdateService.Update`, `service.QueryService.GetAgent`, `service.QueryService.SearchAgents` (all from Milestone 2 and Task 2), `auth.BearerToken` (Task 1)
- Produces: `mcpapi.NewServer(registration *service.RegistrationService, heartbeat *service.HeartbeatService, update *service.UpdateService, query *service.QueryService) *mcp.Server`

**Step 1: Write the failing tests**

Every test in this file calls a handler function directly — no transport — per this plan's research finding 4: `req.Extra` is only populated over a real HTTP request, so a hand-built `*mcp.CallToolRequest` is how these handlers are unit-tested. `internal/mcpapi/server_test.go` needs its own copy of the database-and-services fixture `internal/httpapi/agents_test.go` already built, because the two packages cannot share unexported test helpers across a package boundary; the duplication is small (create org, team, mint a token) and deliberate, not an oversight.

```go
// internal/mcpapi/server_test.go
package mcpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

type mcpFixture struct {
	pool       *pgxpool.Pool
	registration *service.RegistrationService
	heartbeat    *service.HeartbeatService
	update       *service.UpdateService
	query        *service.QueryService
	enrollment   string
}

func newMCPFixture(t *testing.T) mcpFixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t)

	var orgID, teamID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ('Acme', 'acme') RETURNING id`).Scan(&orgID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, 'Platform', 'platform') RETURNING id`,
		orgID).Scan(&teamID))

	tokens := service.NewTokenService(pool)
	minted, err := tokens.Mint(ctx, service.MintInput{
		OrgID: orgID, TeamID: teamID, Environment: domain.EnvironmentProduction,
		Name: "platform enrollment", CreatedBy: "test",
	})
	require.NoError(t, err)

	return mcpFixture{
		pool:         pool,
		registration: service.NewRegistrationService(pool, tokens, 45),
		heartbeat:    service.NewHeartbeatService(pool, 45),
		update:       service.NewUpdateService(pool),
		query:        service.NewQueryService(pool, tokens),
		enrollment:   minted.Plaintext,
	}
}

// bearer builds the request every tool handler in this file expects: no
// transport, just a hand-built Extra carrying the Authorization header, per
// this plan's Task 1 research (req.Extra is nil over the in-memory
// transport, and never populated at all without one).
func bearer(token string) *mcp.CallToolRequest {
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: h}}
}

func TestRegisterAgentTool(t *testing.T) {
	f := newMCPFixture(t)
	s := newHandlers(f.registration, f.heartbeat, f.update, f.query)

	_, out, err := s.registerAgent(context.Background(), bearer(f.enrollment), registerAgentIn{
		Descriptor: map[string]any{
			"name":         "Invoice triage",
			"purpose":      "Triages AP invoices",
			"instance_key": "mcp-register-1",
		},
	})

	require.NoError(t, err)
	require.NotEmpty(t, out.AgentID)
	require.NotEmpty(t, out.AgentToken)
	require.Equal(t, 45, out.HeartbeatIntervalSeconds)
	require.True(t, out.Created)
}

func TestRegisterAgentToolRequiresAnEnrollmentToken(t *testing.T) {
	f := newMCPFixture(t)
	s := newHandlers(f.registration, f.heartbeat, f.update, f.query)

	res, _, err := s.registerAgent(context.Background(), bearer(""), registerAgentIn{
		Descriptor: map[string]any{"name": "x", "purpose": "y"},
	})

	require.Error(t, err)
	require.Nil(t, res)
}

func TestHeartbeatTool(t *testing.T) {
	f := newMCPFixture(t)
	s := newHandlers(f.registration, f.heartbeat, f.update, f.query)

	_, registered, err := s.registerAgent(context.Background(), bearer(f.enrollment), registerAgentIn{
		Descriptor: map[string]any{"name": "Worker", "purpose": "Processes jobs", "instance_key": "mcp-heartbeat-1"},
	})
	require.NoError(t, err)

	_, out, err := s.heartbeatTool(context.Background(), bearer(registered.AgentToken), heartbeatIn{
		AgentID: registered.AgentID,
	})

	require.NoError(t, err)
	require.Equal(t, string(domain.LifecycleActive), out.LifecycleState)
	require.Equal(t, 1, out.ReplicaCount)
}

func TestUpdateAgentTool(t *testing.T) {
	f := newMCPFixture(t)
	s := newHandlers(f.registration, f.heartbeat, f.update, f.query)

	_, registered, err := s.registerAgent(context.Background(), bearer(f.enrollment), registerAgentIn{
		Descriptor: map[string]any{"name": "Worker", "purpose": "Processes jobs", "instance_key": "mcp-update-1"},
	})
	require.NoError(t, err)

	_, out, err := s.updateAgent(context.Background(), bearer(registered.AgentToken), updateAgentIn{
		AgentID:    registered.AgentID,
		Descriptor: map[string]any{"purpose": "Processes jobs faster"},
	})

	require.NoError(t, err)
	require.True(t, out.Changed)
}

func TestGetAgentTool(t *testing.T) {
	f := newMCPFixture(t)
	s := newHandlers(f.registration, f.heartbeat, f.update, f.query)

	_, registered, err := s.registerAgent(context.Background(), bearer(f.enrollment), registerAgentIn{
		Descriptor: map[string]any{"name": "Worker", "purpose": "Processes jobs", "instance_key": "mcp-get-1"},
	})
	require.NoError(t, err)

	_, out, err := s.getAgent(context.Background(), bearer(f.enrollment), getAgentIn{
		AgentID: registered.AgentID,
	})

	require.NoError(t, err)
	require.Equal(t, "Worker", out.DisplayName)
}

func TestSearchAgentsTool(t *testing.T) {
	f := newMCPFixture(t)
	s := newHandlers(f.registration, f.heartbeat, f.update, f.query)

	_, _, err := s.registerAgent(context.Background(), bearer(f.enrollment), registerAgentIn{
		Descriptor: map[string]any{"name": "Worker", "purpose": "Processes jobs", "instance_key": "mcp-search-1"},
	})
	require.NoError(t, err)

	_, out, err := s.searchAgents(context.Background(), bearer(f.enrollment), searchAgentsIn{})

	require.NoError(t, err)
	require.Len(t, out.Agents, 1)
}

func TestNewServerRegistersAllFiveTools(t *testing.T) {
	f := newMCPFixture(t)
	server := NewServer(f.registration, f.heartbeat, f.update, f.query)
	require.NotNil(t, server)
}
```

**Step 2: Run and verify it fails**

Run: `go test ./internal/mcpapi/ -v`
Expected: FAIL — the package does not exist yet.

**Step 3: Implement**

```go
// internal/mcpapi/server.go
//
// Package mcpapi is the MCP surface of the inventory: one tool per
// service-layer operation, each a decode/call/encode wrapper with no logic
// of its own — the same discipline internal/httpapi holds itself to, for the
// same reason. The two surfaces delegate to one service layer so they cannot
// tell an agent two different things about the same operation.
//
// Every tool authenticates with the Authorization header carried on the
// underlying HTTP request (mcp.RequestExtra.Header), exactly as the HTTP API
// does — the same header, the same token formats, the same service-layer
// sentinels. See docs/plans/2026-08-16-milestone-4-mcp-surface.md for why.
package mcpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ebellefontaine/ai-inventory/internal/auth"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

const (
	serverName    = "ai-inventory"
	serverVersion = "v0.1.0"
)

// handlers holds the four services every tool delegates to. It exists as a
// type of its own, distinct from NewServer, so the tests in this package can
// call a handler directly without going through mcp.Server at all — the only
// way to exercise req.Extra with a hand-built value rather than a real
// transport.
type handlers struct {
	registration *service.RegistrationService
	heartbeat    *service.HeartbeatService
	update       *service.UpdateService
	query        *service.QueryService
}

func newHandlers(
	registration *service.RegistrationService,
	heartbeat *service.HeartbeatService,
	update *service.UpdateService,
	query *service.QueryService,
) *handlers {
	return &handlers{registration: registration, heartbeat: heartbeat, update: update, query: query}
}

// NewServer builds the MCP server and registers all five tools. Nothing here
// reads a team or an environment from a tool's arguments — the credential is
// the only source of scope, for the same reason internal/httpapi/agents.go
// never reads one from the request body.
func NewServer(
	registration *service.RegistrationService,
	heartbeat *service.HeartbeatService,
	update *service.UpdateService,
	query *service.QueryService,
) *mcp.Server {
	h := newHandlers(registration, heartbeat, update, query)
	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "register_agent",
		Description: "Register this agent with AI Inventory, or update its record if an agent " +
			"with the same identity has already registered. Requires an enrollment token in the " +
			"Authorization header. The owning team and environment are inherited from that token " +
			"and cannot be set by the descriptor.",
	}, h.registerAgent)

	mcp.AddTool(server, &mcp.Tool{
		Name: "heartbeat",
		Description: "Signal that this agent is still running. Requires the per-agent credential " +
			"returned by register_agent, in the Authorization header. Carries no descriptor changes.",
	}, h.heartbeatTool)

	mcp.AddTool(server, &mcp.Tool{
		Name: "update_agent",
		Description: "Report a partial change to this agent's descriptor — only the fields " +
			"included are changed. Requires the per-agent credential in the Authorization header.",
	}, h.updateAgent)

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_agent",
		Description: "Look up one agent's current record. Requires either an enrollment token " +
			"scoped to that agent's team and environment, or the agent's own per-agent credential, " +
			"in the Authorization header.",
	}, h.getAgent)

	mcp.AddTool(server, &mcp.Tool{
		Name: "search_agents",
		Description: "List agents within an enrollment token's own team and environment, most " +
			"recently active first. Requires an enrollment token in the Authorization header.",
	}, h.searchAgents)

	return server
}

// bearerToken extracts the credential from req, wrapped so every handler
// below reports a failure the same way this plan's Task 1 already made HTTP
// and MCP agree on: auth.ErrNoBearerToken is part of service.AuthSentinels
// by construction, since it is the same sentinel internal/httpapi uses.
//
// req.Extra is nil over the in-memory transport (no real HTTP request sits
// behind it), which is exactly the case Task 3's tests construct by hand; a
// nil Extra is treated as an empty header set rather than a panic, and an
// empty header set is precisely what auth.BearerToken already reports as
// ErrNoBearerToken.
func bearerToken(req *mcp.CallToolRequest) (string, error) {
	header := http.Header{}
	if req.Extra != nil {
		header = req.Extra.Header
	}
	return auth.BearerToken(header)
}

// parseAgentID mirrors internal/httpapi/agents.go's agentID: an unparseable
// id is refused as an authentication failure rather than a distinct "bad
// input" error, for the same enumeration-oracle reason documented there — a
// caller who could tell "malformed id" apart from "well-formed id, wrong
// credential" would learn that the id's shape is checked before the
// credential is, which is exactly the side channel this system's closed,
// indistinguishable failure set exists to close.
func parseAgentID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: agent id is not a uuid: %v", auth.ErrNoBearerToken, err)
	}
	return id, nil
}

// --- register_agent -------------------------------------------------------

type registerAgentIn struct {
	// Descriptor is the agent's self-reported descriptor: name, purpose,
	// platform, hosting, source, telemetry_links, and any other fields —
	// identical in shape to the HTTP registration body. It is map[string]any
	// rather than json.RawMessage: this plan's research found that the
	// latter infers a byte-array schema, not an object schema, from the
	// SDK's automatic schema generation.
	Descriptor map[string]any `json:"descriptor" jsonschema:"the agent's self-reported descriptor, as a JSON object: name, purpose, platform, hosting, source, telemetry_links, and any other fields"`
}

type registerAgentOut struct {
	AgentID                  string   `json:"agent_id"`
	AgentToken               string   `json:"agent_token"`
	HeartbeatIntervalSeconds int      `json:"heartbeat_interval_seconds"`
	Flags                    []string `json:"flags"`
	Created                  bool     `json:"created"`
}

func (h *handlers) registerAgent(ctx context.Context, req *mcp.CallToolRequest, in registerAgentIn) (*mcp.CallToolResult, registerAgentOut, error) {
	token, err := bearerToken(req)
	if err != nil {
		return nil, registerAgentOut{}, err
	}

	raw, err := jsonMarshal(in.Descriptor)
	if err != nil {
		return nil, registerAgentOut{}, err
	}

	result, err := h.registration.Register(ctx, service.RegisterInput{
		EnrollmentToken: token,
		RawDescriptor:   raw,
	})
	if err != nil {
		return nil, registerAgentOut{}, err
	}

	return nil, registerAgentOut{
		AgentID:                  result.AgentID.String(),
		AgentToken:               result.AgentToken,
		HeartbeatIntervalSeconds: result.HeartbeatIntervalSeconds,
		Flags:                    jsonList(result.Flags),
		Created:                  result.Created,
	}, nil
}

// --- heartbeat --------------------------------------------------------------

type heartbeatIn struct {
	// AgentID is a string, not uuid.UUID: this plan's research found that
	// uuid.UUID's underlying [16]byte array infers an integer-array schema
	// from the SDK's reflection-based generator, not a string one, and a real
	// UUID string then fails schema validation. parseAgentID converts it.
	AgentID string `json:"agent_id" jsonschema:"the agent id returned by register_agent"`
}

type heartbeatOut struct {
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	LifecycleState           string `json:"lifecycle_state"`
	ReplicaCount             int    `json:"replica_count"`
}

func (h *handlers) heartbeatTool(ctx context.Context, req *mcp.CallToolRequest, in heartbeatIn) (*mcp.CallToolResult, heartbeatOut, error) {
	token, err := bearerToken(req)
	if err != nil {
		return nil, heartbeatOut{}, err
	}

	agentID, err := parseAgentID(in.AgentID)
	if err != nil {
		return nil, heartbeatOut{}, err
	}

	result, err := h.heartbeat.Heartbeat(ctx, service.HeartbeatInput{
		AgentID:    agentID,
		AgentToken: token,
	})
	if err != nil {
		return nil, heartbeatOut{}, err
	}

	return nil, heartbeatOut{
		HeartbeatIntervalSeconds: result.HeartbeatIntervalSeconds,
		LifecycleState:           string(result.LifecycleState),
		ReplicaCount:             result.ReplicaCount,
	}, nil
}

// --- update_agent -----------------------------------------------------------

type updateAgentIn struct {
	// AgentID is a string for the same reason heartbeatIn's is; see there.
	AgentID    string         `json:"agent_id" jsonschema:"the agent id returned by register_agent"`
	Descriptor map[string]any `json:"descriptor" jsonschema:"the fields to change; fields not included are left alone"`
}

type updateAgentOut struct {
	Flags   []string `json:"flags"`
	Changed bool     `json:"changed"`
}

func (h *handlers) updateAgent(ctx context.Context, req *mcp.CallToolRequest, in updateAgentIn) (*mcp.CallToolResult, updateAgentOut, error) {
	token, err := bearerToken(req)
	if err != nil {
		return nil, updateAgentOut{}, err
	}

	agentID, err := parseAgentID(in.AgentID)
	if err != nil {
		return nil, updateAgentOut{}, err
	}

	raw, err := jsonMarshal(in.Descriptor)
	if err != nil {
		return nil, updateAgentOut{}, err
	}

	result, err := h.update.Update(ctx, service.UpdateInput{
		AgentID:       agentID,
		AgentToken:    token,
		RawDescriptor: raw,
	})
	if err != nil {
		return nil, updateAgentOut{}, err
	}

	return nil, updateAgentOut{Flags: jsonList(result.Flags), Changed: result.Changed}, nil
}

// --- get_agent ----------------------------------------------------------

type getAgentIn struct {
	// AgentID is a string for the same reason heartbeatIn's is; see there.
	AgentID string `json:"agent_id" jsonschema:"the agent id to look up"`
}

type agentOut struct {
	ID                 string   `json:"id"`
	TeamSlug           string   `json:"team_slug"`
	Environment        string   `json:"environment"`
	DisplayName        string   `json:"display_name"`
	Purpose            string   `json:"purpose"`
	LifecycleState     string   `json:"lifecycle_state"`
	RegistrationSource string   `json:"registration_source"`
	ReplicaCount       int      `json:"replica_count"`
	Flags              []string `json:"flags"`
	FirstSeenAt        string   `json:"first_seen_at"`
	LastHeartbeatAt    string   `json:"last_heartbeat_at"`
}

func (h *handlers) getAgent(ctx context.Context, req *mcp.CallToolRequest, in getAgentIn) (*mcp.CallToolResult, agentOut, error) {
	token, err := bearerToken(req)
	if err != nil {
		return nil, agentOut{}, err
	}

	agentID, err := parseAgentID(in.AgentID)
	if err != nil {
		return nil, agentOut{}, err
	}

	agent, err := h.query.GetAgent(ctx, service.GetAgentInput{AgentID: agentID, Credential: token})
	if err != nil {
		return nil, agentOut{}, err
	}

	return nil, toAgentOut(agent), nil
}

// --- search_agents ----------------------------------------------------------

type searchAgentsIn struct {
	LifecycleState string `json:"lifecycle_state,omitempty" jsonschema:"filter to one lifecycle state: active, idle, stale, retired, or quarantined; omit for all"`
	Limit          int    `json:"limit,omitempty" jsonschema:"maximum number of agents to return; defaults to 50, capped at 200"`
}

type searchAgentsOut struct {
	Agents []agentOut `json:"agents"`
}

func (h *handlers) searchAgents(ctx context.Context, req *mcp.CallToolRequest, in searchAgentsIn) (*mcp.CallToolResult, searchAgentsOut, error) {
	token, err := bearerToken(req)
	if err != nil {
		return nil, searchAgentsOut{}, err
	}

	results, err := h.query.SearchAgents(ctx, service.SearchAgentsInput{
		EnrollmentToken: token,
		LifecycleState:  in.LifecycleState,
		Limit:           in.Limit,
	})
	if err != nil {
		return nil, searchAgentsOut{}, err
	}

	out := searchAgentsOut{Agents: make([]agentOut, len(results))}
	for i, agent := range results {
		out.Agents[i] = toAgentOut(agent)
	}
	return nil, out, nil
}

func toAgentOut(a service.Agent) agentOut {
	return agentOut{
		ID:                 a.ID.String(),
		TeamSlug:           a.TeamSlug,
		Environment:        a.Environment,
		DisplayName:        a.DisplayName,
		Purpose:            a.Purpose,
		LifecycleState:     a.LifecycleState,
		RegistrationSource: a.RegistrationSource,
		ReplicaCount:       a.ReplicaCount,
		Flags:              jsonList(a.Flags),
		FirstSeenAt:        a.FirstSeenAt.Format(timeFormat),
		LastHeartbeatAt:    a.LastHeartbeatAt.Format(timeFormat),
	}
}

const timeFormat = "2006-01-02T15:04:05Z07:00" // time.RFC3339

// jsonList turns a nil slice into an empty one, matching
// internal/httpapi/agents.go's jsonList: a field that is sometimes [] and
// sometimes null is a field every client has to special-case.
func jsonList(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// jsonMarshal turns a decoded descriptor argument back into the bytes
// domain.DecodeDescriptor and service.RegisterInput/UpdateInput already
// accept — the map[string]any round trip this plan's research required in
// place of json.RawMessage.
func jsonMarshal(v map[string]any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode descriptor: %w", err)
	}
	return raw, nil
}
```

**Step 4: Run and verify it passes**

Run: `go test ./internal/mcpapi/ -v`
Expected: PASS, all seven tests.

**Step 5: Run the full suite**

Run: `go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: everything passes.

**Step 6: Commit**

```bash
git add internal/mcpapi/
git commit -s -m "feat: implement the five agent-facing mcp tools"
```

---

## Task 4: Mount the MCP server and prove it end to end over real HTTP

**Files:**
- Modify: `cmd/aiinv/app.go`, `cmd/aiinv/main_test.go`

**Interfaces:**
- Consumes: `mcpapi.NewServer` (Task 3)
- Produces: `app.handler` now routes `/mcp` to the MCP server and everything else to the existing REST API, unchanged for any existing caller

**Step 1: Write the failing test**

This mirrors `TestAgentLifecycleOverHTTP` in the same file — mint a token the way the CLI does, then be an agent, this time over a real MCP client and a real streamable HTTP connection, with the credential carried as a header exactly as `internal/mcpapi`'s research established.

```go
// appended to cmd/aiinv/main_test.go
func TestAgentLifecycleOverMCP(t *testing.T) {
	cfg := testConfig(t, migratedDatabaseURL(t))
	ctx := context.Background()

	app, err := newApp(ctx, cfg, testLogger())
	require.NoError(t, err)
	defer app.close()

	minted, err := app.createToken(ctx, "finance-platform", "production", "mcp-smoke")
	require.NoError(t, err)

	server := httptest.NewServer(app.handler)
	defer server.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "smoke-client", Version: "v0.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   server.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRoundTripper{token: minted.Plaintext}},
	}
	session, err := client.Connect(ctx, transport, nil)
	require.NoError(t, err)
	defer session.Close()

	registerResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "register_agent",
		Arguments: map[string]any{
			"descriptor": map[string]any{
				"name":         "invoice-triage",
				"purpose":      "Triages accounts-payable invoices",
				"instance_key": "mcp-smoke-1",
				"team":         "somebody-elses-team",
				"environment":  "development",
			},
		},
	})
	require.NoError(t, err)
	require.False(t, registerResult.IsError, "%+v", registerResult)

	var registered struct {
		AgentID                  string   `json:"agent_id"`
		AgentToken               string   `json:"agent_token"`
		HeartbeatIntervalSeconds int      `json:"heartbeat_interval_seconds"`
		Flags                    []string `json:"flags"`
		Created                  bool     `json:"created"`
	}
	require.NoError(t, json.Unmarshal(structuredContentBytes(t, registerResult), &registered))
	require.True(t, registered.Created)
	require.Contains(t, registered.Flags, domain.FlagIgnoredScopeFields)

	// Reconnect as the agent: the agent's own credential authenticates
	// heartbeat and update_agent, not the enrollment token.
	agentTransport := &mcp.StreamableClientTransport{
		Endpoint:   server.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRoundTripper{token: registered.AgentToken}},
	}
	agentSession, err := client.Connect(ctx, agentTransport, nil)
	require.NoError(t, err)
	defer agentSession.Close()

	beatResult, err := agentSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "heartbeat",
		Arguments: map[string]any{"agent_id": registered.AgentID},
	})
	require.NoError(t, err)
	require.False(t, beatResult.IsError, "%+v", beatResult)

	updateResult, err := agentSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "update_agent",
		Arguments: map[string]any{
			"agent_id":   registered.AgentID,
			"descriptor": map[string]any{"purpose": "Triages and codes accounts-payable invoices"},
		},
	})
	require.NoError(t, err)
	require.False(t, updateResult.IsError, "%+v", updateResult)

	getResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_agent",
		Arguments: map[string]any{"agent_id": registered.AgentID},
	})
	require.NoError(t, err)
	require.False(t, getResult.IsError, "%+v", getResult)

	var fetched struct {
		DisplayName string `json:"display_name"`
		Purpose     string `json:"purpose"`
		TeamSlug    string `json:"team_slug"`
		Environment string `json:"environment"`
	}
	require.NoError(t, json.Unmarshal(structuredContentBytes(t, getResult), &fetched))
	require.Equal(t, "invoice-triage", fetched.DisplayName)
	require.Equal(t, "Triages and codes accounts-payable invoices", fetched.Purpose)
	require.Equal(t, "finance-platform", fetched.TeamSlug, "ownership must come from the token")
	require.Equal(t, "production", fetched.Environment, "the environment must come from the token")
}

type bearerRoundTripper struct{ token string }

func (rt bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+rt.token)
	return http.DefaultTransport.RoundTrip(req)
}

func structuredContentBytes(t *testing.T, res *mcp.CallToolResult) []byte {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	return raw
}
```

Add `"github.com/modelcontextprotocol/go-sdk/mcp"` to `cmd/aiinv/main_test.go`'s import block.

**Step 2: Run and verify it fails**

Run: `go test ./cmd/aiinv/ -run TestAgentLifecycleOverMCP -v`
Expected: FAIL — no MCP endpoint exists at `/mcp` yet (404 from the existing catch-all route, surfaced as a client connect error).

**Step 3: Mount the handler**

In `cmd/aiinv/app.go`, add the import `"github.com/ebellefontaine/ai-inventory/internal/mcpapi"` and `"github.com/modelcontextprotocol/go-sdk/mcp"`, then change the handler construction in `newApp`:

```go
tokens := service.NewTokenService(pool)
registration := service.NewRegistrationService(pool, tokens, cfg.HeartbeatIntervalSeconds)
heartbeat := service.NewHeartbeatService(pool, cfg.HeartbeatIntervalSeconds)
update := service.NewUpdateService(pool)
query := service.NewQueryService(pool, tokens)

mcpServer := mcpapi.NewServer(registration, heartbeat, update, query)
mcpHandler := mcp.NewStreamableHTTPHandler(
	func(*http.Request) *mcp.Server { return mcpServer },
	&mcp.StreamableHTTPOptions{Stateless: true},
)

mux := http.NewServeMux()
mux.Handle("/mcp", mcpHandler)
mux.Handle("/", httpapi.NewServer(logger, registration, heartbeat, update).Handler())

return &app{
	cfg:       cfg,
	logger:    logger,
	pool:      pool,
	handler:   mux,
	tokens:    tokens,
	bootstrap: bootstrap,
	orgID:     orgID,
	worker: worker.New(pool, worker.Config{
		HeartbeatIntervalSeconds: cfg.HeartbeatIntervalSeconds,
		IdleAfterMissedBeats:     cfg.IdleAfterMissedBeats,
		StaleAfterDays:           cfg.StaleAfterDays,
	}),
}, nil
```

`mux.Handle("/mcp", ...)` matches the exact path `/mcp` only; `mux.Handle("/", ...)` remains the catch-all for everything else, including `/healthz` and every REST route, exactly as before. Go's `ServeMux` always prefers the more specific pattern, so this ordering is not load-bearing, but the handler is registered first here because it is new.

**Step 4: Run and verify it passes**

Run: `go test ./cmd/aiinv/ -run TestAgentLifecycleOverMCP -v`
Expected: PASS.

**Step 5: Run the full suite**

Run: `go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: everything passes, including `TestAgentLifecycleOverHTTP` and `TestHealthzOverRealWiring` — the REST surface must be entirely unaffected by this change.

**Step 6: Commit**

```bash
git add cmd/aiinv/
git commit -s -m "feat: mount the mcp server alongside the http api"
```

---

## Task 5: Run the contract suite against the MCP surface

Task 24 of the phase-1 plan built `spec/conformance/cases.json` and an HTTP runner specifically so that "the runner executes them against the HTTP surface here, and against the MCP surface in Milestone 4 — from the same file." This task is that promise kept: a second runner, same file, unchanged.

**Files:**
- Create: `internal/mcpapi/contract_test.go`

**Interfaces:**
- Consumes: `spec/conformance/cases.json` (unchanged — its symbolic `op`/`auth`/`status` vocabulary was designed in Task 24 specifically to not name an HTTP concept)

**Step 1: Design note — mapping symbolic outcomes onto MCP's shape**

`internal/httpapi/contract_test.go`'s runner maps a case's symbolic `status` (`"created"`, `"ok"`, `"unauthorized"`, `"bad_request"`, `"payload_too_large"`) onto an HTTP status code. MCP tool calls have no status code: per this plan's research, an error returned from a tool handler is packed into `CallToolResult` with `IsError: true` rather than failing the call at the protocol level (`ClientSession.CallTool` itself only returns a non-nil `error` for a transport or protocol failure, which none of these cases exercise). The MCP runner therefore collapses the five symbolic statuses to two outcomes: `"created"` and `"ok"` mean `IsError` must be `false`; everything else means it must be `true`. The finer distinction the HTTP runner draws between, say, `unauthorized` and `bad_request` is not observable at this layer and is not claimed by any case in the file — every current case's failure path is `"unauthorized"`.

**Step 2: Write the failing test**

```go
// internal/mcpapi/contract_test.go
package mcpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/domain"
)

// TestConformance is the MCP counterpart to internal/httpapi's runner of the
// same name, over the same spec/conformance/cases.json. See that file's
// TestConformance for what a case and a step mean; this runner reuses the
// exact same types by re-declaring them (an unexported type cannot cross a
// package boundary), and reuses the exact same JSON, unmodified.
func TestConformance(t *testing.T) {
	raw, err := os.ReadFile("../../spec/conformance/cases.json")
	require.NoError(t, err, "read contract fixture set")

	var suite conformanceSuite
	require.NoError(t, json.Unmarshal(raw, &suite), "parse contract fixture set")
	require.NotEmpty(t, suite.Cases, "the contract suite must not be empty")

	for _, c := range suite.Cases {
		t.Run(c.ID, func(t *testing.T) {
			runMCPContractCase(t, c)
		})
	}
}
```

**Step 2: Run and verify it fails**

Run: `go test ./internal/mcpapi/ -run TestConformance -v`
Expected: FAIL — `undefined: conformanceSuite` and the rest of the runner.

**Step 3: Implement**

Re-declare the same case/step/expectation/store-check types `internal/httpapi/contract_test.go` defines — they are pure data shapes with no dependency on either transport, so the duplication is the same kind Task 3 already accepted for the fixture-building helpers, for the same reason. Copy these five declarations verbatim from `internal/httpapi/contract_test.go` (`conformanceSuite`, `knownGap`, `contractCase`, `contractStep`, `stepExpectation` including its `UnmarshalJSON` method, and `storeCheck`):

```go
type conformanceSuite struct {
	Cases     []contractCase `json:"cases"`
	KnownGaps []knownGap     `json:"known_gaps"`
}

type knownGap struct {
	Requirement string `json:"requirement"`
	Description string `json:"description"`
}

type contractCase struct {
	ID          string         `json:"id"`
	Requirement string         `json:"requirement"`
	Description string         `json:"description"`
	Steps       []contractStep `json:"steps"`
	StoreChecks []storeCheck   `json:"store_checks"`
}

type contractStep struct {
	Op     string          `json:"op"`
	Auth   string          `json:"auth"`
	Agent  string          `json:"agent"`
	Body   json.RawMessage `json:"body"`
	SaveAs string          `json:"save_as"`
	Expect stepExpectation `json:"expect"`
}

type stepExpectation struct {
	Status                                string   `json:"status"`
	Created                               *bool    `json:"created"`
	Changed                               *bool    `json:"changed"`
	AgentTokenPresent                     bool     `json:"agent_token_present"`
	HeartbeatIntervalPositive             bool     `json:"heartbeat_interval_positive"`
	AgentIDMatches                        string   `json:"agent_id_matches"`
	FlagsInclude                          []string `json:"flags_include"`
	FlagsExclude                          []string `json:"flags_exclude"`
	FlagsEqual                            []string `json:"flags_equal"`
	FlagsEqualSet                         bool     `json:"-"`
	LifecycleState                        string   `json:"lifecycle_state"`
	ReplicaCount                          *int     `json:"replica_count"`
	HeartbeatIntervalMatchesRegistration  string   `json:"heartbeat_interval_matches_registration"`
}

func (e *stepExpectation) UnmarshalJSON(data []byte) error {
	type alias stepExpectation
	var raw struct {
		alias
		FlagsEqual *[]string `json:"flags_equal"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*e = stepExpectation(raw.alias)
	if raw.FlagsEqual != nil {
		e.FlagsEqual = *raw.FlagsEqual
		e.FlagsEqualSet = true
	}
	return nil
}

type storeCheck struct {
	Agent                     string   `json:"agent"`
	TeamEqualsTokenTeam       bool     `json:"team_equals_token_team"`
	EnvironmentEqualsTokenEnv bool     `json:"environment_equals_token_environment"`
	DisplayName               string   `json:"display_name"`
	Purpose                   string   `json:"purpose"`
	PlatformContains          []string `json:"platform_contains"`
	HostingContains           []string `json:"hosting_contains"`
	SourceContains            []string `json:"source_contains"`
	TelemetryContains         []string `json:"telemetry_contains"`
	AttributesContains        []string `json:"attributes_contains"`
	RevisionCount             *int     `json:"revision_count"`
}
```

Then the fixture and runner. A `ClientSession` in this SDK is not re-authenticatable per call — the bearer header lives on the `http.Client` the transport was built with, not on an individual `CallTool` invocation — so unlike `internal/httpapi/contract_test.go`'s single `apiFixture.send`, this fixture connects **fresh for every call**, each with the credential that step needs on its transport's `http.Client`, exactly as `cmd/aiinv/main_test.go`'s `TestAgentLifecycleOverMCP` already does with its separate `session` and `agentSession`:

```go
type mcpContractFixture struct {
	pool       *pgxpool.Pool
	url        string
	enrollment string
}

func newMCPContractFixture(t *testing.T) mcpContractFixture {
	t.Helper()
	f := newMCPFixture(t)

	server := NewServer(f.registration, f.heartbeat, f.update, f.query)
	httpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	ts := httptest.NewServer(httpHandler)
	t.Cleanup(ts.Close)

	return mcpContractFixture{pool: f.pool, url: ts.URL, enrollment: f.enrollment}
}

// call connects fresh with token as the bearer credential, invokes name once,
// and disconnects. A fresh connection per call is wasteful compared to
// reusing a session across an agent's whole lifetime, but a contract case
// authenticates as a different identity (the enrollment token, then a
// freshly issued agent credential) from one step to the next, and this SDK
// carries the credential on the transport's http.Client rather than on
// CallToolParams — there is no cheaper way to change it mid-session.
func (f mcpContractFixture) call(t *testing.T, token, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()

	client := mcp.NewClient(&mcp.Implementation{Name: "contract-runner", Version: "v0.0.0"}, nil)
	rt := http.DefaultTransport
	if token != "" {
		rt = bearerRoundTripper{token: token, inner: http.DefaultTransport}
	}
	transport := &mcp.StreamableClientTransport{Endpoint: f.url, HTTPClient: &http.Client{Transport: rt}}

	session, err := client.Connect(ctx, transport, nil)
	require.NoError(t, err)
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err, "tool call transport failure")
	return res
}

type bearerRoundTripper struct {
	token string
	inner http.RoundTripper
}

func (rt bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+rt.token)
	return rt.inner.RoundTrip(req)
}

type mcpSavedAgent struct {
	id                string
	token             string
	heartbeatInterval int
}

func runMCPContractCase(t *testing.T, c contractCase) {
	t.Helper()
	f := newMCPContractFixture(t)
	saved := map[string]mcpSavedAgent{}

	for i, step := range c.Steps {
		t.Run(fmt.Sprintf("step %d %s", i+1, step.Op), func(t *testing.T) {
			runMCPContractStep(t, f, saved, step)
		})
	}

	for _, check := range c.StoreChecks {
		name := check.Agent
		if name == "" {
			name = "self"
		}
		agent, ok := saved[name]
		require.True(t, ok, "store check references agent %q, which no step saved", name)
		verifyMCPStoreCheck(t, f, agent, check)
	}
}

func runMCPContractStep(t *testing.T, f mcpContractFixture, saved map[string]mcpSavedAgent, step contractStep) {
	t.Helper()

	token := resolveMCPAuth(t, f, saved, step)
	wantSuccess := step.Expect.Status == "created" || step.Expect.Status == "ok"

	switch step.Op {
	case "register":
		var body map[string]any
		require.NoError(t, json.Unmarshal(step.Body, &body))

		res := f.call(t, token, "register_agent", map[string]any{"descriptor": body})
		require.Equal(t, wantSuccess, !res.IsError, "content: %+v", res.Content)
		if res.IsError {
			return
		}

		var out registerAgentOut
		decodeStructured(t, res, &out)

		if step.Expect.Created != nil {
			require.Equal(t, *step.Expect.Created, out.Created)
		}
		if step.Expect.AgentTokenPresent {
			require.NotEmpty(t, out.AgentToken)
		}
		if step.Expect.HeartbeatIntervalPositive {
			require.Positive(t, out.HeartbeatIntervalSeconds)
		}
		if step.Expect.AgentIDMatches != "" {
			require.Equal(t, saved[step.Expect.AgentIDMatches].id, out.AgentID)
		}
		verifyMCPFlags(t, step.Expect, out.Flags)

		if step.SaveAs != "" {
			saved[step.SaveAs] = mcpSavedAgent{id: out.AgentID, token: out.AgentToken, heartbeatInterval: out.HeartbeatIntervalSeconds}
		}

	case "heartbeat":
		agent := requireMCPSavedAgent(t, saved, step.Agent)
		res := f.call(t, token, "heartbeat", map[string]any{"agent_id": agent.id})
		require.Equal(t, wantSuccess, !res.IsError, "content: %+v", res.Content)
		if res.IsError {
			return
		}

		var out heartbeatOut
		decodeStructured(t, res, &out)

		if step.Expect.LifecycleState != "" {
			require.Equal(t, step.Expect.LifecycleState, out.LifecycleState)
		}
		if step.Expect.ReplicaCount != nil {
			require.Equal(t, *step.Expect.ReplicaCount, out.ReplicaCount)
		}
		if step.Expect.HeartbeatIntervalMatchesRegistration != "" {
			want := requireMCPSavedAgent(t, saved, step.Expect.HeartbeatIntervalMatchesRegistration).heartbeatInterval
			require.Equal(t, want, out.HeartbeatIntervalSeconds)
		}

	case "update":
		agent := requireMCPSavedAgent(t, saved, step.Agent)
		var body map[string]any
		require.NoError(t, json.Unmarshal(step.Body, &body))

		res := f.call(t, token, "update_agent", map[string]any{"agent_id": agent.id, "descriptor": body})
		require.Equal(t, wantSuccess, !res.IsError, "content: %+v", res.Content)
		if res.IsError {
			return
		}

		var out updateAgentOut
		decodeStructured(t, res, &out)

		if step.Expect.Changed != nil {
			require.Equal(t, *step.Expect.Changed, out.Changed)
		}
		verifyMCPFlags(t, step.Expect, out.Flags)

	default:
		t.Fatalf("unknown contract step op %q", step.Op)
	}
}

func decodeStructured(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, out), "structured content: %s", raw)
}

func requireMCPSavedAgent(t *testing.T, saved map[string]mcpSavedAgent, name string) mcpSavedAgent {
	t.Helper()
	agent, ok := saved[name]
	require.True(t, ok, "step references agent %q, which no earlier step saved", name)
	return agent
}

func resolveMCPAuth(t *testing.T, f mcpContractFixture, saved map[string]mcpSavedAgent, step contractStep) string {
	t.Helper()
	switch step.Auth {
	case "enrollment_token":
		return f.enrollment
	case "agent_token":
		return requireMCPSavedAgent(t, saved, step.Agent).token
	case "bogus_token":
		return "aiinv_ac_thiscredentialdoesnotexist"
	case "none", "":
		return ""
	default:
		t.Fatalf("unknown contract step auth %q", step.Auth)
		return ""
	}
}

func verifyMCPFlags(t *testing.T, expect stepExpectation, got []string) {
	t.Helper()
	for _, want := range expect.FlagsInclude {
		require.Contains(t, got, want)
	}
	for _, unwanted := range expect.FlagsExclude {
		require.NotContains(t, got, unwanted)
	}
	if expect.FlagsEqualSet {
		require.ElementsMatch(t, expect.FlagsEqual, got)
	}
}

func verifyMCPStoreCheck(t *testing.T, f mcpContractFixture, agent mcpSavedAgent, check storeCheck) {
	t.Helper()
	id, err := uuid.Parse(agent.id)
	require.NoError(t, err)

	var (
		teamID                                                                    *uuid.UUID
		environment, displayName, purpose, platform, hosting, source, telemetry, attributes string
		revisionCount                                                              int
	)
	require.NoError(t, f.pool.QueryRow(context.Background(), `
		SELECT team_id, environment, display_name, purpose,
		       platform::text, hosting::text, source::text,
		       telemetry_links::text, attributes::text
		  FROM agents WHERE id = $1`, id,
	).Scan(&teamID, &environment, &displayName, &purpose, &platform, &hosting, &source, &telemetry, &attributes))
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_revisions WHERE agent_id = $1`, id,
	).Scan(&revisionCount))

	if check.TeamEqualsTokenTeam {
		require.NotNil(t, teamID)
	}
	if check.EnvironmentEqualsTokenEnv {
		require.Equal(t, string(domain.EnvironmentProduction), environment)
	}
	if check.DisplayName != "" {
		require.Equal(t, check.DisplayName, displayName)
	}
	if check.Purpose != "" {
		require.Equal(t, check.Purpose, purpose)
	}
	for _, want := range check.PlatformContains {
		require.Contains(t, platform, want)
	}
	for _, want := range check.HostingContains {
		require.Contains(t, hosting, want)
	}
	for _, want := range check.SourceContains {
		require.Contains(t, source, want)
	}
	for _, want := range check.TelemetryContains {
		require.Contains(t, telemetry, want)
	}
	for _, want := range check.AttributesContains {
		require.Contains(t, attributes, want)
	}
	if check.RevisionCount != nil {
		require.Equal(t, *check.RevisionCount, revisionCount)
	}
}
```

Add `"github.com/jackc/pgx/v5/pgxpool"` to the import block for `mcpContractFixture.pool`'s type.

**Step 4: Run and verify it passes**

Run: `go test ./internal/mcpapi/ -run TestConformance -v`
Expected: PASS, all thirteen cases — the same thirteen `internal/httpapi`'s runner already passes, from the same unmodified file.

**Step 5: Falsify it, the same way Task 24 falsified the HTTP runner**

Temporarily edit one case's `flags_equal` in `spec/conformance/cases.json` to something wrong (as Task 24's own history did), confirm `go test ./internal/mcpapi/ -run TestConformance` fails, then revert the edit. This is not optional: a contract runner that cannot be shown to fail against a wrong fixture is not evidence that it checks anything.

**Step 6: Run the full suite**

Run: `go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: everything passes.

**Step 7: Commit**

```bash
git add internal/mcpapi/contract_test.go
git commit -s -m "test: run the contract fixture set against the mcp surface

Same spec/conformance/cases.json Task 24 wrote for the HTTP runner, unchanged
-- the mechanism the phase-1 plan promised would stop the two surfaces from
telling an agent different things."
```

---

## Milestone 4 exit criteria

An agent can add this server to its MCP client configuration, call `register_agent` with an enrollment token, and appear in the inventory exactly as it would over HTTP — same identity rules, same flags, same ownership-from-the-token guarantee. Demo it: mint a token with the CLI, connect an MCP client (or `npx @modelcontextprotocol/inspector` pointed at `http://localhost:8080/mcp` with the token as a bearer header) and call `register_agent`, `heartbeat`, `update_agent`, `get_agent`, `search_agents` in turn.

## Deferred to Milestone 5

- Real operator identity and roles for `get_agent`/`search_agents` (this plan's scope is deliberately the narrowest honest thing bare credentials can support).
- An HTTP admin endpoint for listing/searching agents (Milestone 5's "agent list with filters").
- `Server.RemoveTools` / capability changes for revoked or retired agents — not needed yet; nothing in this milestone changes the tool list at runtime.
