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
    team_id         = EXCLUDED.team_id,
    display_name    = EXCLUDED.display_name,
    purpose         = EXCLUDED.purpose,
    platform        = EXCLUDED.platform,
    hosting         = EXCLUDED.hosting,
    source          = EXCLUDED.source,
    telemetry_links = EXCLUDED.telemetry_links,
    attributes      = EXCLUDED.attributes,
    flags           = EXCLUDED.flags,
    lifecycle_state = CASE
        WHEN agents.lifecycle_state IN ('idle','stale') THEN 'active'
        ELSE agents.lifecycle_state
    END,
    last_heartbeat_at = now(),
    updated_at        = now()
RETURNING id, (xmax = 0) AS created;
`

// Upsert registers or updates an agent. It is idempotent on
// (org_id, environment, instance_key) and safe under concurrency.
//
// The (xmax = 0) expression is a PostgreSQL idiom for "this row was inserted
// rather than updated by this statement" — xmax is zero on a fresh insert.
//
// Note the lifecycle CASE: re-registering revives an idle or stale agent, but
// never resurrects one a human deliberately retired or quarantined.
func (s *AgentStore) Upsert(ctx context.Context, in UpsertAgentInput) (UpsertAgentResult, error) {
	var out UpsertAgentResult
	err := s.pool.QueryRow(ctx, upsertAgentSQL,
		in.OrgID, in.TeamID, in.InstanceKey, in.Environment,
		in.DisplayName, in.Purpose,
		in.Platform, in.Hosting, in.Source, in.Telemetry, in.Attributes, in.Flags,
	).Scan(&out.ID, &out.Created)
	if err != nil {
		return UpsertAgentResult{}, fmt.Errorf("upsert agent: %w", err)
	}
	return out, nil
}
