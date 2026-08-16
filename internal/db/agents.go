package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type UpsertAgentInput struct {
	OrgID       uuid.UUID
	TeamID      uuid.UUID
	InstanceKey string
	Environment string
	DisplayName string
	Purpose     string
	Platform    []byte // json
	Hosting     []byte // json
	Source      []byte // json
	Telemetry   []byte // json
	Attributes  []byte // json
	Flags       []string
}

type UpsertAgentResult struct {
	ID      uuid.UUID
	Created bool
	// Snapshot is the stored row as JSON, exactly as the database holds it
	// after the statement. It is what a revision and an audit entry's "after"
	// are built from: taking it from the row rather than from the values Go
	// sent means it reflects the server-managed columns and any human-edited
	// field the guard preserved, so a reconstruction from revisions is faithful
	// rather than aspirational.
	Snapshot []byte
}

type AgentStore struct{ pool *pgxpool.Pool }

func NewAgentStore(pool *pgxpool.Pool) *AgentStore { return &AgentStore{pool: pool} }

const upsertAgentSQL = `
INSERT INTO agents (
    org_id, team_id, instance_key, environment,
    display_name, purpose,
    platform, hosting, source, telemetry_links, attributes, flags
) VALUES (
    $1, $2, $3, $4, $5, $6,
    COALESCE($7,'{}')::jsonb, COALESCE($8,'{}')::jsonb, COALESCE($9,'{}')::jsonb,
    COALESCE($10,'[]')::jsonb, COALESCE($11,'{}')::jsonb,
    COALESCE($12::text[],'{}')
)
ON CONFLICT (org_id, environment, instance_key) DO UPDATE SET
    team_id = CASE WHEN 'team_id' = ANY(agents.human_edited_fields)
                   THEN agents.team_id ELSE EXCLUDED.team_id END,
    display_name = CASE WHEN 'display_name' = ANY(agents.human_edited_fields)
                        THEN agents.display_name ELSE EXCLUDED.display_name END,
    purpose = CASE WHEN 'purpose' = ANY(agents.human_edited_fields)
                   THEN agents.purpose ELSE EXCLUDED.purpose END,
    platform = CASE WHEN 'platform' = ANY(agents.human_edited_fields)
                    THEN agents.platform ELSE EXCLUDED.platform END,
    hosting = CASE WHEN 'hosting' = ANY(agents.human_edited_fields)
                   THEN agents.hosting ELSE EXCLUDED.hosting END,
    source = CASE WHEN 'source' = ANY(agents.human_edited_fields)
                  THEN agents.source ELSE EXCLUDED.source END,
    telemetry_links = CASE WHEN 'telemetry_links' = ANY(agents.human_edited_fields)
                           THEN agents.telemetry_links ELSE EXCLUDED.telemetry_links END,
    attributes = CASE WHEN 'attributes' = ANY(agents.human_edited_fields)
                      THEN agents.attributes ELSE EXCLUDED.attributes END,
    flags           = EXCLUDED.flags,
    lifecycle_state = CASE
        WHEN agents.lifecycle_state IN ('idle','stale') THEN 'active'
        ELSE agents.lifecycle_state
    END,
    last_heartbeat_at = now(),
    updated_at        = now()
RETURNING id, (xmax = 0) AS created, to_jsonb(agents) AS snapshot;
`

// Upsert registers or updates an agent on the store's pool.
//
// It is the standalone path, for a caller with nothing else to write: one
// statement is already atomic on its own. A caller that must write other rows
// alongside the agent — a credential, a revision, an audit entry — calls
// UpsertTx with its own transaction instead. Both issue upsertAgentSQL, which
// is what keeps the human-edit guard below true of every path into this table.
func (s *AgentStore) Upsert(ctx context.Context, in UpsertAgentInput) (UpsertAgentResult, error) {
	out, err := UpsertTx(ctx, s.pool, in)
	if err != nil {
		return UpsertAgentResult{}, fmt.Errorf("upsert agent: %w", err)
	}
	return out, nil
}

// UpsertTx registers or updates an agent on whichever executor the caller
// supplies. It is idempotent on (org_id, environment, instance_key) and safe
// under concurrency.
//
// The (xmax = 0) expression is a PostgreSQL idiom for "this row was inserted
// rather than updated by this statement" — xmax is zero on a fresh insert.
//
// Note the lifecycle CASE: re-registering revives an idle or stale agent, but
// never resurrects one a human deliberately retired or quarantined.
//
// Every self-reported column is wrapped in a guard that keeps the stored value
// when the column's name appears in agents.human_edited_fields. That is OWN-4:
// once an administrator corrects a field — most importantly the owning team —
// the agent's next self-report must not silently revert it. The guard lives
// here, in the statement itself, rather than in a service: a check one layer up
// would be bypassed by the next caller to write this table, and the guarantee
// has to hold for all of them.
//
// The guard covers exactly the eight columns an agent reports about itself:
// team_id, display_name, purpose, platform, hosting, source, telemetry_links,
// attributes. It deliberately does not cover last_heartbeat_at, updated_at,
// flags, or the lifecycle CASE. Those are server-managed — the server derives
// them from the act of registering rather than copying them out of the payload,
// so there is nothing for a human edit to defend them against, and freezing
// them would stop an agent's own heartbeat from being recorded.
//
// Releasing a column back to auto-management is just removing its name from the
// array. The API for doing that arrives with the admin API in Milestone 5.
//
// Flags binds through COALESCE($12::text[],'{}') because a nil []string binds
// as SQL NULL, which a NOT NULL column rejects.
//
// The error is returned unwrapped. Each caller is a different operation to the
// operator reading the log — a standalone upsert, a registration — and wraps it
// with the name of the one it is.
func UpsertTx(ctx context.Context, tx DBTX, in UpsertAgentInput) (UpsertAgentResult, error) {
	var out UpsertAgentResult
	err := tx.QueryRow(ctx, upsertAgentSQL,
		in.OrgID, in.TeamID, in.InstanceKey, in.Environment,
		in.DisplayName, in.Purpose,
		in.Platform, in.Hosting, in.Source, in.Telemetry, in.Attributes, in.Flags,
	).Scan(&out.ID, &out.Created, &out.Snapshot)
	if err != nil {
		return UpsertAgentResult{}, err
	}
	return out, nil
}
