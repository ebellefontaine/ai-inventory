package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/auth"
	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/service"
	"github.com/ebellefontaine/ai-inventory/internal/worker"
)

// The thresholds under test are deliberately not the production defaults, so a
// worker that hard-codes 60/3/7 fails here.
const (
	testHeartbeatInterval    = 45
	testIdleAfterMissedBeats = 4
	testStaleAfterDays       = 9
)

const (
	// idleAfter is how long an agent may be silent before the server infers
	// idle, and staleAfter how long before it infers stale.
	idleAfter  = testHeartbeatInterval * testIdleAfterMissedBeats * time.Second
	staleAfter = testStaleAfterDays * 24 * time.Hour
	// livenessWindow mirrors the window heartbeat.go derives: two intervals.
	livenessWindow = 2 * testHeartbeatInterval * time.Second
)

// fixedNow is the clock every test injects, and it is years away from the wall
// clock on purpose. A worker that reaches for now() in SQL instead of the
// injected clock does not merely drift by a second here: every timestamp these
// tests write lands in its future, nothing at all transitions, and the failure
// is loud rather than intermittent.
var fixedNow = time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)

// lifecycleFixture is one org, one enrollment token, a real registration
// service, and the worker under test. Agents are registered through
// RegistrationService rather than inserted by hand, so the rows and credentials
// the sweep reads are the ones production would give it.
type lifecycleFixture struct {
	pool       *pgxpool.Pool
	reg        *service.RegistrationService
	worker     *worker.LifecycleWorker
	orgID      uuid.UUID
	enrollment string
}

func newLifecycleFixture(t *testing.T, slug string) lifecycleFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t)

	var orgID, teamID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ('Acme', $1) RETURNING id`, slug,
	).Scan(&orgID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, 'Platform', $2) RETURNING id`, orgID, slug,
	).Scan(&teamID))

	tokens := service.NewTokenService(pool)
	minted, err := tokens.Mint(ctx, service.MintInput{
		OrgID:       orgID,
		TeamID:      teamID,
		Environment: domain.EnvironmentProduction,
		Name:        slug + " enrollment",
		CreatedBy:   "admin@example.com",
	})
	require.NoError(t, err)

	return lifecycleFixture{
		pool:       pool,
		reg:        service.NewRegistrationService(pool, tokens, testHeartbeatInterval),
		orgID:      orgID,
		enrollment: minted.Plaintext,
		worker: worker.New(pool, worker.Config{
			HeartbeatIntervalSeconds: testHeartbeatInterval,
			IdleAfterMissedBeats:     testIdleAfterMissedBeats,
			StaleAfterDays:           testStaleAfterDays,
			Now:                      func() time.Time { return fixedNow },
		}),
	}
}

// register runs one real registration. Calling it twice with the same instance
// key models two replicas of one deployment: one agent record, two credentials.
func (fx lifecycleFixture) register(t *testing.T, instanceKey string) service.RegisterResult {
	t.Helper()

	raw, err := json.Marshal(map[string]any{
		"name":         "Invoice triage",
		"purpose":      "Triages accounts payable invoices",
		"instance_key": instanceKey,
	})
	require.NoError(t, err)

	res, err := fx.reg.Register(context.Background(), service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   raw,
	})
	require.NoError(t, err)
	return res
}

func (fx lifecycleFixture) sweep(t *testing.T) worker.SweepResult {
	t.Helper()

	res, err := fx.worker.Sweep(context.Background())
	require.NoError(t, err)
	return res
}

// silentFor backdates an agent's last heartbeat relative to the injected clock,
// which is how these tests age an agent by nine days without sleeping.
func (fx lifecycleFixture) silentFor(t *testing.T, id uuid.UUID, d time.Duration) {
	t.Helper()
	fx.exec(t, `UPDATE agents SET last_heartbeat_at = $2 WHERE id = $1`, id, fixedNow.Add(-d))
}

func (fx lifecycleFixture) setLifecycle(t *testing.T, id uuid.UUID, state domain.LifecycleState) {
	t.Helper()
	fx.exec(t, `UPDATE agents SET lifecycle_state = $2 WHERE id = $1`, id, string(state))
}

// setReplicaCount plants the frozen count this worker exists to correct: a
// number last written by a heartbeat that will never arrive again.
func (fx lifecycleFixture) setReplicaCount(t *testing.T, id uuid.UUID, n int) {
	t.Helper()
	fx.exec(t, `UPDATE agents SET replica_count = $2 WHERE id = $1`, id, n)
}

// credentialSeen stamps a credential's last_seen_at relative to the injected
// clock, standing in for the heartbeat that would have written it.
func (fx lifecycleFixture) credentialSeen(t *testing.T, token string, ago time.Duration) {
	t.Helper()

	tag, err := fx.pool.Exec(context.Background(),
		`UPDATE agent_credentials SET last_seen_at = $2 WHERE token_hash = $1`,
		auth.HashToken(token), fixedNow.Add(-ago))
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "the fixture must have changed exactly one credential")
}

func (fx lifecycleFixture) exec(t *testing.T, sql string, id uuid.UUID, arg any) {
	t.Helper()

	tag, err := fx.pool.Exec(context.Background(), sql, id, arg)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "the fixture must have changed exactly one agent")
}

// agentState is what the sweep is allowed to move.
type agentState struct {
	lifecycleState string
	replicaCount   int
}

func (fx lifecycleFixture) read(t *testing.T, id uuid.UUID) agentState {
	t.Helper()

	var s agentState
	require.NoError(t, fx.pool.QueryRow(context.Background(),
		`SELECT lifecycle_state, replica_count FROM agents WHERE id = $1`, id,
	).Scan(&s.lifecycleState, &s.replicaCount))
	return s
}

// countRows counts a whole table. The table name is a constant supplied by the
// test, never by input, so interpolating it is safe here.
func (fx lifecycleFixture) countRows(t *testing.T, table string) int {
	t.Helper()

	var n int
	require.NoError(t, fx.pool.QueryRow(context.Background(),
		fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n))
	return n
}

// auditEntry is one audit_log row, with the two lifecycle states pulled out of
// the before/after snapshots.
type auditEntry struct {
	orgID       uuid.UUID
	actor       string
	action      string
	targetType  string
	targetID    string
	beforeState string
	afterState  string
}

// lifecycleAuditFor narrows the audit log to the entries this worker is
// responsible for. Registration writes its own entries against the same agent,
// and an assertion that counted those would pass or fail for reasons that have
// nothing to do with a sweep. A worker that recorded its transitions under some
// other action name shows up here as no entry at all, which is a failure.
func (fx lifecycleFixture) lifecycleAuditFor(t *testing.T, agentID uuid.UUID) []auditEntry {
	t.Helper()

	rows, err := fx.pool.Query(context.Background(),
		`SELECT org_id, actor, action, target_type, target_id,
		        before ->> 'lifecycle_state', after ->> 'lifecycle_state'
		   FROM audit_log
		  WHERE target_id = $1 AND action IN ('agent.marked_idle', 'agent.marked_stale')
		  ORDER BY id`, agentID.String())
	require.NoError(t, err)
	defer rows.Close()

	var entries []auditEntry
	for rows.Next() {
		var e auditEntry
		var before, after *string
		require.NoError(t, rows.Scan(
			&e.orgID, &e.actor, &e.action, &e.targetType, &e.targetID, &before, &after))
		require.NotNil(t, before, "an audit entry must carry a before snapshot")
		require.NotNil(t, after, "an audit entry must carry an after snapshot")
		e.beforeState, e.afterState = *before, *after
		entries = append(entries, e)
	}
	require.NoError(t, rows.Err())
	return entries
}

// A sweep is the only thing that ever revises the server's opinion of an agent
// nobody is hearing from, so one fixture carries the whole story: who moves,
// who does not, what is written, and what is emphatically not.
//
// The subtests share one database and one sweep on purpose. Each testdb.New
// costs a PostgreSQL container, and they run in order against the state that
// sweep left.
func TestSweepMarksSilentAgents(t *testing.T) {
	fx := newLifecycleFixture(t, "lifecycle-sweep")

	// Beating within the interval, one live replica. Not silent, so not the
	// worker's business at all.
	fresh := fx.register(t, "fresh")
	fx.silentFor(t, fresh.AgentID, idleAfter/2)
	fx.credentialSeen(t, fresh.AgentToken, 5*time.Second)

	// Three replicas registered, one still inside the liveness window. Silent
	// past the idle threshold but nowhere near stale.
	quiet := fx.register(t, "quiet")
	quietSecond := fx.register(t, "quiet")
	quietThird := fx.register(t, "quiet")
	require.Equal(t, quiet.AgentID, quietSecond.AgentID, "three replicas are one agent")
	require.Equal(t, quiet.AgentID, quietThird.AgentID)
	fx.silentFor(t, quiet.AgentID, idleAfter+time.Minute)
	fx.setReplicaCount(t, quiet.AgentID, 3)
	fx.credentialSeen(t, quiet.AgentToken, 10*time.Second)
	fx.credentialSeen(t, quietSecond.AgentToken, 5*livenessWindow)
	fx.credentialSeen(t, quietThird.AgentToken, 5*livenessWindow)

	// Two replicas, both long dead, and the record still claims two. This is
	// the agent a frozen replica count actively misleads an operator about.
	abandoned := fx.register(t, "abandoned")
	abandonedSecond := fx.register(t, "abandoned")
	fx.silentFor(t, abandoned.AgentID, staleAfter+24*time.Hour)
	fx.setReplicaCount(t, abandoned.AgentID, 2)
	fx.credentialSeen(t, abandoned.AgentToken, 30*24*time.Hour)
	fx.credentialSeen(t, abandonedSecond.AgentToken, 30*24*time.Hour)

	// Already written off as idle by an earlier sweep, and has now been quiet
	// long enough to be stale: the idle to stale transition proper.
	lingering := fx.register(t, "lingering")
	fx.setLifecycle(t, lingering.AgentID, domain.LifecycleIdle)
	fx.silentFor(t, lingering.AgentID, staleAfter+time.Hour)

	// Two human decisions, silent for a very long time. Neither may be moved.
	retired := fx.register(t, "retired")
	fx.setLifecycle(t, retired.AgentID, domain.LifecycleRetired)
	fx.silentFor(t, retired.AgentID, staleAfter+100*24*time.Hour)
	fx.setReplicaCount(t, retired.AgentID, 5)

	quarantined := fx.register(t, "quarantined")
	fx.setLifecycle(t, quarantined.AgentID, domain.LifecycleQuarantined)
	fx.silentFor(t, quarantined.AgentID, staleAfter+100*24*time.Hour)
	fx.setReplicaCount(t, quarantined.AgentID, 7)

	// A retired agent that is still running. The worker must not promote it
	// back to active on the strength of a fresh heartbeat: retirement is a
	// human decision and only a human undoes it.
	retiredRunning := fx.register(t, "retired-running")
	fx.setLifecycle(t, retiredRunning.AgentID, domain.LifecycleRetired)
	fx.silentFor(t, retiredRunning.AgentID, 5*time.Second)
	fx.credentialSeen(t, retiredRunning.AgentToken, 5*time.Second)
	fx.setReplicaCount(t, retiredRunning.AgentID, 1)

	agentsBefore := fx.countRows(t, "agents")
	credentialsBefore := fx.countRows(t, "agent_credentials")
	revisionsBefore := fx.countRows(t, "agent_revisions")
	require.Equal(t, 7, agentsBefore, "seven agents were registered")

	result := fx.sweep(t)

	t.Run("an agent heartbeating recently stays active", func(t *testing.T) {
		require.Equal(t, agentState{string(domain.LifecycleActive), 1}, fx.read(t, fresh.AgentID))
		require.Empty(t, fx.lifecycleAuditFor(t, fresh.AgentID),
			"an agent the sweep did not move has nothing to audit")
	})

	t.Run("silence past the idle threshold is idle", func(t *testing.T) {
		require.Equal(t, string(domain.LifecycleIdle), fx.read(t, quiet.AgentID).lifecycleState)
	})

	t.Run("silence past the stale threshold is stale", func(t *testing.T) {
		require.Equal(t, string(domain.LifecycleStale), fx.read(t, abandoned.AgentID).lifecycleState)
		require.Equal(t, string(domain.LifecycleStale), fx.read(t, lingering.AgentID).lifecycleState,
			"an agent already idle goes on to stale")
	})

	t.Run("retired and quarantined are never touched", func(t *testing.T) {
		require.Equal(t, agentState{string(domain.LifecycleRetired), 5}, fx.read(t, retired.AgentID),
			"a retired agent silent for months is still retired, not idle or stale")
		require.Equal(t, agentState{string(domain.LifecycleQuarantined), 7}, fx.read(t, quarantined.AgentID),
			"a quarantined agent silent for months is still quarantined")
		require.Equal(t, agentState{string(domain.LifecycleRetired), 1}, fx.read(t, retiredRunning.AgentID),
			"and a retired agent that is still running is not revived to active")

		for _, id := range []uuid.UUID{retired.AgentID, quarantined.AgentID, retiredRunning.AgentID} {
			require.Empty(t, fx.lifecycleAuditFor(t, id),
				"a state the worker must not touch has nothing to audit")
		}
	})

	t.Run("nothing is deleted", func(t *testing.T) {
		require.Equal(t, agentsBefore, fx.countRows(t, "agents"),
			"a stale record is an audit finding, not cleanup work")
		require.Equal(t, credentialsBefore, fx.countRows(t, "agent_credentials"),
			"the evidence the replica count is derived from must survive too")
	})

	t.Run("no revisions are written", func(t *testing.T) {
		require.Equal(t, revisionsBefore, fx.countRows(t, "agent_revisions"),
			"lifecycle state is server-observed, not a change to what the record says")
	})

	t.Run("every transition is audited", func(t *testing.T) {
		cases := []struct {
			name   string
			id     uuid.UUID
			action string
			before string
			after  string
		}{
			{"idle", quiet.AgentID, "agent.marked_idle", "active", "idle"},
			{"stale from active", abandoned.AgentID, "agent.marked_stale", "active", "stale"},
			{"stale from idle", lingering.AgentID, "agent.marked_stale", "idle", "stale"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				entries := fx.lifecycleAuditFor(t, tc.id)
				require.Len(t, entries, 1, "one transition, one audit entry")
				require.Equal(t, auditEntry{
					orgID:       fx.orgID,
					actor:       "system:lifecycle",
					action:      tc.action,
					targetType:  "agent",
					targetID:    tc.id.String(),
					beforeState: tc.before,
					afterState:  tc.after,
				}, entries[0])
			})
		}
	})

	t.Run("a fully silent agent reports zero replicas", func(t *testing.T) {
		require.Equal(t, 0, fx.read(t, abandoned.AgentID).replicaCount,
			"the worker must not inherit the heartbeat's floor of 1: for an agent "+
				"nobody has heard from, zero is the honest answer")
		require.Equal(t, 1, fx.read(t, quiet.AgentID).replicaCount,
			"one replica is still inside the liveness window")
	})

	t.Run("the result counts what moved", func(t *testing.T) {
		require.Equal(t, worker.SweepResult{
			MarkedIdle:  1,
			MarkedStale: 2,
			// quiet 3 -> 1, abandoned 2 -> 0, lingering 1 -> 0.
			ReplicaCountsUpdated: 3,
		}, result)
	})

	t.Run("a second sweep is a no-op", func(t *testing.T) {
		states := map[uuid.UUID]agentState{}
		for _, id := range []uuid.UUID{
			fresh.AgentID, quiet.AgentID, abandoned.AgentID, lingering.AgentID,
			retired.AgentID, quarantined.AgentID, retiredRunning.AgentID,
		} {
			states[id] = fx.read(t, id)
		}
		audits := fx.countRows(t, "audit_log")

		require.Equal(t, worker.SweepResult{}, fx.sweep(t),
			"nothing has changed since the last sweep, so nothing is left to do")

		for id, want := range states {
			require.Equal(t, want, fx.read(t, id), "agent %s moved on an idempotent sweep", id)
		}
		require.Equal(t, audits, fx.countRows(t, "audit_log"),
			"a worker on a short ticker must not write an audit entry per tick")
		require.Equal(t, revisionsBefore, fx.countRows(t, "agent_revisions"))
	})
}

// The thresholds are exact, and both edges are asserted. A test that only
// checked the far side would pass just as happily against a worker whose
// threshold was off by an hour.
//
// The Run subtests share this fixture's database because a PostgreSQL container
// per test function is the expensive thing here.
func TestSweepThresholdsAreExact(t *testing.T) {
	fx := newLifecycleFixture(t, "lifecycle-boundary")

	// Silent for exactly the idle threshold: reached, not passed.
	atIdle := fx.register(t, "at-idle")
	fx.silentFor(t, atIdle.AgentID, idleAfter)

	// One millisecond further, and the server has waited long enough.
	pastIdle := fx.register(t, "past-idle")
	fx.silentFor(t, pastIdle.AgentID, idleAfter+time.Millisecond)

	atStale := fx.register(t, "at-stale")
	fx.setLifecycle(t, atStale.AgentID, domain.LifecycleIdle)
	fx.silentFor(t, atStale.AgentID, staleAfter)

	pastStale := fx.register(t, "past-stale")
	fx.setLifecycle(t, pastStale.AgentID, domain.LifecycleIdle)
	fx.silentFor(t, pastStale.AgentID, staleAfter+time.Millisecond)

	t.Run("the idle threshold", func(t *testing.T) {
		fx.sweep(t)

		require.Equal(t, string(domain.LifecycleActive), fx.read(t, atIdle.AgentID).lifecycleState,
			"an agent silent for exactly the threshold has not passed it")
		require.Equal(t, string(domain.LifecycleIdle), fx.read(t, pastIdle.AgentID).lifecycleState,
			"one millisecond past the threshold is past the threshold")
	})

	t.Run("the stale threshold", func(t *testing.T) {
		require.Equal(t, string(domain.LifecycleIdle), fx.read(t, atStale.AgentID).lifecycleState,
			"an agent silent for exactly the stale threshold is still only idle")
		require.Equal(t, string(domain.LifecycleStale), fx.read(t, pastStale.AgentID).lifecycleState,
			"one millisecond past the threshold is past the threshold")
	})

	// A worker that leaked its ticker or ignored cancellation would keep
	// sweeping after the process asked it to stop.
	t.Run("Run returns when its context is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan error, 1)
		go func() { done <- fx.worker.Run(ctx, time.Hour) }()

		cancel()

		select {
		case err := <-done:
			require.NoError(t, err, "cancellation is how the worker is asked to stop, not a failure")
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after its context was cancelled")
		}
	})

	t.Run("Run returns immediately for an already-cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		require.NoError(t, fx.worker.Run(ctx, time.Hour))
		require.Less(t, time.Since(start), time.Second,
			"Run must not wait for a tick it will never act on")
	})
}
