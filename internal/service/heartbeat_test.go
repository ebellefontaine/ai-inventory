package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/auth"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

// heartbeatFixture is a registration fixture plus the service under test. Every
// credential these tests use is issued by a real registration, so the tests
// exercise the actual contract between the two services rather than a
// hand-inserted row that happens to look like one.
type heartbeatFixture struct {
	registrationFixture
	hb *service.HeartbeatService
}

func newHeartbeatFixture(t *testing.T, slug string) heartbeatFixture {
	t.Helper()

	fx := newRegistrationFixture(t, slug)
	return heartbeatFixture{
		registrationFixture: fx,
		hb:                  service.NewHeartbeatService(fx.pool, testHeartbeatInterval),
	}
}

// register runs one registration and returns its result. Calling it twice with
// the same instance key models two replicas of one deployment: one agent
// record, two credentials.
func (fx heartbeatFixture) register(t *testing.T, instanceKey string) service.RegisterResult {
	t.Helper()

	res, err := fx.svc.Register(context.Background(), service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   minimalDescriptor(t, instanceKey),
	})
	require.NoError(t, err)
	return res
}

func (fx heartbeatFixture) beat(t *testing.T, agentID uuid.UUID, token string) service.HeartbeatResult {
	t.Helper()

	res, err := fx.hb.Heartbeat(context.Background(), service.HeartbeatInput{
		AgentID:    agentID,
		AgentToken: token,
	})
	require.NoError(t, err)
	return res
}

// agentLiveness holds the columns a heartbeat is allowed to move.
type agentLiveness struct {
	lifecycleState  string
	replicaCount    int
	lastHeartbeatAt time.Time
}

func readAgentLiveness(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) agentLiveness {
	t.Helper()

	var l agentLiveness
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT lifecycle_state, replica_count, last_heartbeat_at FROM agents WHERE id = $1`, id,
	).Scan(&l.lifecycleState, &l.replicaCount, &l.lastHeartbeatAt))
	return l
}

// credentialLastSeen returns a credential's last_seen_at, nil when it has never
// heartbeated. The credential is found by hash, the same way the service finds
// it, so a test can hold the plaintext and never the stored form.
func credentialLastSeen(t *testing.T, pool *pgxpool.Pool, token string) *time.Time {
	t.Helper()

	var at *time.Time
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT last_seen_at FROM agent_credentials WHERE token_hash = $1`, auth.HashToken(token),
	).Scan(&at))
	return at
}

func execOnCredential(t *testing.T, pool *pgxpool.Pool, sql, token string, args ...any) {
	t.Helper()

	tag, err := pool.Exec(context.Background(), sql, append([]any{auth.HashToken(token)}, args...)...)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "the fixture must have changed exactly one credential")
}

func revokeCredential(t *testing.T, pool *pgxpool.Pool, token string) {
	t.Helper()
	execOnCredential(t, pool,
		`UPDATE agent_credentials SET revoked_at = now() WHERE token_hash = $1`, token)
}

func expireCredential(t *testing.T, pool *pgxpool.Pool, token string) {
	t.Helper()
	execOnCredential(t, pool,
		`UPDATE agent_credentials SET expires_at = now() - interval '1 hour' WHERE token_hash = $1`, token)
}

// backdateLastSeen pushes a credential's last heartbeat into the past, which is
// how a test simulates a replica that has gone quiet without waiting for real
// time to pass.
func backdateLastSeen(t *testing.T, pool *pgxpool.Pool, token string, seconds int) {
	t.Helper()
	execOnCredential(t, pool,
		`UPDATE agent_credentials SET last_seen_at = now() - make_interval(secs => $2)
		  WHERE token_hash = $1`, token, seconds)
}

func setLifecycle(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, state domain.LifecycleState) {
	t.Helper()

	tag, err := pool.Exec(context.Background(),
		`UPDATE agents SET lifecycle_state = $2 WHERE id = $1`, id, string(state))
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
}

// backdateHeartbeat ages an agent's last_heartbeat_at so that a heartbeat
// advancing it is observable without depending on clock resolution.
func backdateHeartbeat(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) time.Time {
	t.Helper()

	var at time.Time
	require.NoError(t, pool.QueryRow(context.Background(),
		`UPDATE agents SET last_heartbeat_at = now() - interval '1 hour'
		  WHERE id = $1 RETURNING last_heartbeat_at`, id).Scan(&at))
	return at
}

// A heartbeat is the cheapest thing an agent does: it proves liveness, gets
// told how often to come back, and learns what the server now believes about
// it.
//
// The interval must be the server's configured value. testHeartbeatInterval is
// deliberately not the production default of 60, so an implementation that
// returns a hard-coded constant fails here.
//
// The replica count of a single fresh replica is 1, and it is floored there
// rather than computed to it: the caller heartbeating is itself proof that one
// replica is alive.
func TestHeartbeatRecordsLivenessAndReturnsServerInterval(t *testing.T) {
	fx := newHeartbeatFixture(t, "heartbeat-liveness")

	agent := fx.register(t, "invoice-triage")
	require.Nil(t, credentialLastSeen(t, fx.pool, agent.AgentToken),
		"a freshly issued credential has never been seen")

	aged := backdateHeartbeat(t, fx.pool, agent.AgentID)

	res := fx.beat(t, agent.AgentID, agent.AgentToken)
	require.Equal(t, testHeartbeatInterval, res.HeartbeatIntervalSeconds,
		"the interval is server policy, not the agent's choice")
	require.Equal(t, domain.LifecycleActive, res.LifecycleState)
	require.Equal(t, 1, res.ReplicaCount)

	stored := readAgentLiveness(t, fx.pool, agent.AgentID)
	require.True(t, stored.lastHeartbeatAt.After(aged),
		"the heartbeat must advance last_heartbeat_at")
	require.Equal(t, 1, stored.replicaCount)

	seen := credentialLastSeen(t, fx.pool, agent.AgentToken)
	require.NotNil(t, seen, "the heartbeating credential must record its own last_seen_at")
	require.True(t, seen.After(aged))
}

// The three failures stay distinguishable here for the operator's sake. Task
// 20's HTTP layer collapses them into one indistinguishable 401; doing that at
// this layer would destroy the diagnostic.
func TestHeartbeatRejectsInvalidCredentials(t *testing.T) {
	fx := newHeartbeatFixture(t, "heartbeat-invalid")

	first := fx.register(t, "invoice-triage")
	second := fx.register(t, "invoice-triage")
	require.Equal(t, first.AgentID, second.AgentID, "two replicas, one agent record")

	revokeCredential(t, fx.pool, first.AgentToken)
	expireCredential(t, fx.pool, second.AgentToken)

	before := readAgentLiveness(t, fx.pool, first.AgentID)

	cases := []struct {
		name  string
		token string
		want  error
	}{
		{"never issued", "aiinv_ac_neverissued", service.ErrCredentialNotFound},
		{"revoked", first.AgentToken, service.ErrCredentialRevoked},
		{"expired", second.AgentToken, service.ErrCredentialExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fx.hb.Heartbeat(context.Background(), service.HeartbeatInput{
				AgentID:    first.AgentID,
				AgentToken: tc.token,
			})
			require.ErrorIs(t, err, tc.want)
			require.Equal(t, before, readAgentLiveness(t, fx.pool, first.AgentID),
				"a rejected heartbeat must leave the agent untouched")
		})
	}

	require.Nil(t, credentialLastSeen(t, fx.pool, first.AgentToken))
	require.Nil(t, credentialLastSeen(t, fx.pool, second.AgentToken))
}

// The credential is valid, unrevoked, and unexpired — it simply belongs to
// somebody else. Without the ownership check any agent in the installation
// could keep any other agent looking alive, and "last seen" would stop meaning
// anything at all.
func TestHeartbeatRejectsCredentialBelongingToAnotherAgent(t *testing.T) {
	fx := newHeartbeatFixture(t, "heartbeat-mismatch")

	mine := fx.register(t, "invoice-triage")
	theirs := fx.register(t, "expense-triage")
	require.NotEqual(t, mine.AgentID, theirs.AgentID)

	before := readAgentLiveness(t, fx.pool, mine.AgentID)
	theirsBefore := readAgentLiveness(t, fx.pool, theirs.AgentID)
	revisions := countRows(t, fx.pool, "agent_revisions")

	_, err := fx.hb.Heartbeat(context.Background(), service.HeartbeatInput{
		AgentID:    mine.AgentID,
		AgentToken: theirs.AgentToken,
	})
	require.ErrorIs(t, err, service.ErrCredentialAgentMismatch)

	require.Equal(t, before, readAgentLiveness(t, fx.pool, mine.AgentID),
		"an agent must not be kept alive by somebody else's credential")
	require.Equal(t, theirsBefore, readAgentLiveness(t, fx.pool, theirs.AgentID),
		"and the credential's real owner must not be touched either")
	require.Nil(t, credentialLastSeen(t, fx.pool, theirs.AgentToken),
		"a rejected heartbeat must not stamp the credential it was rejected for")
	require.Equal(t, revisions, countRows(t, fx.pool, "agent_revisions"))
}

// A heartbeat is not a change to the descriptor. Writing a revision for each
// one would bury the handful of real changes under a revision per agent per
// interval, and the history that makes revisions worth keeping would be
// unreadable within a day.
func TestHeartbeatWritesNoRevision(t *testing.T) {
	fx := newHeartbeatFixture(t, "heartbeat-no-revision")

	agent := fx.register(t, "invoice-triage")
	before := countRows(t, fx.pool, "agent_revisions")
	require.Equal(t, 1, before, "registering wrote exactly one revision")

	for range 3 {
		fx.beat(t, agent.AgentID, agent.AgentToken)
	}

	require.Equal(t, before, countRows(t, fx.pool, "agent_revisions"),
		"heartbeats must not append to the revision history")
}

// Idle and stale are the server's own inference from silence. A heartbeat is
// direct evidence against that inference, so it overrides it.
func TestHeartbeatRevivesIdleAndStaleAgents(t *testing.T) {
	fx := newHeartbeatFixture(t, "heartbeat-revive")

	agent := fx.register(t, "invoice-triage")

	for _, state := range []domain.LifecycleState{domain.LifecycleIdle, domain.LifecycleStale} {
		setLifecycle(t, fx.pool, agent.AgentID, state)

		res := fx.beat(t, agent.AgentID, agent.AgentToken)
		require.Equalf(t, domain.LifecycleActive, res.LifecycleState,
			"a heartbeat must revive a %s agent", state)
		require.Equal(t, string(domain.LifecycleActive),
			readAgentLiveness(t, fx.pool, agent.AgentID).lifecycleState)
	}
}

// Retired and quarantined are human decisions. An agent that keeps heartbeating
// after being retired is exactly the situation an operator needs to see, and
// letting the agent quietly undo the decision by continuing to run would hide
// it. Quarantine matters more still: it is what an operator reaches for when an
// agent is suspect, and a suspect agent must not be able to release itself.
func TestHeartbeatDoesNotReviveRetiredOrQuarantinedAgents(t *testing.T) {
	fx := newHeartbeatFixture(t, "heartbeat-no-revive")

	agent := fx.register(t, "invoice-triage")

	for _, state := range []domain.LifecycleState{domain.LifecycleRetired, domain.LifecycleQuarantined} {
		setLifecycle(t, fx.pool, agent.AgentID, state)

		res := fx.beat(t, agent.AgentID, agent.AgentToken)
		require.Equalf(t, state, res.LifecycleState,
			"a %s agent must stay %s, and the result must say so", state, state)
		require.Equal(t, string(state),
			readAgentLiveness(t, fx.pool, agent.AgentID).lifecycleState)
	}
}

// Registration issues one credential per call, so three replicas of one
// deployment hold three credentials against one agent record. That is what lets
// the server count replicas from evidence it holds rather than believe a number
// the agent reports about itself — a self-reported count could be inflated at
// will, and this one cannot be.
func TestHeartbeatDerivesReplicaCountFromLiveCredentials(t *testing.T) {
	fx := newHeartbeatFixture(t, "heartbeat-replicas")

	replicas := []service.RegisterResult{
		fx.register(t, "invoice-triage"),
		fx.register(t, "invoice-triage"),
		fx.register(t, "invoice-triage"),
	}
	agentID := replicas[0].AgentID
	for _, r := range replicas {
		require.Equal(t, agentID, r.AgentID, "three replicas are one agent")
	}

	for i, r := range replicas {
		res := fx.beat(t, agentID, r.AgentToken)
		require.Equalf(t, i+1, res.ReplicaCount,
			"after %d replicas have heartbeated the count is %d", i+1, i+1)
	}

	require.Equal(t, 3, readAgentLiveness(t, fx.pool, agentID).replicaCount,
		"the derived count must be persisted, not just returned")
}

// The count is a window, not a tally. A replica that stops heartbeating must
// drop out of it, or replica_count becomes a monotonic count of every process
// that ever registered and an operator reading it learns nothing about now.
//
// One missed beat is normal — a slow request, a restart — so the window is two
// intervals. Two missed beats is not normal, and that is where the replica
// stops counting.
func TestHeartbeatReplicaCountDecaysOutsideTheLivenessWindow(t *testing.T) {
	fx := newHeartbeatFixture(t, "heartbeat-decay")

	replicas := []service.RegisterResult{
		fx.register(t, "invoice-triage"),
		fx.register(t, "invoice-triage"),
		fx.register(t, "invoice-triage"),
	}
	agentID := replicas[0].AgentID

	for _, r := range replicas {
		fx.beat(t, agentID, r.AgentToken)
	}
	require.Equal(t, 3, readAgentLiveness(t, fx.pool, agentID).replicaCount)

	// Just inside the window: one missed beat must not cost a replica.
	backdateLastSeen(t, fx.pool, replicas[0].AgentToken, testHeartbeatInterval+1)
	res := fx.beat(t, agentID, replicas[1].AgentToken)
	require.Equal(t, 3, res.ReplicaCount,
		"a replica that has missed one beat is still counted")

	// Past two intervals it is gone.
	backdateLastSeen(t, fx.pool, replicas[0].AgentToken, 2*testHeartbeatInterval+1)
	res = fx.beat(t, agentID, replicas[1].AgentToken)
	require.Equal(t, 2, res.ReplicaCount,
		"a replica silent for two intervals must drop out of the count")
	require.Equal(t, 2, readAgentLiveness(t, fx.pool, agentID).replicaCount)
}

// Revoking a replica's credential is how an operator kills one compromised
// process without unregistering the other nineteen. The killed replica must
// stop counting immediately rather than lingering until its window expires.
func TestHeartbeatExcludesRevokedCredentialsFromReplicaCount(t *testing.T) {
	fx := newHeartbeatFixture(t, "heartbeat-revoked-replica")

	first := fx.register(t, "invoice-triage")
	second := fx.register(t, "invoice-triage")
	agentID := first.AgentID

	fx.beat(t, agentID, first.AgentToken)
	require.Equal(t, 2, fx.beat(t, agentID, second.AgentToken).ReplicaCount)

	revokeCredential(t, fx.pool, first.AgentToken)

	res := fx.beat(t, agentID, second.AgentToken)
	require.Equal(t, 1, res.ReplicaCount,
		"a revoked credential is not a live replica")
	require.Equal(t, 1, readAgentLiveness(t, fx.pool, agentID).replicaCount)
}
