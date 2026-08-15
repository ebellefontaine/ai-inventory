// Package worker holds the background loops that keep the inventory honest
// about agents nobody is hearing from.
package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ebellefontaine/ai-inventory/internal/domain"
)

// The audit vocabulary this worker writes. The actor names the worker rather
// than a person because nobody asked for these transitions: they are the
// server's own inference from silence, and an audit reader must be able to tell
// them apart from a human retiring an agent.
const (
	actorLifecycle    = "system:lifecycle"
	actionMarkedIdle  = "agent.marked_idle"
	actionMarkedStale = "agent.marked_stale"
	targetTypeAgent   = "agent"
)

// livenessWindowIntervals is how many heartbeat intervals a credential may go
// quiet before it stops counting as a live replica. It mirrors the constant of
// the same name in internal/service/heartbeat.go, deliberately: the count this
// worker recomputes and the count a heartbeat recomputes must mean the same
// thing, or an agent's replica count would jump every time the last replica
// died and the two definitions disagreed about when.
const livenessWindowIntervals = 2

// secondsPerDay converts StaleAfterDays into an interval of fixed length.
//
// A calendar day would be the other reading, and it is the wrong one here: on
// timestamptz, PostgreSQL's day arithmetic is relative to the session time zone,
// so "seven days ago" would move by an hour twice a year and an agent could
// cross the stale threshold because the clocks changed rather than because it
// went quiet.
const secondsPerDay = 24 * 60 * 60

// Config is the policy a sweep applies: how often agents are expected to
// report, how much silence is idle, and how much is stale.
type Config struct {
	HeartbeatIntervalSeconds int
	IdleAfterMissedBeats     int
	StaleAfterDays           int
	// Now is the clock every threshold is measured against. It is injected so
	// that a test can reason about nine days of silence without waiting nine
	// days, or sleeping at all. Defaults to time.Now.
	Now func() time.Time
}

// LifecycleWorker records what the server can infer about agents that have
// stopped reporting.
//
// It deletes nothing, ever. An agent that goes quiet becomes idle and then
// stale, and a stale record is not cleanup work left undone: it is an audit
// finding, and the evidence somebody will eventually need. The same principle
// makes revoking a token flag its agents rather than remove them.
type LifecycleWorker struct {
	pool *pgxpool.Pool
	cfg  Config
}

// New wires the worker, defaulting the clock to the real one.
func New(pool *pgxpool.Pool, cfg Config) *LifecycleWorker {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &LifecycleWorker{pool: pool, cfg: cfg}
}

// SweepResult is what one pass changed. The three counts are independent: an
// agent that both crossed a threshold and lost its last replica is counted
// once under the transition it made and once under ReplicaCountsUpdated.
type SweepResult struct {
	MarkedIdle  int
	MarkedStale int
	// ReplicaCountsUpdated counts agents whose replica_count actually moved,
	// not agents whose count was recomputed. A steady-state sweep therefore
	// reports zero rather than the size of the silent fleet.
	ReplicaCountsUpdated int
}

// Sweep performs one pass over the agents that have gone quiet. It is safe to
// call repeatedly: a sweep that finds nothing new writes nothing at all, which
// is what makes the worker safe to put on a short ticker.
//
// Two things happen to every silent agent, and the second is the reason this
// worker recomputes anything rather than only relabelling:
//
//   - Its lifecycle state is moved on if the silence has passed a threshold.
//     retired and quarantined are excluded entirely. They are decisions a human
//     made, and this worker must not move an agent out of them in either
//     direction: not on to idle or stale, and not back to active either.
//   - Its replica_count is recomputed from the credentials that are still
//     beating. A heartbeat already does this, but only while at least one
//     replica survives to send one. When the last replica of an agent dies the
//     count freezes at its final value, and "3 replicas" on a record nobody has
//     heard from in a month is worse than no number at all. This worker is
//     already visiting exactly those agents.
//
// The count written here is not floored at 1. heartbeat.go floors it because
// the caller is itself proof that one replica is alive; here there is no
// caller, nothing is alive, and zero is the honest answer.
//
// No revision is written. A revision records a change to what the record says
// about itself, and going idle changes only what the server has observed about
// it. update.go and heartbeat.go decline to write one for the same reason.
func (w *LifecycleWorker) Sweep(ctx context.Context) (SweepResult, error) {
	now := w.cfg.Now()

	// One transaction: an agent whose state moved without the audit entry that
	// explains it is a record that changed for no recorded reason, which is
	// precisely what an audit trail exists to prevent.
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return SweepResult{}, fmt.Errorf("lifecycle sweep: %w", err)
	}
	// Rollback after a successful commit is a no-op, so this is safe on every
	// path.
	defer func() { _ = tx.Rollback(ctx) }()

	transitions, result, err := sweepAgents(ctx, tx, now, w.thresholds())
	if err != nil {
		return SweepResult{}, err
	}

	for _, t := range transitions {
		if _, err := tx.Exec(ctx, writeAuditSQL,
			t.orgID, actorLifecycle, t.action(), targetTypeAgent, t.agentID.String(),
			t.beforeSnapshot, t.afterSnapshot,
		); err != nil {
			return SweepResult{}, fmt.Errorf("lifecycle sweep: write audit entry: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return SweepResult{}, fmt.Errorf("lifecycle sweep: %w", err)
	}
	return result, nil
}

// Run sweeps on a ticker until ctx is cancelled.
//
// The first sweep happens one tick in rather than immediately. Cancellation is
// then the only way Run can return without having done any work, which keeps
// its contract with a caller shutting the process down unambiguous, and nothing
// is lost by it: the ticker is expected to be far shorter than the thresholds a
// sweep measures.
//
// A cancelled context is how the worker is asked to stop, so it is not
// reported as an error. A failed sweep is: the caller supervising this loop is
// the only thing in a position to decide whether to restart it, and swallowing
// the error here would leave a worker that had silently stopped keeping the
// inventory honest looking exactly like one that was running.
func (w *LifecycleWorker) Run(ctx context.Context, every time.Duration) error {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// A tick and a cancellation that arrive together are chosen
			// between at random, so cancellation is checked explicitly rather
			// than left to that coin flip.
			if ctx.Err() != nil {
				return nil
			}
			if _, err := w.Sweep(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

// thresholds are the three windows a sweep measures, in seconds, because that
// is the unit make_interval takes and the unit that keeps a day a fixed length.
type thresholds struct {
	idleSeconds     float64
	staleSeconds    float64
	livenessSeconds float64
}

func (w *LifecycleWorker) thresholds() thresholds {
	return thresholds{
		idleSeconds:     float64(w.cfg.HeartbeatIntervalSeconds * w.cfg.IdleAfterMissedBeats),
		staleSeconds:    float64(w.cfg.StaleAfterDays * secondsPerDay),
		livenessSeconds: float64(w.cfg.HeartbeatIntervalSeconds * livenessWindowIntervals),
	}
}

// transition is one agent the sweep moved, with the snapshots that explain it.
type transition struct {
	agentID        uuid.UUID
	orgID          uuid.UUID
	afterState     domain.LifecycleState
	beforeSnapshot []byte
	afterSnapshot  []byte
}

// action names the transition for the audit log. The sweep only ever writes
// idle or stale, so there is no third case to report.
func (t transition) action() string {
	if t.afterState == domain.LifecycleStale {
		return actionMarkedStale
	}
	return actionMarkedIdle
}

// sweepAgents runs the one statement that does all the work and sorts what came
// back into transitions to audit and counts to report.
//
// The rows are drained before anything else is executed on the transaction:
// pgx allows only one query in flight per connection, so the audit entries
// cannot be written while this result set is still open.
func sweepAgents(ctx context.Context, tx pgx.Tx, now time.Time, th thresholds) ([]transition, SweepResult, error) {
	rows, err := tx.Query(ctx, sweepSQL, now, th.idleSeconds, th.staleSeconds, th.livenessSeconds)
	if err != nil {
		return nil, SweepResult{}, fmt.Errorf("lifecycle sweep: select silent agents: %w", err)
	}
	defer rows.Close()

	var (
		transitions []transition
		result      SweepResult
	)
	for rows.Next() {
		var (
			t                             transition
			beforeState, afterState       string
			beforeReplicas, afterReplicas int
		)
		if err := rows.Scan(
			&t.agentID, &t.orgID, &beforeState, &afterState,
			&beforeReplicas, &afterReplicas, &t.beforeSnapshot, &t.afterSnapshot,
		); err != nil {
			return nil, SweepResult{}, fmt.Errorf("lifecycle sweep: read swept agent: %w", err)
		}
		t.afterState = domain.LifecycleState(afterState)

		if beforeReplicas != afterReplicas {
			result.ReplicaCountsUpdated++
		}
		if beforeState == afterState {
			// Recomputed replica count, same lifecycle state. Nothing has been
			// inferred about the agent, so there is nothing to audit: an audit
			// entry per silent agent per tick would bury the transitions that
			// matter.
			continue
		}
		if t.afterState == domain.LifecycleStale {
			result.MarkedStale++
		} else {
			result.MarkedIdle++
		}
		transitions = append(transitions, t)
	}
	if err := rows.Err(); err != nil {
		return nil, SweepResult{}, fmt.Errorf("lifecycle sweep: read swept agents: %w", err)
	}
	return transitions, result, nil
}

// sweepSQL moves every silent agent on and recomputes what the server can still
// see of it, in one statement.
//
// $1 is the injected clock, and every threshold is measured against it rather
// than against now(). That is what makes the worker testable without sleeping,
// and it also means one sweep measures every agent against a single instant
// instead of drifting across the fleet as the statement runs.
//
// The candidate set is agents in active or idle only. retired and quarantined
// are human decisions and are not merely left in place but left out entirely:
// their replica_count is not recomputed either, because "never touched" is a
// far easier guarantee to keep than "touched, but only in these columns".
//
// An active agent that has been silent past the stale threshold goes straight
// to stale rather than to idle. The two-step reading would write an
// agent.marked_idle entry recording an idle period the server never observed,
// and would need a second sweep to reach the state it can already prove. This
// arises when a sweep first runs after a long outage of its own, which is
// exactly when the honest answer matters most.
//
// The comparison is strict: an agent silent for exactly the threshold has
// reached it, not passed it.
//
// FOR UPDATE is what makes the before snapshot true. It locks the candidates
// for the rest of the statement, so a heartbeat arriving mid-sweep either lands
// first — in which case PostgreSQL re-checks this WHERE clause against the row
// it wrote, sees a fresh last_heartbeat_at, and drops the agent from the sweep —
// or waits and revives the agent immediately afterwards. Either order is
// correct; without the lock the sweep could mark an agent idle a moment after
// it proved itself alive. ORDER BY id fixes the lock order so two sweeps
// overlapping cannot deadlock.
//
// The final predicate is what makes a repeated sweep free: an agent already
// idle, still not stale, and with the replica count it already had is not
// written at all, so it produces no row here and no audit entry below.
//
// updated_at is deliberately not stamped, for the same reason no revision is
// written: it answers "when did this record last say something different", and
// the record has not said anything.
const sweepSQL = `
WITH candidate AS (
    SELECT a.id,
           a.org_id,
           a.lifecycle_state AS before_state,
           a.replica_count   AS before_replicas,
           to_jsonb(a)       AS before_snapshot,
           CASE WHEN a.last_heartbeat_at < $1::timestamptz - make_interval(secs => $3)
                THEN 'stale' ELSE 'idle' END AS after_state,
           (SELECT count(*)
              FROM agent_credentials c
             WHERE c.agent_id = a.id
               AND c.revoked_at IS NULL
               AND (c.expires_at IS NULL OR c.expires_at > $1::timestamptz)
               AND c.last_seen_at > $1::timestamptz - make_interval(secs => $4)
           ) AS live_replicas
      FROM agents a
     WHERE a.lifecycle_state IN ('active','idle')
       AND a.last_heartbeat_at < $1::timestamptz - make_interval(secs => $2)
     ORDER BY a.id
     FOR UPDATE
)
UPDATE agents a SET
    lifecycle_state = c.after_state,
    replica_count   = c.live_replicas
  FROM candidate c
 WHERE a.id = c.id
   AND (a.lifecycle_state IS DISTINCT FROM c.after_state
        OR a.replica_count IS DISTINCT FROM c.live_replicas)
RETURNING a.id, a.org_id, c.before_state, a.lifecycle_state,
          c.before_replicas, a.replica_count,
          c.before_snapshot, to_jsonb(a);
`

// The audit entry carries the whole row on both sides rather than the one
// column that moved. A reader asking why an agent went stale wants what else
// was true of it at the time — when it was last seen, how many replicas it
// still claimed, who owned it — and a diff of two snapshots answers that
// without this worker having to guess in advance which columns mattered.
const writeAuditSQL = `
INSERT INTO audit_log (org_id, actor, action, target_type, target_id, before, after)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb);
`
