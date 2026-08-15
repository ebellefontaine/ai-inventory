package db_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/db"
	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
)

func TestUpsertAgentIsIdempotent(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewAgentStore(pool)
	org, team := seedOrgAndTeam(t, pool)

	in := db.UpsertAgentInput{
		OrgID: org, TeamID: team,
		Environment: "production",
		InstanceKey: "lambda:arn:aws:lambda:us-east-1:123:function:invoice-triage",
		DisplayName: "Invoice triage",
		Purpose:     "Triages AP invoices",
	}

	first, err := store.Upsert(ctx, in)
	require.NoError(t, err)
	require.True(t, first.Created)

	second, err := store.Upsert(ctx, in)
	require.NoError(t, err)
	require.False(t, second.Created, "second registration must update, not insert")
	require.Equal(t, first.ID, second.ID, "same instance key must map to same agent")

	require.Equal(t, 1, countAgents(t, pool))
}

// Twenty replicas of one deployment starting simultaneously must converge to
// exactly one record, with no error surfaced to any caller.
func TestUpsertAgentConvergesUnderConcurrency(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewAgentStore(pool)
	org, team := seedOrgAndTeam(t, pool)

	in := db.UpsertAgentInput{
		OrgID: org, TeamID: team,
		Environment: "production",
		InstanceKey: "k8s:default/invoice-triage",
		DisplayName: "Invoice triage",
		Purpose:     "Triages AP invoices",
	}

	// Opening a connection costs far more than the upsert itself, so replicas
	// racing on a cold pool serialize on connection setup and never overlap.
	// Warm the pool, then release every goroutine from one barrier.
	warmPool(t, pool)

	const replicas = 20
	var ready, wg sync.WaitGroup
	ready.Add(replicas)
	start := make(chan struct{})
	errs := make([]error, replicas)
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			_, errs[i] = store.Upsert(ctx, in)
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "replica %d must not error", i)
	}
	require.Equal(t, 1, countAgents(t, pool))
}

// Same instance key in a different environment is a different agent.
func TestUpsertAgentScopesByEnvironment(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewAgentStore(pool)
	org, team := seedOrgAndTeam(t, pool)

	base := db.UpsertAgentInput{
		OrgID: org, TeamID: team,
		InstanceKey: "repo:github.com/acme/invoice#main.py",
		DisplayName: "Invoice triage",
		Purpose:     "Triages AP invoices",
	}

	prod := base
	prod.Environment = "production"
	dev := base
	dev.Environment = "development"

	a, err := store.Upsert(ctx, prod)
	require.NoError(t, err)
	b, err := store.Upsert(ctx, dev)
	require.NoError(t, err)

	require.NotEqual(t, a.ID, b.ID)
	require.Equal(t, 2, countAgents(t, pool))
}

// Re-registration revives an agent the system marked stale, but must never
// resurrect one a human deliberately retired or quarantined.
func TestUpsertAgentRevivesStaleButNotRetired(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewAgentStore(pool)
	org, team := seedOrgAndTeam(t, pool)

	base := db.UpsertAgentInput{
		OrgID: org, TeamID: team,
		Environment: "production",
		DisplayName: "Invoice triage",
		Purpose:     "Triages AP invoices",
	}

	cases := []struct {
		name        string
		instanceKey string
		start       string
		want        string
	}{
		{"stale is revived", "k8s:default/stale-agent", "stale", "active"},
		{"idle is revived", "k8s:default/idle-agent", "idle", "active"},
		{"retired stays retired", "k8s:default/retired-agent", "retired", "retired"},
		{"quarantined stays quarantined", "k8s:default/quarantined-agent", "quarantined", "quarantined"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.InstanceKey = tc.instanceKey

			first, err := store.Upsert(ctx, in)
			require.NoError(t, err)
			require.True(t, first.Created)
			require.Equal(t, "active", agentLifecycle(t, pool, first.ID))

			setAgentLifecycle(t, pool, first.ID, tc.start)

			second, err := store.Upsert(ctx, in)
			require.NoError(t, err)
			require.False(t, second.Created)
			require.Equal(t, first.ID, second.ID)
			require.Equal(t, tc.want, agentLifecycle(t, pool, first.ID),
				"re-registering a %q agent must leave it %q", tc.start, tc.want)
		})
	}
}
