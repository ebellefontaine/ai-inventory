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
