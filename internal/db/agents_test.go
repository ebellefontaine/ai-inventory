package db_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
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

// selfReport is the payload an agent sends on every registration. The tests
// below re-register with a changed payload and check what survives.
func selfReport(org, team uuid.UUID, instanceKey string) db.UpsertAgentInput {
	return db.UpsertAgentInput{
		OrgID: org, TeamID: team,
		Environment: "production",
		InstanceKey: instanceKey,
		DisplayName: "invoice-triage-7f9c",
		Purpose:     "Triages AP invoices",
		Platform:    []byte(`{"framework":"langchain"}`),
		Hosting:     []byte(`{"provider":"aws"}`),
		Source:      []byte(`{"repo":"acme/invoice"}`),
		Telemetry:   []byte(`[{"kind":"dashboard"}]`),
		Attributes:  []byte(`{"tier":"internal"}`),
		Flags:       []string{"unowned"},
	}
}

// OWN-4: a human edit is not overwritten by subsequent self-reports. The guard
// must also be selective — a column the administrator never touched still
// tracks whatever the agent reports.
func TestUpsertAgentPreservesHumanEditedField(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewAgentStore(pool)
	org, team := seedOrgAndTeam(t, pool)

	in := selfReport(org, team, "k8s:default/invoice-triage")
	first, err := store.Upsert(ctx, in)
	require.NoError(t, err)
	require.True(t, first.Created)

	// An administrator gives the agent a readable name in the UI.
	corrected := "Invoice Triage (Accounts Payable)"
	humanEdit(t, pool, first.ID, agentEdit{DisplayName: &corrected})

	// The agent re-registers, still reporting its generated name, and reports
	// a changed purpose that nobody has corrected.
	in.DisplayName = "invoice-triage-a41b"
	in.Purpose = "Triages AP invoices and credit memos"
	second, err := store.Upsert(ctx, in)
	require.NoError(t, err)
	require.False(t, second.Created)
	require.Equal(t, first.ID, second.ID)

	got := readAgent(t, pool, first.ID)
	require.Equal(t, corrected, got.DisplayName,
		"a human-edited display_name must survive self-registration")
	require.Equal(t, "Triages AP invoices and credit memos", got.Purpose,
		"an unprotected column must still track the self-report")
}

// Ownership is the edit that matters most: an administrator reassigning an
// agent to the team that actually runs it must not be undone seconds later by
// the agent's own enrollment-derived team.
func TestUpsertAgentPreservesHumanEditedTeam(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewAgentStore(pool)
	org, team := seedOrgAndTeam(t, pool)
	financeTeam := seedTeam(t, pool, org, "Finance", "finance")

	in := selfReport(org, team, "k8s:default/invoice-triage")
	first, err := store.Upsert(ctx, in)
	require.NoError(t, err)
	require.True(t, first.Created)

	humanEdit(t, pool, first.ID, agentEdit{TeamID: &financeTeam})

	// The agent re-registers under its original enrollment scope.
	_, err = store.Upsert(ctx, in)
	require.NoError(t, err)

	got := readAgent(t, pool, first.ID)
	require.NotNil(t, got.TeamID)
	require.Equal(t, financeTeam, *got.TeamID,
		"a human-assigned team_id must survive self-registration")
}

// Protecting several columns at once works, and leaves the rest self-reported.
func TestUpsertAgentPreservesMultipleHumanEditedFields(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewAgentStore(pool)
	org, team := seedOrgAndTeam(t, pool)
	financeTeam := seedTeam(t, pool, org, "Finance", "finance")

	in := selfReport(org, team, "k8s:default/invoice-triage")
	first, err := store.Upsert(ctx, in)
	require.NoError(t, err)

	name := "Invoice Triage (Accounts Payable)"
	purpose := "Reviews and codes supplier invoices"
	humanEdit(t, pool, first.ID, agentEdit{
		TeamID:      &financeTeam,
		DisplayName: &name,
		Purpose:     &purpose,
	})

	in.DisplayName = "invoice-triage-a41b"
	in.Purpose = "Triages AP invoices and credit memos"
	in.Attributes = []byte(`{"tier":"critical"}`)
	_, err = store.Upsert(ctx, in)
	require.NoError(t, err)

	got := readAgent(t, pool, first.ID)
	require.Equal(t, financeTeam, *got.TeamID)
	require.Equal(t, name, got.DisplayName)
	require.Equal(t, purpose, got.Purpose)
	require.JSONEq(t, `{"tier":"critical"}`, got.Attributes,
		"columns outside human_edited_fields must still update")
	require.ElementsMatch(t,
		[]string{"team_id", "display_name", "purpose"}, got.HumanEdited)
}

// The default is an empty marker set, and with it every self-reported column
// updates exactly as it did before the guard existed.
func TestUpsertAgentWithoutHumanEditsUpdatesEveryColumn(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewAgentStore(pool)
	org, team := seedOrgAndTeam(t, pool)
	financeTeam := seedTeam(t, pool, org, "Finance", "finance")

	in := selfReport(org, team, "k8s:default/invoice-triage")
	first, err := store.Upsert(ctx, in)
	require.NoError(t, err)
	require.Empty(t, readAgent(t, pool, first.ID).HumanEdited,
		"human_edited_fields must default to empty")

	updated := db.UpsertAgentInput{
		OrgID: org, TeamID: financeTeam,
		Environment: in.Environment,
		InstanceKey: in.InstanceKey,
		DisplayName: "invoice-triage-a41b",
		Purpose:     "Triages AP invoices and credit memos",
		Platform:    []byte(`{"framework":"crewai"}`),
		Hosting:     []byte(`{"provider":"gcp"}`),
		Source:      []byte(`{"repo":"acme/invoice-v2"}`),
		Telemetry:   []byte(`[{"kind":"trace"}]`),
		Attributes:  []byte(`{"tier":"critical"}`),
		Flags:       []string{"unreviewed"},
	}
	_, err = store.Upsert(ctx, updated)
	require.NoError(t, err)

	got := readAgent(t, pool, first.ID)
	require.Equal(t, financeTeam, *got.TeamID)
	require.Equal(t, updated.DisplayName, got.DisplayName)
	require.Equal(t, updated.Purpose, got.Purpose)
	require.JSONEq(t, string(updated.Platform), got.Platform)
	require.JSONEq(t, string(updated.Hosting), got.Hosting)
	require.JSONEq(t, string(updated.Source), got.Source)
	require.JSONEq(t, string(updated.Telemetry), got.Telemetry)
	require.JSONEq(t, string(updated.Attributes), got.Attributes)
	require.Equal(t, updated.Flags, got.Flags)
}

// A first registration writes the payload verbatim; the new column neither
// blocks the insert nor arrives populated.
func TestUpsertAgentInsertIsUnaffectedByHumanEditedFields(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	store := db.NewAgentStore(pool)
	org, team := seedOrgAndTeam(t, pool)

	in := selfReport(org, team, "k8s:default/invoice-triage")
	res, err := store.Upsert(ctx, in)
	require.NoError(t, err)
	require.True(t, res.Created)

	got := readAgent(t, pool, res.ID)
	require.Empty(t, got.HumanEdited)
	require.Equal(t, team, *got.TeamID)
	require.Equal(t, in.DisplayName, got.DisplayName)
	require.Equal(t, in.Purpose, got.Purpose)
	require.JSONEq(t, string(in.Platform), got.Platform)
	require.JSONEq(t, string(in.Attributes), got.Attributes)
	require.Equal(t, in.Flags, got.Flags)
	require.Equal(t, "active", got.LifecycleState)
}
