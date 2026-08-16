package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/domain"
)

// TestConformance is the runner for spec/conformance/cases.json.
//
// The cases are declarative on purpose: they describe what FUNCTIONAL.md
// section 5 promises an agent, not how this package happens to implement it.
// Milestone 4 will write a second runner that walks the same file against the
// MCP surface, which is the mechanism that stops the two surfaces from
// telling an agent different things — a divergence shows up as one runner
// passing a case the other fails, rather than as a bug report months later.
//
// Everything already covered exhaustively elsewhere — the shape of the
// indistinguishable 401, body-size limits, malformed JSON — is deliberately
// not repeated here. This suite exists to keep the acceptance criteria and
// the code honest, not to re-derive router_test.go and agents_test.go in a
// second syntax.
func TestConformance(t *testing.T) {
	suite := loadConformanceSuite(t)
	require.NotEmpty(t, suite.Cases, "the contract suite must not be empty")

	for _, c := range suite.Cases {
		t.Run(c.ID, func(t *testing.T) {
			runContractCase(t, c)
		})
	}
}

// TestConformanceKnownGaps surfaces every acceptance criterion this suite
// cannot yet exercise, as a named, visible skip rather than a silent absence.
// An acceptance criterion with no case and no entry here is not a gap that
// was considered — it is one nobody looked for.
func TestConformanceKnownGaps(t *testing.T) {
	suite := loadConformanceSuite(t)
	for _, gap := range suite.KnownGaps {
		t.Run(gap.Requirement, func(t *testing.T) {
			t.Skip(gap.Description)
		})
	}
}

func loadConformanceSuite(t *testing.T) conformanceSuite {
	t.Helper()
	// go test sets the working directory to this package's source directory
	// regardless of where `go test` was invoked from, so this relative path is
	// stable. spec/conformance is shared with the future MCP runner and cannot
	// live under this package, which rules out go:embed here.
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

// contractCase is one scenario: a sequence of operations against one fresh
// inventory, followed by assertions against the stored record. Each step is
// independently authenticated and independently checked, because an
// acceptance criterion about a response and an acceptance criterion about
// what a response causes to be stored are two different claims.
type contractCase struct {
	ID          string         `json:"id"`
	Requirement string         `json:"requirement"`
	Description string         `json:"description"`
	Steps       []contractStep `json:"steps"`
	StoreChecks []storeCheck   `json:"store_checks"`
}

type contractStep struct {
	// Op names the operation: "register", "heartbeat", or "update" — the same
	// three verbs the MCP surface will expose as tools, so a future MCP runner
	// can dispatch on this field unchanged.
	Op string `json:"op"`
	// Auth selects the credential: "enrollment_token" (register only),
	// "agent_token" (the credential saved under Agent), "bogus_token", or
	// "none".
	Auth string `json:"auth"`
	// Agent names a previously saved agent this step targets (heartbeat,
	// update) or borrows a credential from (Auth: "agent_token").
	Agent string          `json:"agent"`
	Body  json.RawMessage `json:"body"`
	// SaveAs remembers this step's resulting agent id and credential under a
	// name later steps and store checks can refer to.
	SaveAs string          `json:"save_as"`
	Expect stepExpectation `json:"expect"`
}

type stepExpectation struct {
	// Status is a symbolic outcome, not a transport-specific code: "created",
	// "ok", "unauthorized", or "bad_request". The HTTP runner maps this onto a
	// status code; an MCP runner would map it onto a tool result's success or
	// error shape instead.
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

// UnmarshalJSON distinguishes "flags_equal omitted" from "flags_equal: []":
// only the latter is a claim that the flag set is empty, and a case that
// never mentions flags_equal must not be checked against one.
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
	// Agent defaults to "self" when empty.
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

// savedAgent is what a case remembers about a registration so later steps and
// store checks can refer back to it by name.
type savedAgent struct {
	id                string
	token             string
	heartbeatInterval int
}

// runContractCase executes one case's steps against one fresh database, then
// its store checks against whatever they left behind.
func runContractCase(t *testing.T, c contractCase) {
	t.Helper()
	f := newAPIFixture(t)
	saved := map[string]savedAgent{}

	for i, step := range c.Steps {
		t.Run(fmt.Sprintf("step %d %s", i+1, step.Op), func(t *testing.T) {
			runContractStep(t, f, saved, step)
		})
	}

	for _, check := range c.StoreChecks {
		name := check.Agent
		if name == "" {
			name = "self"
		}
		agent, ok := saved[name]
		require.True(t, ok, "store check references agent %q, which no step saved", name)
		verifyStoreCheck(t, f, agent, check)
	}
}

func runContractStep(t *testing.T, f apiFixture, saved map[string]savedAgent, step contractStep) {
	t.Helper()

	token := resolveAuth(t, f, saved, step)
	wantStatus := statusFor(t, step.Expect.Status)

	switch step.Op {
	case "register":
		got := f.send(t, http.MethodPost, registerPath, token, body(string(step.Body)))
		require.Equal(t, wantStatus, got.Status, "body: %s", got.Body)
		if got.Status != http.StatusOK && got.Status != http.StatusCreated {
			return
		}
		resp := decodeJSON[registerBody](t, got.Body)

		if step.Expect.Created != nil {
			require.Equal(t, *step.Expect.Created, resp.Created)
		}
		if step.Expect.AgentTokenPresent {
			require.NotEmpty(t, resp.AgentToken)
		}
		if step.Expect.HeartbeatIntervalPositive {
			require.Positive(t, resp.HeartbeatIntervalSeconds)
		}
		if step.Expect.AgentIDMatches != "" {
			require.Equal(t, saved[step.Expect.AgentIDMatches].id, resp.AgentID,
				"expected the same agent as %q", step.Expect.AgentIDMatches)
		}
		verifyFlags(t, step.Expect, resp.Flags)

		if step.SaveAs != "" {
			saved[step.SaveAs] = savedAgent{
				id:                resp.AgentID,
				token:             resp.AgentToken,
				heartbeatInterval: resp.HeartbeatIntervalSeconds,
			}
		}

	case "heartbeat":
		agent := requireSavedAgent(t, saved, step.Agent)
		got := f.send(t, http.MethodPost, heartbeatPath(agent.id), token, nil)
		require.Equal(t, wantStatus, got.Status, "body: %s", got.Body)
		if got.Status != http.StatusOK {
			return
		}
		resp := decodeJSON[heartbeatBody](t, got.Body)

		if step.Expect.LifecycleState != "" {
			require.Equal(t, step.Expect.LifecycleState, resp.LifecycleState)
		}
		if step.Expect.ReplicaCount != nil {
			require.Equal(t, *step.Expect.ReplicaCount, resp.ReplicaCount)
		}
		if step.Expect.HeartbeatIntervalMatchesRegistration != "" {
			want := requireSavedAgent(t, saved, step.Expect.HeartbeatIntervalMatchesRegistration).heartbeatInterval
			require.Equal(t, want, resp.HeartbeatIntervalSeconds)
		}

	case "update":
		agent := requireSavedAgent(t, saved, step.Agent)
		got := f.send(t, http.MethodPatch, agentPath(agent.id), token, body(string(step.Body)))
		require.Equal(t, wantStatus, got.Status, "body: %s", got.Body)
		if got.Status != http.StatusOK {
			return
		}
		resp := decodeJSON[updateBody](t, got.Body)

		if step.Expect.Changed != nil {
			require.Equal(t, *step.Expect.Changed, resp.Changed)
		}
		verifyFlags(t, step.Expect, resp.Flags)

	default:
		t.Fatalf("unknown contract step op %q", step.Op)
	}
}

func requireSavedAgent(t *testing.T, saved map[string]savedAgent, name string) savedAgent {
	t.Helper()
	agent, ok := saved[name]
	require.True(t, ok, "step references agent %q, which no earlier step saved", name)
	return agent
}

// resolveAuth turns a step's symbolic Auth into the bearer token to send.
// "bogus_token" and "none" both exercise rejection paths, but only the first
// sends a well-formed, unrecognised credential; the tests that distinguish
// those two are router_test.go's and agents_test.go's job, not this suite's.
func resolveAuth(t *testing.T, f apiFixture, saved map[string]savedAgent, step contractStep) string {
	t.Helper()
	switch step.Auth {
	case "enrollment_token":
		return f.enrollment
	case "agent_token":
		return requireSavedAgent(t, saved, step.Agent).token
	case "bogus_token":
		return "aiinv_ac_thiscredentialdoesnotexist"
	case "none", "":
		return ""
	default:
		t.Fatalf("unknown contract step auth %q", step.Auth)
		return ""
	}
}

// statusFor maps a case's symbolic outcome onto the HTTP status this runner
// checks. A new symbol here should also grow a case in every future runner
// for the same file, which is the point of keeping the set closed and small.
func statusFor(t *testing.T, symbol string) int {
	t.Helper()
	switch symbol {
	case "created":
		return http.StatusCreated
	case "ok":
		return http.StatusOK
	case "unauthorized":
		return http.StatusUnauthorized
	case "bad_request":
		return http.StatusBadRequest
	case "payload_too_large":
		return http.StatusRequestEntityTooLarge
	default:
		t.Fatalf("unknown contract step expected status %q", symbol)
		return 0
	}
}

func verifyFlags(t *testing.T, expect stepExpectation, got []string) {
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

// conformanceAgentRow is the stored record, read directly rather than through
// the HTTP surface: a store check exists precisely to catch a case where the
// response looks right but what landed in the database does not.
type conformanceAgentRow struct {
	teamID        *uuid.UUID
	environment   string
	displayName   string
	purpose       string
	platform      string
	hosting       string
	source        string
	telemetry     string
	attributes    string
	revisionCount int
}

func fetchConformanceAgentRow(t *testing.T, f apiFixture, agentID string) conformanceAgentRow {
	t.Helper()
	id, err := uuid.Parse(agentID)
	require.NoError(t, err)

	var row conformanceAgentRow
	require.NoError(t, f.pool.QueryRow(context.Background(), `
		SELECT team_id, environment, display_name, purpose,
		       platform::text, hosting::text, source::text,
		       telemetry_links::text, attributes::text
		  FROM agents WHERE id = $1`, id,
	).Scan(&row.teamID, &row.environment, &row.displayName, &row.purpose,
		&row.platform, &row.hosting, &row.source, &row.telemetry, &row.attributes))

	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_revisions WHERE agent_id = $1`, id,
	).Scan(&row.revisionCount))

	return row
}

func verifyStoreCheck(t *testing.T, f apiFixture, agent savedAgent, check storeCheck) {
	t.Helper()
	row := fetchConformanceAgentRow(t, f, agent.id)

	if check.TeamEqualsTokenTeam {
		require.NotNil(t, row.teamID)
		require.Equal(t, f.teamID, *row.teamID, "team must come from the enrollment token, never the descriptor")
	}
	if check.EnvironmentEqualsTokenEnv {
		require.Equal(t, string(domain.EnvironmentProduction), row.environment,
			"environment must come from the enrollment token, never the descriptor")
	}
	if check.DisplayName != "" {
		require.Equal(t, check.DisplayName, row.displayName)
	}
	if check.Purpose != "" {
		require.Equal(t, check.Purpose, row.purpose)
	}
	for _, want := range check.PlatformContains {
		require.Contains(t, row.platform, want)
	}
	for _, want := range check.HostingContains {
		require.Contains(t, row.hosting, want)
	}
	for _, want := range check.SourceContains {
		require.Contains(t, row.source, want)
	}
	for _, want := range check.TelemetryContains {
		require.Contains(t, row.telemetry, want)
	}
	for _, want := range check.AttributesContains {
		require.Contains(t, row.attributes, want)
	}
	if check.RevisionCount != nil {
		require.Equal(t, *check.RevisionCount, row.revisionCount)
	}
}
