package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ebellefontaine/ai-inventory/internal/auth"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
)

// The four failure modes below are distinguished for audit and operator
// diagnostics only: an administrator chasing a fleet that has gone quiet needs
// to know whether a credential was revoked, has expired, never existed, or is
// being presented for the wrong agent.
//
// The HTTP layer in Task 20 must collapse all four into one indistinguishable
// 401. A caller who can tell "revoked" from "not found" has a
// credential-enumeration oracle: they can confirm which guessed credentials are
// real, which is precisely the information a brute-force search needs and the
// response must not carry. ErrCredentialAgentMismatch leaks more than the
// others if returned — it confirms a credential is valid while naming the agent
// it is not valid for — so it must be collapsed with the rest rather than
// treated as the "safe" one to report.
var (
	// ErrCredentialNotFound means no credential with that hash exists.
	ErrCredentialNotFound = errors.New("agent credential not found")
	// ErrCredentialRevoked means the credential exists but an operator killed it.
	ErrCredentialRevoked = errors.New("agent credential revoked")
	// ErrCredentialExpired means the credential exists but is past its expires_at.
	ErrCredentialExpired = errors.New("agent credential expired")
	// ErrCredentialAgentMismatch means the credential is valid but belongs to a
	// different agent than the one the heartbeat names. Without this check, any
	// agent in the installation could keep any other agent looking alive.
	ErrCredentialAgentMismatch = errors.New("agent credential belongs to a different agent")
)

// livenessWindowIntervals is how many heartbeat intervals a credential may go
// quiet before it stops counting as a live replica.
//
// One missed beat is ordinary: a slow request, a rolling restart, a scheduler
// pause. Two is not, and a window of exactly one interval would make the count
// flap every time a replica's timer drifted a second past the server's. Two
// intervals is the smallest window that tolerates the normal case without
// holding a dead replica in the count for long.
const livenessWindowIntervals = 2

// HeartbeatService records that an agent is still running and derives what the
// server can observe about it from that fact.
type HeartbeatService struct {
	pool *pgxpool.Pool
	// heartbeatIntervalSeconds is how often the server wants to hear from an
	// agent. It is returned on every beat rather than fixed at registration, so
	// an operator can dial a whole fleet up or down and have it take effect
	// within one interval without redeploying anything.
	heartbeatIntervalSeconds int
	// livenessWindowSeconds is how recently a credential must have been seen to
	// count as a live replica.
	livenessWindowSeconds int
}

// NewHeartbeatService wires the service. The liveness window is derived from
// the interval rather than configured separately: the two cannot be allowed to
// drift apart, because a window shorter than the interval would make every
// agent's replica count collapse to 1.
func NewHeartbeatService(pool *pgxpool.Pool, heartbeatIntervalSeconds int) *HeartbeatService {
	return &HeartbeatService{
		pool:                     pool,
		heartbeatIntervalSeconds: heartbeatIntervalSeconds,
		livenessWindowSeconds:    heartbeatIntervalSeconds * livenessWindowIntervals,
	}
}

// HeartbeatInput is one beat: which agent claims to be alive, and the
// credential that has to back the claim up.
//
// The agent id is carried alongside the credential rather than inferred from it
// alone so that the two can be checked against each other. A heartbeat is an
// assertion about a specific inventory record, and an assertion nobody checks
// the authority for is how "last seen" stops meaning anything.
type HeartbeatInput struct {
	AgentID    uuid.UUID
	AgentToken string
}

// HeartbeatResult is what the agent learns in return: when to come back, what
// the server now believes about its lifecycle, and how many replicas of it the
// server can currently see.
type HeartbeatResult struct {
	HeartbeatIntervalSeconds int
	LifecycleState           domain.LifecycleState
	ReplicaCount             int
}

// Heartbeat records liveness for one replica and recomputes what that implies
// for the agent as a whole.
//
// The operation is deliberately small. It is the only call an agent makes on a
// timer, so it is one request per agent per interval for the life of the fleet:
// two statements and no revision. Everything expensive belongs somewhere else.
//
// Writing no revision is the specific omission worth naming. A heartbeat is not
// a change to the descriptor — nothing the agent said about itself is different
// — and appending one per agent per interval would bury the handful of real
// changes under millions of identical snapshots. The history that makes
// revisions worth keeping would become unreadable within a day.
//
// The replica count is derived, never reported. Registration issues a separate
// credential per call, so twenty replicas of one deployment hold twenty
// distinct credentials against one agent record, and the server can simply
// count the ones that have beaten recently. That is evidence the server already
// holds rather than a number the agent asserts about itself: an agent cannot
// inflate it, and no agent cooperation is required to collect it. It is the
// same provenance rule that makes ownership come from the enrollment token
// instead of the payload.
//
// The count is floored at 1 because the caller heartbeating is itself proof
// that one replica is alive — a clock skew or a window edge must never let a
// running agent report zero of itself.
func (s *HeartbeatService) Heartbeat(ctx context.Context, in HeartbeatInput) (HeartbeatResult, error) {
	// One transaction: the credential's last_seen_at and the count derived from
	// it must not be able to disagree. Stamping the credential and then failing
	// to recompute would leave an agent whose replica count silently excludes a
	// replica that just proved itself alive.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return HeartbeatResult{}, fmt.Errorf("heartbeat: %w", err)
	}
	// Rollback after a successful commit is a no-op, so this is safe on every
	// path — including the four rejections below, which is what guarantees a
	// refused heartbeat writes nothing at all.
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		credentialID uuid.UUID
		agentID      uuid.UUID
		expiresAt    *time.Time
		revokedAt    *time.Time
	)
	err = tx.QueryRow(ctx, resolveCredentialSQL, auth.HashToken(in.AgentToken)).Scan(
		&credentialID, &agentID, &expiresAt, &revokedAt,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return HeartbeatResult{}, ErrCredentialNotFound
	case err != nil:
		return HeartbeatResult{}, fmt.Errorf("heartbeat: resolve credential: %w", err)
	}

	// Revocation is reported ahead of expiry, and both ahead of the ownership
	// mismatch. An operator who deliberately killed a credential wants to see
	// that rather than the incidental fact that it also aged out, and a
	// credential that is dead in its own right is not worth reporting as
	// somebody else's.
	switch {
	case revokedAt != nil:
		return HeartbeatResult{}, ErrCredentialRevoked
	case expiresAt != nil && !expiresAt.After(time.Now()):
		return HeartbeatResult{}, ErrCredentialExpired
	case agentID != in.AgentID:
		return HeartbeatResult{}, ErrCredentialAgentMismatch
	}

	if _, err := tx.Exec(ctx, touchCredentialSQL, credentialID); err != nil {
		return HeartbeatResult{}, fmt.Errorf("heartbeat: record credential liveness: %w", err)
	}

	var (
		state        string
		replicaCount int
	)
	err = tx.QueryRow(ctx, recordHeartbeatSQL, agentID, float64(s.livenessWindowSeconds)).
		Scan(&state, &replicaCount)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Only a concurrent hard delete of the agent can get here: the
		// credential's foreign key guarantees the row existed a statement ago,
		// and deleting it cascades to the credential.
		return HeartbeatResult{}, fmt.Errorf("heartbeat: agent %s no longer exists", agentID)
	case err != nil:
		return HeartbeatResult{}, fmt.Errorf("heartbeat: record agent liveness: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return HeartbeatResult{}, fmt.Errorf("heartbeat: %w", err)
	}

	return HeartbeatResult{
		HeartbeatIntervalSeconds: s.heartbeatIntervalSeconds,
		LifecycleState:           domain.LifecycleState(state),
		ReplicaCount:             replicaCount,
	}, nil
}

// The lookup is by hash alone, and deliberately does not filter on revoked_at,
// expires_at, or agent_id in SQL. A query that returned no row for a revoked
// credential could not tell the audit trail apart from a credential that never
// existed, and that distinction is the whole point of the sentinel errors. The
// same goes for the ownership check: filtering on agent_id here would report a
// stolen credential as one that does not exist.
const resolveCredentialSQL = `
SELECT id, agent_id, expires_at, revoked_at
FROM agent_credentials
WHERE token_hash = $1;
`

// One credential, one replica. This is the row that makes the replica count
// observable: without it the server would have to take the agent's word for how
// many copies of itself are running.
const touchCredentialSQL = `
UPDATE agent_credentials SET last_seen_at = now() WHERE id = $1;
`

// The count is recomputed from the credential table rather than incremented,
// which is what makes it a measure of now rather than a tally of everything
// that ever registered. A counter would only ever go up; this goes down again
// the moment a replica stops beating, is revoked, or expires.
//
// now() is the transaction's start time, so the timestamp written to
// last_seen_at a statement earlier and the window this statement measures
// against come from one clock reading. The credential that just beat therefore
// cannot fall outside its own window.
//
// idle and stale are the server's own inference from silence, and a heartbeat
// is direct evidence against it, so it is overridden. retired and quarantined
// are human decisions and are left exactly alone: an agent that keeps
// heartbeating after being retired is precisely what an operator needs to see,
// and a quarantined agent must not be able to release itself by continuing to
// run. The CASE mirrors the one in registerAgentSQL, for the same reasons.
//
// updated_at is not touched. It marks a change to what the record says, and a
// heartbeat changes only what the server has observed; bumping it every
// interval for every agent would make "recently changed" useless — the same
// reason no revision is written.
const recordHeartbeatSQL = `
UPDATE agents SET
    last_heartbeat_at = now(),
    lifecycle_state = CASE
        WHEN agents.lifecycle_state IN ('idle','stale') THEN 'active'
        ELSE agents.lifecycle_state
    END,
    replica_count = GREATEST(1, (
        SELECT count(*)
        FROM agent_credentials c
        WHERE c.agent_id = agents.id
          AND c.revoked_at IS NULL
          AND (c.expires_at IS NULL OR c.expires_at > now())
          AND c.last_seen_at > now() - make_interval(secs => $2)
    ))
WHERE agents.id = $1
RETURNING lifecycle_state, replica_count;
`
