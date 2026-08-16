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
	"github.com/jackc/pgx/v5/pgxpool"
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
	suite := loadMCPConformanceSuite(t)
	require.NotEmpty(t, suite.Cases, "the contract suite must not be empty")

	for _, c := range suite.Cases {
		t.Run(c.ID, func(t *testing.T) {
			runMCPContractCase(t, c)
		})
	}
}

// TestConformanceKnownGaps is the MCP counterpart to
// internal/httpapi/contract_test.go's test of the same name: it surfaces
// every acceptance criterion the shared fixture cannot yet exercise, as a
// named, visible skip on this surface too, rather than letting the MCP
// runner silently say nothing about a gap the HTTP runner already declares.
func TestConformanceKnownGaps(t *testing.T) {
	suite := loadMCPConformanceSuite(t)
	for _, gap := range suite.KnownGaps {
		t.Run(gap.Requirement, func(t *testing.T) {
			t.Skip(gap.Description)
		})
	}
}

func loadMCPConformanceSuite(t *testing.T) conformanceSuite {
	t.Helper()
	raw, err := os.ReadFile("../../spec/conformance/cases.json")
	require.NoError(t, err, "read contract fixture set")

	var suite conformanceSuite
	require.NoError(t, json.Unmarshal(raw, &suite), "parse contract fixture set")
	return suite
}

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
	Status                               string   `json:"status"`
	Created                              *bool    `json:"created"`
	Changed                              *bool    `json:"changed"`
	AgentTokenPresent                    bool     `json:"agent_token_present"`
	HeartbeatIntervalPositive            bool     `json:"heartbeat_interval_positive"`
	AgentIDMatches                       string   `json:"agent_id_matches"`
	FlagsInclude                         []string `json:"flags_include"`
	FlagsExclude                         []string `json:"flags_exclude"`
	FlagsEqual                           []string `json:"flags_equal"`
	FlagsEqualSet                        bool     `json:"-"`
	LifecycleState                       string   `json:"lifecycle_state"`
	ReplicaCount                         *int     `json:"replica_count"`
	HeartbeatIntervalMatchesRegistration string   `json:"heartbeat_interval_matches_registration"`
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
		teamID                                                                              *uuid.UUID
		environment, displayName, purpose, platform, hosting, source, telemetry, attributes string
		revisionCount                                                                       int
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
