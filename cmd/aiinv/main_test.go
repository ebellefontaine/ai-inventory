package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/config"
	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// migratedDatabaseURL is a database testdb has already migrated. It is what
// most tests here want: they are about the wiring, not about goose.
func migratedDatabaseURL(t *testing.T) string {
	t.Helper()
	return testdb.New(t).Config().ConnString()
}

// unmigratedDatabaseURL is an empty database with no schema at all, created
// inside the same container. It exists so the migration test can fail: run
// against testdb's already-migrated clone, an assertion that the schema exists
// would pass without newApp having done anything.
func unmigratedDatabaseURL(t *testing.T) string {
	t.Helper()

	pool := testdb.New(t)
	name := "fresh_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	_, err := pool.Exec(context.Background(),
		"CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err, "create unmigrated database")

	u, err := url.Parse(pool.Config().ConnString())
	require.NoError(t, err)
	u.Path = "/" + name
	return u.String()
}

// testConfig runs the real config loader so the defaults under test are the
// ones a real start would use, with the listen address pinned to a port the
// kernel picks.
func testConfig(t *testing.T, databaseURL string) config.Config {
	t.Helper()

	cfg, err := config.Load(func(key string) (string, bool) {
		switch key {
		case "AIINV_DATABASE_URL":
			return databaseURL, true
		case "AIINV_LISTEN_ADDR":
			return "127.0.0.1:0", true
		}
		return "", false
	})
	require.NoError(t, err)
	return cfg
}

func openPool(t *testing.T, databaseURL string) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxpool.New(context.Background(), databaseURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// TestNewAppMigratesAFreshDatabase is NFR-7 in one assertion: a self-hosted
// install is the binary and a connection string, with no schema step in
// between.
func TestNewAppMigratesAFreshDatabase(t *testing.T) {
	databaseURL := unmigratedDatabaseURL(t)
	ctx := context.Background()

	// The database really is empty before newApp touches it, or the assertion
	// below would be about goose having run in testdb rather than here.
	fresh := openPool(t, databaseURL)
	var before *string
	require.NoError(t, fresh.QueryRow(ctx, `SELECT to_regclass('public.agents')::text`).Scan(&before))
	require.Nil(t, before, "the database must start with no schema")

	app, err := newApp(ctx, testConfig(t, databaseURL), testLogger())
	require.NoError(t, err)
	defer app.close()

	var after *string
	require.NoError(t, fresh.QueryRow(ctx, `SELECT to_regclass('public.agents')::text`).Scan(&after))
	require.NotNil(t, after, "newApp must have applied the migrations")
}

// TestNewAppBootstrapsOneOrganisation: starting twice is the ordinary case —
// a restart, a rolling deploy, two replicas — and it must not found a second
// installation or fail on the unique slug.
func TestNewAppBootstrapsOneOrganisation(t *testing.T) {
	cfg := testConfig(t, migratedDatabaseURL(t))
	ctx := context.Background()

	first, err := newApp(ctx, cfg, testLogger())
	require.NoError(t, err)
	defer first.close()

	second, err := newApp(ctx, cfg, testLogger())
	require.NoError(t, err)
	defer second.close()

	require.Equal(t, first.orgID, second.orgID)

	var orgs int
	require.NoError(t, first.pool.QueryRow(ctx,
		`SELECT count(*) FROM organizations`).Scan(&orgs))
	require.Equal(t, 1, orgs)
}

// TestCreateTokenCreatesTeamThenReusesIt: a token names the team it is scoped
// to, and on a fresh install that team does not exist yet. The second token for
// the same team must land on the same scope, or two rollouts of one team's
// agents would show up as two teams' inventories.
func TestCreateTokenCreatesTeamThenReusesIt(t *testing.T) {
	cfg := testConfig(t, migratedDatabaseURL(t))
	ctx := context.Background()

	app, err := newApp(ctx, cfg, testLogger())
	require.NoError(t, err)
	defer app.close()

	first, err := app.createToken(ctx, "finance-platform", "production", "rollout-1")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(first.Plaintext, "aiinv_et"), "got %q", first.Plaintext)

	second, err := app.createToken(ctx, "finance-platform", "production", "rollout-2")
	require.NoError(t, err)

	var teams int
	require.NoError(t, app.pool.QueryRow(ctx,
		`SELECT count(*) FROM teams`).Scan(&teams))
	require.Equal(t, 1, teams, "the second token must reuse the team the first one created")

	require.Equal(t, tokenTeam(t, app.pool, first.ID), tokenTeam(t, app.pool, second.ID))

	// A different team is a different scope, created on demand in its turn.
	_, err = app.createToken(ctx, "data-science", "staging", "rollout-3")
	require.NoError(t, err)
	require.NoError(t, app.pool.QueryRow(ctx, `SELECT count(*) FROM teams`).Scan(&teams))
	require.Equal(t, 2, teams)
}

// TestCreateTokenRefusesAnInvalidEnvironment: the typo is caught before a team
// is founded for it, so a mistyped environment does not leave a permanent row
// behind as a souvenir.
func TestCreateTokenRefusesAnInvalidEnvironment(t *testing.T) {
	cfg := testConfig(t, migratedDatabaseURL(t))
	ctx := context.Background()

	app, err := newApp(ctx, cfg, testLogger())
	require.NoError(t, err)
	defer app.close()

	_, err = app.createToken(ctx, "finance-platform", "prod", "typo")
	require.ErrorIs(t, err, service.ErrInvalidEnvironment)

	var teams int
	require.NoError(t, app.pool.QueryRow(ctx, `SELECT count(*) FROM teams`).Scan(&teams))
	require.Equal(t, 0, teams, "a refused mint must not found a team")
}

func tokenTeam(t *testing.T, pool *pgxpool.Pool, tokenID uuid.UUID) uuid.UUID {
	t.Helper()

	var teamID uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT team_id FROM enrollment_tokens WHERE id = $1`, tokenID).Scan(&teamID))
	return teamID
}

// TestHealthzOverRealWiring: the probe an orchestrator uses to decide this
// process is alive, answered by the handler newApp actually builds rather than
// by a router assembled in a test.
func TestHealthzOverRealWiring(t *testing.T) {
	cfg := testConfig(t, migratedDatabaseURL(t))

	app, err := newApp(context.Background(), cfg, testLogger())
	require.NoError(t, err)
	defer app.close()

	server := httptest.NewServer(app.handler)
	defer server.Close()

	res, err := http.Get(server.URL + "/healthz")
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	require.Equal(t, http.StatusOK, res.StatusCode)
}

// TestAgentLifecycleOverHTTP is the whole product in one test: mint a token the
// way the CLI does, then be an agent — register, beat, correct the descriptor —
// over a real socket, and check the inventory says what it should afterwards.
//
// Nothing is faked. The database is real, the listener is real, the credential
// returned by registration is the one the later calls authenticate with, and
// the ownership assertion at the end is the product's central claim: the team
// and environment came from the token, not from anything the agent sent.
func TestAgentLifecycleOverHTTP(t *testing.T) {
	cfg := testConfig(t, migratedDatabaseURL(t))
	ctx := context.Background()

	app, err := newApp(ctx, cfg, testLogger())
	require.NoError(t, err)
	defer app.close()

	minted, err := app.createToken(ctx, "finance-platform", "production", "smoke")
	require.NoError(t, err)

	server := httptest.NewServer(app.handler)
	defer server.Close()

	// Register. The payload claims a team and an environment of its own, which
	// must be ignored and flagged rather than honoured.
	var registered struct {
		AgentID                  uuid.UUID `json:"agent_id"`
		AgentToken               string    `json:"agent_token"`
		HeartbeatIntervalSeconds int       `json:"heartbeat_interval_seconds"`
		Flags                    []string  `json:"flags"`
		Created                  bool      `json:"created"`
	}
	status := call(t, http.MethodPost, server.URL+"/api/v1/agents:register", minted.Plaintext, `{
		"name": "invoice-triage",
		"purpose": "Triages accounts-payable invoices",
		"instance_key": "invoice-triage-1",
		"team": "somebody-elses-team",
		"environment": "development",
		"platform": {"framework": "langgraph", "framework_version": "0.2.1"}
	}`, &registered)

	require.Equal(t, http.StatusCreated, status)
	require.NotEmpty(t, registered.AgentToken)
	require.True(t, registered.Created)
	require.Equal(t, cfg.HeartbeatIntervalSeconds, registered.HeartbeatIntervalSeconds)
	require.Contains(t, registered.Flags, domain.FlagIgnoredScopeFields)

	// Heartbeat with the credential registration issued.
	var beat struct {
		HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
		LifecycleState           string `json:"lifecycle_state"`
		ReplicaCount             int    `json:"replica_count"`
	}
	status = call(t, http.MethodPost,
		fmt.Sprintf("%s/api/v1/agents/%s/heartbeat", server.URL, registered.AgentID),
		registered.AgentToken, "", &beat)

	require.Equal(t, http.StatusOK, status)
	require.Equal(t, string(domain.LifecycleActive), beat.LifecycleState)
	require.Equal(t, 1, beat.ReplicaCount)

	// Correct the descriptor.
	var patched struct {
		Flags   []string `json:"flags"`
		Changed bool     `json:"changed"`
	}
	status = call(t, http.MethodPatch,
		fmt.Sprintf("%s/api/v1/agents/%s", server.URL, registered.AgentID),
		registered.AgentToken,
		`{"purpose": "Triages and codes accounts-payable invoices"}`, &patched)

	require.Equal(t, http.StatusOK, status)
	require.True(t, patched.Changed)

	// What the inventory now holds.
	var (
		displayName, purpose, environment, lifecycle, registrationSource string
		orgSlug, teamSlug                                                string
		replicas                                                         int
	)
	require.NoError(t, app.pool.QueryRow(ctx, `
		SELECT a.display_name, a.purpose, a.environment, a.lifecycle_state,
		       a.registration_source, a.replica_count, o.slug, t.slug
		  FROM agents a
		  JOIN organizations o ON o.id = a.org_id
		  JOIN teams t         ON t.id = a.team_id
		 WHERE a.id = $1`, registered.AgentID,
	).Scan(&displayName, &purpose, &environment, &lifecycle,
		&registrationSource, &replicas, &orgSlug, &teamSlug))

	require.Equal(t, "invoice-triage", displayName)
	require.Equal(t, "Triages and codes accounts-payable invoices", purpose)
	require.Equal(t, string(domain.LifecycleActive), lifecycle)
	require.Equal(t, string(domain.SourceSelfRegistered), registrationSource)
	require.Equal(t, 1, replicas)
	require.Equal(t, service.DefaultOrgSlug, orgSlug)
	require.Equal(t, "finance-platform", teamSlug, "ownership must come from the token")
	require.Equal(t, "production", environment, "the environment must come from the token")
}

// call sends one request with a bearer credential and decodes the response into
// out when out is non-nil. It returns the status code.
func call(t *testing.T, method, target, token, body string, out any) int {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, target, reader)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	if out != nil {
		require.NoError(t, json.Unmarshal(raw, out), "body: %s", raw)
	}
	return res.StatusCode
}

// TestServeWithoutDatabaseURLFailsClearly: the first thing a new operator gets
// wrong, and the message has to name the variable rather than surface as a nil
// pool three layers down.
func TestServeWithoutDatabaseURLFailsClearly(t *testing.T) {
	empty := func(string) (string, bool) { return "", false }

	err := run(context.Background(), []string{"serve"}, empty, io.Discard, io.Discard)

	require.Error(t, err)
	require.Contains(t, err.Error(), "AIINV_DATABASE_URL")
}

// TestGracefulShutdown: a cancelled context has to stop the listener, drain
// what is in flight, and come back without an error, because that is what the
// process's exit code is derived from. close is called twice on purpose: the
// shutdown path and a caller's defer both run in the real binary.
func TestGracefulShutdown(t *testing.T) {
	cfg := testConfig(t, migratedDatabaseURL(t))

	app, err := newApp(context.Background(), cfg, testLogger())
	require.NoError(t, err)

	listener, err := net.Listen("tcp", cfg.ListenAddr)
	require.NoError(t, err)
	addr := listener.Addr().String()

	serveCtx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- app.serve(serveCtx, listener) }()

	// Prove it is actually serving before asking it to stop.
	requireEventually(t, func() bool {
		res, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		_ = res.Body.Close()
		return res.StatusCode == http.StatusOK
	})

	stop()

	select {
	case err := <-served:
		require.NoError(t, err, "a graceful shutdown must not report failure")
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not return after the context was cancelled")
	}

	_, err = http.Get("http://" + addr + "/healthz")
	require.Error(t, err, "the listener must be closed after shutdown")

	app.close()
	app.close()
}

func requireEventually(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// TestUsageIsNotAnError: `aiinv --help` is a request that was answered, not a
// failure, and an operator's shell should see exit 0.
func TestUsageIsNotAnError(t *testing.T) {
	var out bytes.Buffer
	empty := func(string) (string, bool) { return "", false }

	require.NoError(t, run(context.Background(), []string{"--help"}, empty, &out, io.Discard))
	require.Contains(t, out.String(), "token create")
}

// TestAgentLifecycleOverMCP mirrors TestAgentLifecycleOverHTTP but drives the
// same lifecycle through a real MCP client over a real streamable HTTP
// connection, proving the MCP surface mounted alongside the REST API
// authenticates and behaves the same way.
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

// TestUnknownSubcommandFails: a mistyped subcommand must not fall through to
// serve, which would start a server the operator did not ask for.
func TestUnknownSubcommandFails(t *testing.T) {
	empty := func(string) (string, bool) { return "", false }

	err := run(context.Background(), []string{"srve"}, empty, io.Discard, io.Discard)

	require.Error(t, err)
	require.Contains(t, err.Error(), "srve")
}
