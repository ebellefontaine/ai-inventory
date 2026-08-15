package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/db"
)

// seedOrgAndTeam inserts one organization and one team and returns their ids.
func seedOrgAndTeam(t *testing.T, pool *pgxpool.Pool) (orgID, teamID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ('Acme', 'acme') RETURNING id`,
	).Scan(&orgID))

	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, 'Platform', 'platform') RETURNING id`,
		orgID,
	).Scan(&teamID))

	return orgID, teamID
}

// seedTeam inserts one extra team into an existing organization.
func seedTeam(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, name, slug string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, $2, $3) RETURNING id`,
		orgID, name, slug,
	).Scan(&id))
	return id
}

// seedAgent registers one agent through the store and returns its id.
func seedAgent(t *testing.T, pool *pgxpool.Pool, orgID, teamID uuid.UUID, instanceKey string) uuid.UUID {
	t.Helper()

	res, err := db.NewAgentStore(pool).Upsert(context.Background(), db.UpsertAgentInput{
		OrgID:       orgID,
		TeamID:      teamID,
		Environment: "production",
		InstanceKey: instanceKey,
		DisplayName: "Invoice triage",
		Purpose:     "Triages AP invoices",
	})
	require.NoError(t, err)
	return res.ID
}

// insertCredential issues one credential for an agent.
func insertCredential(t *testing.T, pool *pgxpool.Pool, agentID uuid.UUID, tokenHash string) {
	t.Helper()

	_, err := pool.Exec(context.Background(),
		`INSERT INTO agent_credentials (agent_id, token_hash) VALUES ($1, $2)`,
		agentID, tokenHash)
	require.NoError(t, err)
}

// countCredentials returns the number of credentials held by an agent.
func countCredentials(t *testing.T, pool *pgxpool.Pool, agentID uuid.UUID) int {
	t.Helper()

	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_credentials WHERE agent_id = $1`, agentID).Scan(&n))
	return n
}

// insertAuditEntry writes one audit_log row and returns its id. There is no
// audit store yet; the services that write audit entries arrive later.
func insertAuditEntry(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, actor, action string) int64 {
	t.Helper()

	var id int64
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO audit_log (org_id, actor, action, target_type, target_id, after)
		 VALUES ($1, $2, $3, 'agent', $4, '{"lifecycle_state":"retired"}'::jsonb)
		 RETURNING id`,
		orgID, actor, action, uuid.NewString(),
	).Scan(&id))
	return id
}

// countAgents returns the number of rows in the agents table.
func countAgents(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()

	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agents`).Scan(&n))
	return n
}

// warmPool opens every connection the pool allows and returns them, so a
// concurrency test measures contention on the database rather than the cost
// of establishing connections one at a time.
func warmPool(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	conns := make([]*pgxpool.Conn, 0, pool.Config().MaxConns)
	for i := int32(0); i < pool.Config().MaxConns; i++ {
		c, err := pool.Acquire(ctx)
		require.NoError(t, err)
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
}

// agentLifecycle returns the lifecycle_state of one agent.
func agentLifecycle(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()

	var state string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT lifecycle_state FROM agents WHERE id = $1`, id).Scan(&state))
	return state
}

// setAgentLifecycle forces an agent into a lifecycle state, standing in for
// the sweeper (stale) or a human operator (retired).
func setAgentLifecycle(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, state string) {
	t.Helper()

	tag, err := pool.Exec(context.Background(),
		`UPDATE agents SET lifecycle_state = $2 WHERE id = $1`, id, state)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
}

// agentEdit is an administrator's correction to one agent. Every non-nil field
// is written and its column name recorded in human_edited_fields.
type agentEdit struct {
	TeamID      *uuid.UUID
	DisplayName *string
	Purpose     *string
}

// humanEdit stands in for the administrator UI, exactly as setAgentLifecycle
// stands in for the sweeper. The service-layer API for marking fields
// human-edited arrives with the admin API in Milestone 5; until then the tests
// write the correction and the human_edited_fields marker directly, because the
// guarantee under test lives in the storage layer regardless of who sets it.
func humanEdit(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, edit agentEdit) {
	t.Helper()

	fields := []string{}
	if edit.TeamID != nil {
		fields = append(fields, "team_id")
	}
	if edit.DisplayName != nil {
		fields = append(fields, "display_name")
	}
	if edit.Purpose != nil {
		fields = append(fields, "purpose")
	}

	// COALESCE leaves a column alone when the edit does not mention it, which
	// keeps this one statement usable for any subset of fields.
	tag, err := pool.Exec(context.Background(), `
		UPDATE agents SET
		    team_id             = COALESCE($2, team_id),
		    display_name        = COALESCE($3, display_name),
		    purpose             = COALESCE($4, purpose),
		    human_edited_fields = $5
		WHERE id = $1`,
		id, edit.TeamID, edit.DisplayName, edit.Purpose, fields)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
}

// agentSnapshot is one agent's stored state: the eight self-reported columns
// the human-edit guard covers, plus the server-managed columns it must not.
type agentSnapshot struct {
	TeamID         *uuid.UUID
	DisplayName    string
	Purpose        string
	Platform       string
	Hosting        string
	Source         string
	Telemetry      string
	Attributes     string
	Flags          []string
	LifecycleState string
	HumanEdited    []string
}

// readAgent loads one agent's stored state.
func readAgent(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) agentSnapshot {
	t.Helper()

	var s agentSnapshot
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT team_id, display_name, purpose,
		       platform::text, hosting::text, source::text,
		       telemetry_links::text, attributes::text,
		       flags, lifecycle_state, human_edited_fields
		  FROM agents WHERE id = $1`, id,
	).Scan(
		&s.TeamID, &s.DisplayName, &s.Purpose,
		&s.Platform, &s.Hosting, &s.Source,
		&s.Telemetry, &s.Attributes,
		&s.Flags, &s.LifecycleState, &s.HumanEdited,
	))
	return s
}
