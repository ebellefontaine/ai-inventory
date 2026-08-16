package service_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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
	pool       *pgxpool.Pool
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
		pool:       pool,
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

	var otherOrg, otherTeam uuid.UUID
	require.NoError(t, f.pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ('Other', 'other') RETURNING id`).Scan(&otherOrg))
	require.NoError(t, f.pool.QueryRow(ctx,
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, 'Other team', 'other-team') RETURNING id`,
		otherOrg).Scan(&otherTeam))
	otherTokens := service.NewTokenService(f.pool)
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
