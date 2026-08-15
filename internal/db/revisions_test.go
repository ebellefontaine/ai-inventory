package db_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/db"
	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
)

// Writing a revision must never disturb the ones already recorded. This is the
// whole value of the table: "what did this agent look like in March".
func TestRevisionAppendPreservesHistory(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewRevisionStore(pool)
	org, team := seedOrgAndTeam(t, pool)
	agent := seedAgent(t, pool, org, team, "k8s:default/invoice-triage")

	require.NoError(t, store.Append(ctx, agent, []byte(`{"display_name":"v1"}`), "svc:registrar"))
	require.NoError(t, store.Append(ctx, agent, []byte(`{"display_name":"v2"}`), "svc:registrar"))
	require.NoError(t, store.Append(ctx, agent, []byte(`{"display_name":"v3"}`), "user:ada@acme.test"))

	got, err := store.List(ctx, agent, 10)
	require.NoError(t, err)
	require.Len(t, got, 3, "every revision must survive the ones written after it")
}

// Callers want the current shape first, then backwards through time.
func TestRevisionListNewestFirst(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewRevisionStore(pool)
	org, team := seedOrgAndTeam(t, pool)
	agent := seedAgent(t, pool, org, team, "k8s:default/invoice-triage")

	for _, name := range []string{"v1", "v2", "v3"} {
		require.NoError(t, store.Append(ctx, agent,
			[]byte(`{"display_name":"`+name+`"}`), "svc:registrar"))
	}

	got, err := store.List(ctx, agent, 10)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.JSONEq(t, `{"display_name":"v3"}`, string(got[0].Snapshot))
	require.JSONEq(t, `{"display_name":"v2"}`, string(got[1].Snapshot))
	require.JSONEq(t, `{"display_name":"v1"}`, string(got[2].Snapshot))

	require.Equal(t, agent, got[0].AgentID)
	require.Equal(t, "svc:registrar", got[0].Actor)
	require.False(t, got[0].ChangedAt.IsZero())

	limited, err := store.List(ctx, agent, 2)
	require.NoError(t, err)
	require.Len(t, limited, 2)
	require.JSONEq(t, `{"display_name":"v3"}`, string(limited[0].Snapshot))
}

// The append-only guarantee has to live in the database. If it lived in Go it
// would be one careless UPDATE away from worthless.
func TestRevisionUpdateRejectedByDatabase(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewRevisionStore(pool)
	org, team := seedOrgAndTeam(t, pool)
	agent := seedAgent(t, pool, org, team, "k8s:default/invoice-triage")

	require.NoError(t, store.Append(ctx, agent, []byte(`{"display_name":"v1"}`), "svc:registrar"))
	before, err := store.List(ctx, agent, 1)
	require.NoError(t, err)
	require.Len(t, before, 1)

	_, err = pool.Exec(ctx,
		`UPDATE agent_revisions SET actor = 'tamper', snapshot = '{"display_name":"forged"}'::jsonb
		 WHERE id = $1`, before[0].ID)
	require.Error(t, err, "the database must refuse to rewrite a revision")
	require.ErrorContains(t, err, "agent_revisions is append-only")

	after, err := store.List(ctx, agent, 1)
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.Equal(t, "svc:registrar", after[0].Actor)
	require.JSONEq(t, `{"display_name":"v1"}`, string(after[0].Snapshot))
}

func TestRevisionDeleteRejectedByDatabase(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewRevisionStore(pool)
	org, team := seedOrgAndTeam(t, pool)
	agent := seedAgent(t, pool, org, team, "k8s:default/invoice-triage")

	require.NoError(t, store.Append(ctx, agent, []byte(`{"display_name":"v1"}`), "svc:registrar"))
	before, err := store.List(ctx, agent, 1)
	require.NoError(t, err)
	require.Len(t, before, 1)

	_, err = pool.Exec(ctx, `DELETE FROM agent_revisions WHERE id = $1`, before[0].ID)
	require.Error(t, err, "the database must refuse to erase a revision of a live agent")
	require.ErrorContains(t, err, "agent_revisions is append-only")

	after, err := store.List(ctx, agent, 1)
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.Equal(t, before[0].ID, after[0].ID)
}

func TestAuditLogUpdateRejectedByDatabase(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	org, _ := seedOrgAndTeam(t, pool)

	id := insertAuditEntry(t, pool, org, "user:ada@acme.test", "agent.retire")

	_, err := pool.Exec(ctx,
		`UPDATE audit_log SET actor = 'user:mallory@acme.test' WHERE id = $1`, id)
	require.Error(t, err)
	require.ErrorContains(t, err, "audit_log is append-only")

	var actor string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT actor FROM audit_log WHERE id = $1`, id).Scan(&actor))
	require.Equal(t, "user:ada@acme.test", actor)
}

func TestAuditLogDeleteRejectedByDatabase(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	org, _ := seedOrgAndTeam(t, pool)

	id := insertAuditEntry(t, pool, org, "user:ada@acme.test", "agent.retire")

	_, err := pool.Exec(ctx, `DELETE FROM audit_log WHERE id = $1`, id)
	require.Error(t, err)
	require.ErrorContains(t, err, "audit_log is append-only")

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE id = $1`, id).Scan(&n))
	require.Equal(t, 1, n)
}

// The deliberate exception. Nothing in this product deletes history except a
// hard delete of the agent itself, which is an audited administrator action.
// The append-only triggers must not turn that into an unremovable row.
func TestDeletingAgentCascadesHistory(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewRevisionStore(pool)
	org, team := seedOrgAndTeam(t, pool)

	doomed := seedAgent(t, pool, org, team, "k8s:default/doomed")
	survivor := seedAgent(t, pool, org, team, "k8s:default/survivor")

	require.NoError(t, store.Append(ctx, doomed, []byte(`{"display_name":"v1"}`), "svc:registrar"))
	require.NoError(t, store.Append(ctx, doomed, []byte(`{"display_name":"v2"}`), "svc:registrar"))
	require.NoError(t, store.Append(ctx, survivor, []byte(`{"display_name":"v1"}`), "svc:registrar"))
	insertCredential(t, pool, doomed, "hash-doomed")
	insertCredential(t, pool, survivor, "hash-survivor")

	tag, err := pool.Exec(ctx, `DELETE FROM agents WHERE id = $1`, doomed)
	require.NoError(t, err, "a deliberate hard delete of an agent must succeed")
	require.EqualValues(t, 1, tag.RowsAffected())

	gone, err := store.List(ctx, doomed, 10)
	require.NoError(t, err)
	require.Empty(t, gone, "revisions must die with the agent")
	require.Equal(t, 0, countCredentials(t, pool, doomed))

	kept, err := store.List(ctx, survivor, 10)
	require.NoError(t, err)
	require.Len(t, kept, 1, "another agent's history must be untouched")
	require.Equal(t, 1, countCredentials(t, pool, survivor))

	// The audit trail of the deletion outlives the agent it describes:
	// audit_log references the organization, not the agent.
	auditID := insertAuditEntry(t, pool, org, "user:ada@acme.test", "agent.delete")
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE id = $1`, auditID).Scan(&n))
	require.Equal(t, 1, n)
}

// The same exception, one level up: audit entries belong to an organization,
// so deleting the organization takes them with it — and takes the agents, their
// revisions and their credentials in the same cascade.
func TestDeletingOrganizationCascadesHistory(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewRevisionStore(pool)
	org, team := seedOrgAndTeam(t, pool)
	agent := seedAgent(t, pool, org, team, "k8s:default/invoice-triage")

	require.NoError(t, store.Append(ctx, agent, []byte(`{"display_name":"v1"}`), "svc:registrar"))
	insertCredential(t, pool, agent, "hash-a")
	insertAuditEntry(t, pool, org, "user:ada@acme.test", "agent.retire")

	tag, err := pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, org)
	require.NoError(t, err, "a deliberate hard delete of an organization must succeed")
	require.EqualValues(t, 1, tag.RowsAffected())

	for _, table := range []string{"agent_revisions", "agent_credentials", "audit_log", "agents"} {
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM `+table).Scan(&n))
		require.Equal(t, 0, n, "%s must be emptied by the cascade", table)
	}
}

// Two agents cannot be handed the same credential, and a replayed issuance
// cannot silently create a second row for one token.
func TestCredentialTokenHashIsUnique(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	org, team := seedOrgAndTeam(t, pool)

	a := seedAgent(t, pool, org, team, "k8s:default/agent-a")
	b := seedAgent(t, pool, org, team, "k8s:default/agent-b")

	insertCredential(t, pool, a, "shared-hash")

	_, err := pool.Exec(ctx,
		`INSERT INTO agent_credentials (agent_id, token_hash) VALUES ($1, $2)`,
		b, "shared-hash")
	require.Error(t, err, "one token hash must never map to two agents")
	require.ErrorContains(t, err, "agent_credentials_token_hash_key")

	_, err = pool.Exec(ctx,
		`INSERT INTO agent_credentials (agent_id, token_hash) VALUES ($1, $2)`,
		a, "shared-hash")
	require.Error(t, err, "re-issuing the same hash to the same agent must fail too")
}
