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

// TestSearchAgentsLimitDefaultsAndClamps proves the two branches of
// SearchAgents's Limit handling that no existing test exercised: a
// zero-or-negative Limit falls back to the default rather than returning no
// rows, and a Limit above the maximum does not wrongly truncate a normal-
// sized result below what actually exists.
func TestSearchAgentsLimitDefaultsAndClamps(t *testing.T) {
	f := newQueryFixture(t)
	ctx := context.Background()

	registration := service.NewRegistrationService(f.pool, service.NewTokenService(f.pool), 60)
	_, err := registration.Register(ctx, service.RegisterInput{
		EnrollmentToken: f.enrollment,
		RawDescriptor:   []byte(`{"name":"Second worker","purpose":"Processes more jobs","instance_key":"query-fixture-2"}`),
	})
	require.NoError(t, err)

	// Zero falls back to the default (50), which is well above the two
	// agents registered for this test -- so a non-empty result proves the
	// default kicked in rather than Limit: 0 meaning "return nothing".
	zero, err := f.queries.SearchAgents(ctx, service.SearchAgentsInput{EnrollmentToken: f.enrollment, Limit: 0})
	require.NoError(t, err)
	require.NotEmpty(t, zero)

	// A negative Limit must behave the same as zero: fall back to the
	// default, not error and not return zero rows.
	negative, err := f.queries.SearchAgents(ctx, service.SearchAgentsInput{EnrollmentToken: f.enrollment, Limit: -5})
	require.NoError(t, err)
	require.Equal(t, len(zero), len(negative))

	// A Limit far above the 200 clamp must not restrict the result below
	// what actually exists: the clamp bounds an oversized request, it does
	// not truncate a normal-sized one.
	high, err := f.queries.SearchAgents(ctx, service.SearchAgentsInput{EnrollmentToken: f.enrollment, Limit: 1000})
	require.NoError(t, err)
	require.Len(t, high, 2)
}

func TestSearchAgentsRequiresAnEnrollmentTokenNotAnAgentCredential(t *testing.T) {
	f := newQueryFixture(t)

	_, err := f.queries.SearchAgents(context.Background(),
		service.SearchAgentsInput{EnrollmentToken: f.agentToken})

	require.ErrorIs(t, err, service.ErrTokenNotFound)
}
