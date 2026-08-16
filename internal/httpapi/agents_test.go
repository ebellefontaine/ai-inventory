package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

// The three routes, spelled out once. A test that built these by hand could
// pass while the router served something else.
const registerPath = "/api/v1/agents:register"

func heartbeatPath(id string) string { return "/api/v1/agents/" + id + "/heartbeat" }
func agentPath(id string) string     { return "/api/v1/agents/" + id }

// testInterval is deliberately not the production default, so a handler that
// hard-codes 60 fails rather than passes by coincidence.
const testInterval = 45

// apiFixture is the whole stack: a real database, the three real services, and
// the router in front of them. Nothing here is a stub — the properties under
// test (ownership inherited from the token, one indistinguishable 401) only
// exist end to end.
type apiFixture struct {
	handler http.Handler
	pool    *pgxpool.Pool

	orgID  uuid.UUID
	teamID uuid.UUID
	// otherTeamID is a second, real team in the same org. A descriptor that
	// claims it is claiming something that exists, which is the case worth
	// refusing.
	otherTeamID uuid.UUID

	// enrollment is scoped to teamID and production.
	enrollment string
}

func newAPIFixture(t *testing.T) apiFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t)

	var orgID, teamID, otherTeamID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ('Acme', 'acme') RETURNING id`).Scan(&orgID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, 'Platform', 'platform') RETURNING id`,
		orgID).Scan(&teamID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, 'Security', 'security') RETURNING id`,
		orgID).Scan(&otherTeamID))

	tokens := service.NewTokenService(pool)
	minted, err := tokens.Mint(ctx, service.MintInput{
		OrgID:       orgID,
		TeamID:      teamID,
		Environment: domain.EnvironmentProduction,
		Name:        "platform enrollment",
		CreatedBy:   "admin@example.com",
	})
	require.NoError(t, err)

	server := NewServer(discardLogger(),
		service.NewRegistrationService(pool, tokens, testInterval),
		service.NewHeartbeatService(pool, testInterval),
		service.NewUpdateService(pool),
	)

	return apiFixture{
		handler:     server.Handler(),
		pool:        pool,
		orgID:       orgID,
		teamID:      teamID,
		otherTeamID: otherTeamID,
		enrollment:  minted.Plaintext,
	}
}

// send drives one request through the full middleware chain and captures
// everything a client could observe about the answer.
func (f apiFixture) send(t *testing.T, method, target, token string, body io.Reader) capturedResponse {
	t.Helper()

	req := httptest.NewRequest(method, target, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	return capturedResponse{Status: res.StatusCode, Header: res.Header, Body: raw}
}

func body(s string) io.Reader { return strings.NewReader(s) }

// The response shapes, declared in the test rather than imported, so that a
// rename of a field on the handler side breaks a test instead of silently
// changing the wire contract.
type registerBody struct {
	AgentID                  string   `json:"agent_id"`
	AgentToken               string   `json:"agent_token"`
	HeartbeatIntervalSeconds int      `json:"heartbeat_interval_seconds"`
	Flags                    []string `json:"flags"`
	Created                  bool     `json:"created"`
}

type heartbeatBody struct {
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	LifecycleState           string `json:"lifecycle_state"`
	ReplicaCount             int    `json:"replica_count"`
}

type updateBody struct {
	Flags   []string `json:"flags"`
	Changed bool     `json:"changed"`
}

func decodeJSON[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(raw, &v), "response body: %s", raw)
	return v
}

// register posts a descriptor with the fixture's enrollment token.
func (f apiFixture) register(t *testing.T, descriptor string) (capturedResponse, registerBody) {
	t.Helper()
	got := f.send(t, http.MethodPost, registerPath, f.enrollment, body(descriptor))
	if got.Status != http.StatusOK && got.Status != http.StatusCreated {
		return got, registerBody{}
	}
	return got, decodeJSON[registerBody](t, got.Body)
}

// fullDescriptor names every section, so the record it produces raises no flags
// at all. That is what makes the empty-flags serialisation testable.
func fullDescriptor(instanceKey string) string {
	return `{
		"name": "Invoice triage",
		"purpose": "Triages accounts payable invoices",
		"instance_key": "` + instanceKey + `",
		"platform": {"framework": "langgraph", "framework_version": "0.2.1"},
		"hosting": {"provider": "aws", "region": "us-east-1"},
		"source": {"repo_url": "https://github.com/acme/triage", "commit_sha": "abc123"}
	}`
}

type storedAgentRow struct {
	teamID      *uuid.UUID
	environment string
	displayName string
	purpose     string
	platform    string
	hosting     string
	flags       []string
}

func (f apiFixture) readAgent(t *testing.T, id string) storedAgentRow {
	t.Helper()

	agentID, err := uuid.Parse(id)
	require.NoError(t, err)

	var row storedAgentRow
	require.NoError(t, f.pool.QueryRow(context.Background(), `
		SELECT team_id, environment, display_name, purpose, platform::text, hosting::text, flags
		  FROM agents WHERE id = $1`, agentID,
	).Scan(&row.teamID, &row.environment, &row.displayName, &row.purpose,
		&row.platform, &row.hosting, &row.flags))

	return row
}

// TestRegisterInheritsOwnershipFromTheTokenNotThePayload is the product's
// central guarantee, asserted over HTTP rather than at the service boundary.
// The descriptor claims a different team — by the id of a team that really
// exists — and a different environment, and the stored record must show
// neither.
func TestRegisterInheritsOwnershipFromTheTokenNotThePayload(t *testing.T) {
	f := newAPIFixture(t)

	descriptor := `{
		"name": "Invoice triage",
		"purpose": "Triages accounts payable invoices",
		"instance_key": "billing-1",
		"team": "security",
		"team_id": "` + f.otherTeamID.String() + `",
		"environment": "development",
		"env": "development"
	}`

	got, decoded := f.register(t, descriptor)

	require.Equal(t, http.StatusCreated, got.Status)
	require.Contains(t, decoded.Flags, domain.FlagIgnoredScopeFields)

	stored := f.readAgent(t, decoded.AgentID)
	require.NotNil(t, stored.teamID)
	require.Equal(t, f.teamID, *stored.teamID, "team comes from the token, never the payload")
	require.NotEqual(t, f.otherTeamID, *stored.teamID)
	require.Equal(t, string(domain.EnvironmentProduction), stored.environment,
		"environment comes from the token, never the payload")
}

// TestRegisterCreatesThenUpdates: re-registration is an update, and a client
// has to be able to tell the two apart.
func TestRegisterCreatesThenUpdates(t *testing.T) {
	f := newAPIFixture(t)

	first, created := f.register(t, fullDescriptor("billing-1"))
	require.Equal(t, http.StatusCreated, first.Status)
	require.True(t, created.Created)
	require.Equal(t, testInterval, created.HeartbeatIntervalSeconds)
	require.NotEmpty(t, created.AgentToken)

	second, updated := f.register(t, fullDescriptor("billing-1"))
	require.Equal(t, http.StatusOK, second.Status)
	require.False(t, updated.Created)
	require.Equal(t, created.AgentID, updated.AgentID, "the same agent, not a second record")
	require.NotEqual(t, created.AgentToken, updated.AgentToken, "each registration is a replica")
}

// TestReturnedAgentTokenAuthenticatesAHeartbeat: the credential handed back at
// registration is the one thing the agent needs next, so it has to work.
func TestReturnedAgentTokenAuthenticatesAHeartbeat(t *testing.T) {
	f := newAPIFixture(t)

	_, agent := f.register(t, fullDescriptor("billing-1"))

	got := f.send(t, http.MethodPost, heartbeatPath(agent.AgentID), agent.AgentToken, nil)

	require.Equal(t, http.StatusOK, got.Status)
}

// TestHeartbeatReportsServerPolicyAndObservedState: the interval is the
// server's, the lifecycle state is the server's inference, and the replica
// count is derived from credentials rather than claimed by the agent.
func TestHeartbeatReportsServerPolicyAndObservedState(t *testing.T) {
	f := newAPIFixture(t)

	_, first := f.register(t, fullDescriptor("billing-1"))
	_, second := f.register(t, fullDescriptor("billing-1"))
	require.Equal(t, first.AgentID, second.AgentID)

	one := decodeJSON[heartbeatBody](t,
		f.send(t, http.MethodPost, heartbeatPath(first.AgentID), first.AgentToken, nil).Body)

	require.Equal(t, testInterval, one.HeartbeatIntervalSeconds)
	require.Equal(t, string(domain.LifecycleActive), one.LifecycleState)
	require.Equal(t, 1, one.ReplicaCount, "only one replica has beaten so far")

	two := decodeJSON[heartbeatBody](t,
		f.send(t, http.MethodPost, heartbeatPath(second.AgentID), second.AgentToken, nil).Body)

	require.Equal(t, 2, two.ReplicaCount, "the second replica proved itself alive")
}

// TestHeartbeatAcceptsAnEmptyBody: a heartbeat carries no payload, and an agent
// that sends nothing — or an empty document — must not be refused for it.
func TestHeartbeatAcceptsAnEmptyBody(t *testing.T) {
	f := newAPIFixture(t)
	_, agent := f.register(t, fullDescriptor("billing-1"))

	for name, payload := range map[string]io.Reader{
		"no body at all": nil,
		"empty body":     body(""),
		"empty object":   body(`{}`),
	} {
		t.Run(name, func(t *testing.T) {
			got := f.send(t, http.MethodPost, heartbeatPath(agent.AgentID), agent.AgentToken, payload)

			require.Equal(t, http.StatusOK, got.Status, "body: %s", got.Body)
			require.Equal(t, testInterval,
				decodeJSON[heartbeatBody](t, got.Body).HeartbeatIntervalSeconds)
		})
	}
}

// TestMalformedAgentIDIsTheSame401AsABadCredential is the enumeration oracle by
// another route. A 400 for an unparseable id would confirm that a well-formed
// uuid names something, so the two answers are compared byte for byte rather
// than against a literal.
func TestMalformedAgentIDIsTheSame401AsABadCredential(t *testing.T) {
	f := newAPIFixture(t)
	_, agent := f.register(t, fullDescriptor("billing-1"))

	canonicalHeartbeat := f.send(t, http.MethodPost,
		heartbeatPath(agent.AgentID), "aiinv_ac_thiscredentialdoesnotexist", nil)
	require.Equal(t, http.StatusUnauthorized, canonicalHeartbeat.Status)

	canonicalUpdate := f.send(t, http.MethodPatch,
		agentPath(agent.AgentID), "aiinv_ac_thiscredentialdoesnotexist", body(`{"purpose":"x"}`))
	require.Equal(t, http.StatusUnauthorized, canonicalUpdate.Status)

	// An empty segment is deliberately not in this list: "/api/v1/agents//heartbeat"
	// is a different URL, which ServeMux redirects on its path alone, before any
	// handler and without consulting a credential. It leaks nothing because the
	// answer does not depend on one.
	for _, id := range []string{"not-a-uuid", "7f3a", "%20", "0000", "00000000-0000-0000-0000-00000000000"} {
		t.Run("heartbeat "+id, func(t *testing.T) {
			got := f.send(t, http.MethodPost, heartbeatPath(id), agent.AgentToken, nil)

			require.Equal(t, canonicalHeartbeat.Status, got.Status)
			require.Equal(t, canonicalHeartbeat.Header, got.Header)
			require.Equal(t, canonicalHeartbeat.Body, got.Body)
		})

		t.Run("update "+id, func(t *testing.T) {
			got := f.send(t, http.MethodPatch, agentPath(id), agent.AgentToken, body(`{"purpose":"x"}`))

			require.Equal(t, canonicalUpdate.Status, got.Status)
			require.Equal(t, canonicalUpdate.Header, got.Header)
			require.Equal(t, canonicalUpdate.Body, got.Body)
		})
	}
}

// TestAnotherAgentsCredentialIsTheSame401: a valid credential presented for a
// record it does not own must not be distinguishable from one that never
// existed — otherwise it confirms the credential is real.
func TestAnotherAgentsCredentialIsTheSame401(t *testing.T) {
	f := newAPIFixture(t)

	_, mine := f.register(t, fullDescriptor("billing-1"))
	_, theirs := f.register(t, fullDescriptor("billing-2"))
	require.NotEqual(t, mine.AgentID, theirs.AgentID)

	canonical := f.send(t, http.MethodPost,
		heartbeatPath(mine.AgentID), "aiinv_ac_thiscredentialdoesnotexist", nil)

	got := f.send(t, http.MethodPost, heartbeatPath(mine.AgentID), theirs.AgentToken, nil)

	require.Equal(t, http.StatusUnauthorized, got.Status)
	require.Equal(t, canonical.Header, got.Header)
	require.Equal(t, canonical.Body, got.Body)

	patched := f.send(t, http.MethodPatch, agentPath(mine.AgentID), theirs.AgentToken, body(`{"purpose":"x"}`))
	require.Equal(t, http.StatusUnauthorized, patched.Status)
	require.Equal(t, canonical.Body, patched.Body)
}

// TestPatchAppliesOnlyWhatThePayloadCarries: absent is not empty, or a redeploy
// reporting a new region would wipe everything it did not mention.
func TestPatchAppliesOnlyWhatThePayloadCarries(t *testing.T) {
	f := newAPIFixture(t)
	_, agent := f.register(t, fullDescriptor("billing-1"))

	got := f.send(t, http.MethodPatch, agentPath(agent.AgentID), agent.AgentToken,
		body(`{"hosting":{"provider":"gcp","region":"europe-west1"}}`))

	require.Equal(t, http.StatusOK, got.Status)
	require.True(t, decodeJSON[updateBody](t, got.Body).Changed)

	stored := f.readAgent(t, agent.AgentID)
	require.Contains(t, stored.hosting, "gcp")
	require.Contains(t, stored.hosting, "europe-west1")
	require.Equal(t, "Invoice triage", stored.displayName, "an absent key is not an empty value")
	require.Equal(t, "Triages accounts payable invoices", stored.purpose)
	require.Contains(t, stored.platform, "langgraph")
}

// TestPatchReportsUnchangedForANoOp: an agent re-sending what the record
// already holds must not look like a change.
func TestPatchReportsUnchangedForANoOp(t *testing.T) {
	f := newAPIFixture(t)
	_, agent := f.register(t, fullDescriptor("billing-1"))

	got := f.send(t, http.MethodPatch, agentPath(agent.AgentID), agent.AgentToken,
		body(`{"purpose":"Triages accounts payable invoices"}`))

	require.Equal(t, http.StatusOK, got.Status)
	require.False(t, decodeJSON[updateBody](t, got.Body).Changed)
}

// TestMissingOrMalformedAuthorizationIsTheShared401 covers all three endpoints:
// no header, the wrong scheme, and an empty token must all answer as one.
func TestMissingOrMalformedAuthorizationIsTheShared401(t *testing.T) {
	f := newAPIFixture(t)
	_, agent := f.register(t, fullDescriptor("billing-1"))

	canonical := f.send(t, http.MethodPost,
		heartbeatPath(agent.AgentID), "aiinv_ac_thiscredentialdoesnotexist", nil)

	requests := []struct {
		name           string
		method, target string
		payload        string
	}{
		{"register", http.MethodPost, registerPath, fullDescriptor("billing-2")},
		{"heartbeat", http.MethodPost, heartbeatPath(agent.AgentID), ""},
		{"update", http.MethodPatch, agentPath(agent.AgentID), `{"purpose":"x"}`},
	}

	headers := map[string]string{
		"absent":       "",
		"scheme only":  "Bearer",
		"empty token":  "Bearer ",
		"wrong scheme": "Basic aGk6dGhlcmU=",
		"bare token":   "aiinv_ac_thiscredentialdoesnotexist",
	}

	for _, req := range requests {
		for name, header := range headers {
			t.Run(req.name+" "+name, func(t *testing.T) {
				httpReq := httptest.NewRequest(req.method, req.target, body(req.payload))
				if header != "" {
					httpReq.Header.Set("Authorization", header)
				}
				rec := httptest.NewRecorder()
				f.handler.ServeHTTP(rec, httpReq)

				res := rec.Result()
				defer func() { _ = res.Body.Close() }()
				raw, err := io.ReadAll(res.Body)
				require.NoError(t, err)

				require.Equal(t, canonical.Status, res.StatusCode)
				require.Equal(t, canonical.Header, res.Header)
				require.Equal(t, canonical.Body, raw)
			})
		}
	}
}

// TestOversizedBodyIsRefused: the cap is enforced on the two endpoints that
// read a payload.
func TestOversizedBodyIsRefused(t *testing.T) {
	f := newAPIFixture(t)
	_, agent := f.register(t, fullDescriptor("billing-1"))

	oversized := `{"name":"x","purpose":"` + strings.Repeat("a", MaxBodyBytes) + `"}`

	t.Run("register", func(t *testing.T) {
		got := f.send(t, http.MethodPost, registerPath, f.enrollment, body(oversized))

		require.Equal(t, http.StatusRequestEntityTooLarge, got.Status)
		require.Equal(t, codePayloadTooLarge, decodeEnvelope(t, got.Body).Error.Code)
	})

	t.Run("update", func(t *testing.T) {
		got := f.send(t, http.MethodPatch, agentPath(agent.AgentID), agent.AgentToken, body(oversized))

		require.Equal(t, http.StatusRequestEntityTooLarge, got.Status)
		require.Equal(t, codePayloadTooLarge, decodeEnvelope(t, got.Body).Error.Code)
	})
}

// TestUnparseableJSONIsABadRequest: a payload that is not a JSON object has
// nothing to record, and the client is the one who can fix it.
func TestUnparseableJSONIsABadRequest(t *testing.T) {
	f := newAPIFixture(t)
	_, agent := f.register(t, fullDescriptor("billing-1"))

	for name, payload := range map[string]string{
		"truncated": `{"name":`,
		"not JSON":  `this is not json`,
		"an array":  `["name"]`,
	} {
		t.Run("register "+name, func(t *testing.T) {
			got := f.send(t, http.MethodPost, registerPath, f.enrollment, body(payload))

			require.Equal(t, http.StatusBadRequest, got.Status, "body: %s", got.Body)
			require.Equal(t, codeBadRequest, decodeEnvelope(t, got.Body).Error.Code)
		})

		t.Run("update "+name, func(t *testing.T) {
			got := f.send(t, http.MethodPatch, agentPath(agent.AgentID), agent.AgentToken, body(payload))

			require.Equal(t, http.StatusBadRequest, got.Status, "body: %s", got.Body)
			require.Equal(t, codeBadRequest, decodeEnvelope(t, got.Body).Error.Code)
		})
	}
}

// TestEmptyFlagsSerialiseAsAnArray asserts on the raw bytes on purpose: a
// decoded []string cannot tell [] from null, and a client that has to handle
// both is a client that will get it wrong.
func TestEmptyFlagsSerialiseAsAnArray(t *testing.T) {
	f := newAPIFixture(t)

	registered, agent := f.register(t, fullDescriptor("billing-1"))
	require.Empty(t, agent.Flags, "the full descriptor raises no flags")
	require.Contains(t, string(registered.Body), `"flags":[]`)
	require.NotContains(t, string(registered.Body), `"flags":null`)

	patched := f.send(t, http.MethodPatch, agentPath(agent.AgentID), agent.AgentToken,
		body(`{"purpose":"Triages accounts payable invoices"}`))
	require.Contains(t, string(patched.Body), `"flags":[]`)
	require.NotContains(t, string(patched.Body), `"flags":null`)
}
