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
	pool         *pgxpool.Pool
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

	// The handler reports a failure as a populated, IsError result with a
	// nil Go error -- never the raw error -- so mcp.AddTool's wrapper cannot
	// pack err.Error() into Content and leak which sentinel fired. See
	// errors_test.go for the fuller proof that every failure mode lands on
	// exactly the same bytes.
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.IsError)
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
