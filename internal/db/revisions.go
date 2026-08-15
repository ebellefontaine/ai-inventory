package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Revision is one recorded shape of an agent at a moment in time.
//
// There is no Update and no Delete. The database enforces that with a trigger,
// so the absence of those methods is a convenience, not the guarantee.
type Revision struct {
	ID        int64
	AgentID   uuid.UUID
	Snapshot  []byte // json
	Actor     string
	ChangedAt time.Time
}

type RevisionStore struct{ pool *pgxpool.Pool }

func NewRevisionStore(pool *pgxpool.Pool) *RevisionStore { return &RevisionStore{pool: pool} }

const appendRevisionSQL = `
INSERT INTO agent_revisions (agent_id, snapshot, actor)
VALUES ($1, $2::jsonb, $3);
`

// Append records a snapshot of an agent. Capturing history is nearly free at
// write time and impossible to reconstruct afterwards, so we always capture it.
func (s *RevisionStore) Append(ctx context.Context, agentID uuid.UUID, snapshot []byte, actor string) error {
	if _, err := s.pool.Exec(ctx, appendRevisionSQL, agentID, snapshot, actor); err != nil {
		return fmt.Errorf("append revision: %w", err)
	}
	return nil
}

// changed_at ties break on id so revisions written inside one transaction —
// which share a now() — still come back in the order they were written.
const listRevisionsSQL = `
SELECT id, agent_id, snapshot, actor, changed_at
FROM agent_revisions
WHERE agent_id = $1
ORDER BY changed_at DESC, id DESC
LIMIT $2;
`

// List returns an agent's revisions newest first, at most limit of them.
func (s *RevisionStore) List(ctx context.Context, agentID uuid.UUID, limit int) ([]Revision, error) {
	rows, err := s.pool.Query(ctx, listRevisionsSQL, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("list revisions: %w", err)
	}
	defer rows.Close()

	revisions, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Revision])
	if err != nil {
		return nil, fmt.Errorf("list revisions: %w", err)
	}
	return revisions, nil
}
