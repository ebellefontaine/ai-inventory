package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ebellefontaine/ai-inventory/internal/domain"
)

// ErrAgentNotVisible means the credential presented is genuinely valid, but
// its scope does not include the agent asked for: an enrollment token from a
// different team, or a per-agent credential belonging to a different agent.
//
// It is part of AuthSentinels rather than a distinct kind of failure. A
// caller who could tell "wrong scope" apart from "credential doesn't exist"
// would learn something about a credential they were guessing at, which is
// exactly what the rest of that set exists to prevent.
var ErrAgentNotVisible = errors.New("agent not visible to this credential")

// ErrInvalidLifecycleState means a search filter named something that is not
// one of domain's five lifecycle states. Unlike the credential errors, this
// is an ordinary client mistake — it carries no information about anything
// secret — so it is reported plainly rather than folded into AuthSentinels.
var ErrInvalidLifecycleState = errors.New("invalid lifecycle state filter")

func init() {
	AuthSentinels = append(AuthSentinels, ErrAgentNotVisible)
}

const (
	defaultSearchLimit = 50
	maxSearchLimit     = 200
)

// Agent is the read model returned by GetAgent and SearchAgents: everything
// about a record a caller with a valid credential is allowed to see.
//
// It is a type of its own rather than db.UpsertAgentInput or a raw row scan,
// because a read model and a write model answer different questions — this
// one exists to be handed to an MCP client or, later, rendered in the web UI,
// and its shape should be free to diverge from storage without disturbing
// either.
type Agent struct {
	ID                 uuid.UUID
	TeamSlug           string
	Environment        string
	DisplayName        string
	Purpose            string
	LifecycleState     string
	RegistrationSource string
	ReplicaCount       int
	Platform           json.RawMessage
	Hosting            json.RawMessage
	Source             json.RawMessage
	TelemetryLinks     json.RawMessage
	Flags              []string
	FirstSeenAt        time.Time
	LastHeartbeatAt    time.Time
}

// QueryService answers read-only questions about agents already in the
// inventory. It never writes: no revision, no audit entry, no lifecycle
// transition — those all belong to the services that actually change a
// record.
type QueryService struct {
	pool   *pgxpool.Pool
	tokens *TokenService
}

func NewQueryService(pool *pgxpool.Pool, tokens *TokenService) *QueryService {
	return &QueryService{pool: pool, tokens: tokens}
}

// GetAgentInput is one lookup: which agent, and the credential that has to
// justify seeing it.
type GetAgentInput struct {
	AgentID    uuid.UUID
	Credential string
}

// GetAgent resolves Credential as an enrollment token first — the common
// case for an operator or an agent checking on a teammate — and falls back
// to treating it as the agent's own per-agent credential only if that fails.
// Two lookups on the unauthorized path cost nothing a caller can observe: the
// response is the same ErrAgentNotVisible-or-credential-error either way.
func (s *QueryService) GetAgent(ctx context.Context, in GetAgentInput) (Agent, error) {
	agent, err := s.readAgent(ctx, in.AgentID)
	if err != nil {
		return Agent{}, err
	}

	if scope, tokenErr := s.tokens.Resolve(ctx, in.Credential); tokenErr == nil {
		if agent.orgID != scope.OrgID || agent.teamID != scope.TeamID || agent.Environment != string(scope.Environment) {
			return Agent{}, ErrAgentNotVisible
		}
		return agent.Agent, nil
	}

	if _, err := authenticateAgent(ctx, s.pool, in.AgentID, in.Credential, "get agent"); err != nil {
		return Agent{}, err
	}
	return agent.Agent, nil
}

// SearchAgentsInput is one search: the enrollment token whose scope bounds
// the results, an optional lifecycle filter, and how many rows to return.
type SearchAgentsInput struct {
	EnrollmentToken string
	// LifecycleState, if non-empty, must be one of the five states domain
	// defines. Empty means no filter.
	LifecycleState string
	// Limit caps the result set. Zero or negative means defaultSearchLimit;
	// anything above maxSearchLimit is clamped to it.
	Limit int
}

// SearchAgents lists agents within the enrollment token's own scope, most
// recently heartbeated first.
func (s *QueryService) SearchAgents(ctx context.Context, in SearchAgentsInput) ([]Agent, error) {
	scope, err := s.tokens.Resolve(ctx, in.EnrollmentToken)
	if err != nil {
		return nil, err
	}

	if in.LifecycleState != "" && !domain.LifecycleState(in.LifecycleState).Valid() {
		return nil, fmt.Errorf("search agents: %w: %q", ErrInvalidLifecycleState, in.LifecycleState)
	}

	limit := in.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	rows, err := s.pool.Query(ctx, searchAgentsSQL, scope.OrgID, scope.TeamID, string(scope.Environment), in.LifecycleState, limit)
	if err != nil {
		return nil, fmt.Errorf("search agents: %w", err)
	}
	defer rows.Close()

	var results []Agent
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("search agents: %w", err)
		}
		results = append(results, agent.Agent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search agents: %w", err)
	}
	return results, nil
}

// agentWithScope pairs the public read model with the two columns GetAgent
// needs for its own scope check but that Agent has no reason to expose.
type agentWithScope struct {
	Agent
	orgID  uuid.UUID
	teamID uuid.UUID
}

func (s *QueryService) readAgent(ctx context.Context, agentID uuid.UUID) (agentWithScope, error) {
	row := s.pool.QueryRow(ctx, getAgentSQL, agentID)
	agent, err := scanAgent(row)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Deliberately the credential-not-found sentinel, not a distinct
		// "agent not found" one: a caller who has not yet proven a credential
		// is valid must not be able to tell a missing agent apart from an
		// agent it simply cannot see. Both eventually surface as whichever
		// authentication step below fails.
		return agentWithScope{}, ErrCredentialNotFound
	case err != nil:
		return agentWithScope{}, fmt.Errorf("get agent: %w", err)
	}
	return agent, nil
}

// row is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query), which is
// what lets GetAgent and SearchAgents share one Scan call and one column
// list instead of keeping two in sync by hand.
type row interface {
	Scan(dest ...any) error
}

func scanAgent(r row) (agentWithScope, error) {
	var a agentWithScope
	err := r.Scan(
		&a.ID, &a.orgID, &a.teamID, &a.TeamSlug, &a.Environment,
		&a.DisplayName, &a.Purpose, &a.LifecycleState, &a.RegistrationSource, &a.ReplicaCount,
		&a.Platform, &a.Hosting, &a.Source, &a.TelemetryLinks, &a.Flags,
		&a.FirstSeenAt, &a.LastHeartbeatAt,
	)
	return a, err
}

const agentColumnsSQL = `
	a.id, a.org_id, a.team_id, t.slug, a.environment,
	a.display_name, a.purpose, a.lifecycle_state, a.registration_source, a.replica_count,
	a.platform, a.hosting, a.source, a.telemetry_links, a.flags,
	a.first_seen_at, a.last_heartbeat_at`

const getAgentSQL = `
SELECT ` + agentColumnsSQL + `
  FROM agents a JOIN teams t ON t.id = a.team_id
 WHERE a.id = $1;
`

// The empty-string lifecycle filter matches every row via the OR clause
// rather than requiring a second query shape, since "no filter" and "filter
// on an empty column value" can never both be true for a NOT NULL column
// with a CHECK constraint on the state vocabulary.
const searchAgentsSQL = `
SELECT ` + agentColumnsSQL + `
  FROM agents a JOIN teams t ON t.id = a.team_id
 WHERE a.org_id = $1 AND a.team_id = $2 AND a.environment = $3
   AND ($4 = '' OR a.lifecycle_state = $4)
 ORDER BY a.last_heartbeat_at DESC
 LIMIT $5;
`
