package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ebellefontaine/ai-inventory/internal/auth"
	"github.com/ebellefontaine/ai-inventory/internal/db"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
)

// agentCredentialPrefix makes a leaked per-agent credential identifiable on
// sight, and distinguishable from an enrollment token, which is a far more
// valuable secret: one enrollment token registers any number of agents.
const agentCredentialPrefix = "aiinv_ac"

// FlagDerivedInstanceKey marks an agent that supplied no instance_key, so the
// server synthesised one for it.
//
// It lives here rather than in internal/domain because domain.Descriptor's
// validation cannot raise it: deriving a key needs the enrollment token's
// scope, which the descriptor deliberately knows nothing about.
//
// The flag matters because a derived key is a weaker identity than a declared
// one. Two deployments of the same agent under one team and environment are
// indistinguishable to it and will be merged into a single record. That is the
// right default — it beats multiplying one agent into a new row per restart —
// but it is a fact about the record that an operator must be able to see.
const FlagDerivedInstanceKey = "derived_instance_key"

// Audit actions written by registration. The vocabulary is
// "<target_type>.<past-tense verb>", so an audit reader can filter on a prefix.
const (
	actionAgentRegistered = "agent.registered"
	actionAgentUpdated    = "agent.updated"
	targetTypeAgent       = "agent"
)

// RegistrationService turns an enrollment token plus a self-reported
// descriptor into an inventory record.
type RegistrationService struct {
	pool                     *pgxpool.Pool
	tokens                   *TokenService
	heartbeatIntervalSeconds int
}

// NewRegistrationService wires the service. heartbeatIntervalSeconds is what
// registering agents are told to use; it is server policy rather than the
// agent's choice, so that an operator can change the inventory's sensitivity to
// silence without redeploying every agent.
func NewRegistrationService(pool *pgxpool.Pool, tokens *TokenService, heartbeatIntervalSeconds int) *RegistrationService {
	return &RegistrationService{
		pool:                     pool,
		tokens:                   tokens,
		heartbeatIntervalSeconds: heartbeatIntervalSeconds,
	}
}

// RegisterInput is one registration attempt: the credential that authorises it
// and the payload exactly as received.
//
// The descriptor stays raw this far in on purpose. Decoding is part of the
// operation — the record of what the agent actually sent, including the keys
// this system refuses to honour, is evidence, and a pre-decoded struct would
// have thrown it away before the service ever saw it.
type RegisterInput struct {
	EnrollmentToken string
	RawDescriptor   []byte
}

// RegisterResult is what the agent needs in order to keep reporting, plus an
// honest account of what was wrong with what it sent.
type RegisterResult struct {
	AgentID uuid.UUID
	// AgentToken is plaintext and is returned exactly once. Only its hash is
	// stored, so it cannot be recovered or re-shown.
	AgentToken               string
	HeartbeatIntervalSeconds int
	Flags                    []string
	Created                  bool
}

// Register records an agent and issues it a credential.
//
// The one rule that makes this an inventory rather than a list of claims: the
// owning team and the environment come from the enrollment token's scope, and
// nothing the descriptor says about ownership is honoured. A descriptor that
// tries is recorded and flagged rather than rejected, because the attempt is
// itself worth seeing.
//
// Errors from the token layer are returned unchanged. Task 20's HTTP layer
// collapses ErrTokenNotFound, ErrTokenRevoked, and ErrTokenExpired into one
// indistinguishable 401; doing that here would destroy the distinction an
// operator needs while debugging a rollout.
func (s *RegistrationService) Register(ctx context.Context, in RegisterInput) (RegisterResult, error) {
	scope, err := s.tokens.Resolve(ctx, in.EnrollmentToken)
	if err != nil {
		return RegisterResult{}, err
	}

	// A payload that is not a JSON object at all is the one parse failure that
	// is fatal: there is nothing to record and nothing to flag.
	descriptor, err := domain.DecodeDescriptor(in.RawDescriptor)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("register agent: %w", err)
	}

	// Everything except a missing name is recorded and flagged. An inventory
	// that turns sparse input away develops blind spots, and a blind spot is
	// the failure this product exists to eliminate.
	flags, err := descriptor.Validate()
	if err != nil {
		return RegisterResult{}, fmt.Errorf("register agent: %w", err)
	}

	instanceKey := strings.TrimSpace(descriptor.InstanceKey)
	if instanceKey == "" {
		instanceKey = derivedInstanceKey(scope, descriptor.Name)
		flags = append(flags, FlagDerivedInstanceKey)
		slices.Sort(flags)
	}

	agent, err := upsertInput(scope, descriptor, instanceKey, flags)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("register agent: %w", err)
	}

	// Generated before the transaction opens: a failure of the system random
	// source must not leave a transaction hanging, and there is nothing to roll
	// back if it happens here.
	credential, credentialHash, err := auth.GenerateToken(agentCredentialPrefix)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("register agent: %w", err)
	}

	// Steps five through eight are one transaction, and the reason is specific:
	// an agent row written without a credential describes an agent that can
	// never heartbeat. It would go idle, then stale, while running perfectly —
	// an inventory entry that lies, which is worse than no entry at all.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("register agent: %w", err)
	}
	// Rollback after a successful commit is a no-op, so this is safe on every
	// path. Its error is discarded because the operation's outcome is already
	// decided by then.
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := readAgentSnapshot(ctx, tx, scope, agent.Environment, instanceKey)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("register agent: %w", err)
	}

	// The upsert is the store's, issued on this service's transaction rather
	// than on the pool. The human-edit guard that makes OWN-4 true lives in that
	// one statement, so self-registration cannot drift away from it.
	upserted, err := db.UpsertTx(ctx, tx, agent)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("register agent: upsert: %w", err)
	}
	agentID, created, after := upserted.ID, upserted.Created, upserted.Snapshot

	if _, err := tx.Exec(ctx, issueCredentialSQL, agentID, credentialHash); err != nil {
		return RegisterResult{}, fmt.Errorf("register agent: issue credential: %w", err)
	}

	// The actor is the agent itself, named by the identity it registered under.
	// A revision whose author cannot be named is not attributable, and an audit
	// trail that is not attributable is decoration.
	actor := "agent:" + instanceKey

	if _, err := tx.Exec(ctx, appendRevisionSQL, agentID, after, actor); err != nil {
		return RegisterResult{}, fmt.Errorf("register agent: append revision: %w", err)
	}

	action := actionAgentUpdated
	if created {
		action = actionAgentRegistered
	}
	if _, err := tx.Exec(ctx, writeAuditSQL,
		scope.OrgID, actor, action, targetTypeAgent, agentID.String(), before, after,
	); err != nil {
		return RegisterResult{}, fmt.Errorf("register agent: write audit entry: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RegisterResult{}, fmt.Errorf("register agent: %w", err)
	}

	return RegisterResult{
		AgentID:                  agentID,
		AgentToken:               credential,
		HeartbeatIntervalSeconds: s.heartbeatIntervalSeconds,
		Flags:                    flags,
		Created:                  created,
	}, nil
}

// derivedInstanceKey synthesises an identity for an agent that supplied none.
//
// Identity is (org, environment, instance_key), so something has to fill that
// column. A random value would be the easy answer and the wrong one: the agent
// would register a brand new record on every restart and one agent would become
// a hundred inventory entries within a day.
//
// So the key is a hash of the things that are stable across that agent's
// restarts and that we can trust: the scope the token pins (organisation, team,
// environment) and the name the agent calls itself. Two processes that agree on
// all four are treated as the same agent and converge on one record. Two that
// differ in any of them stay separate.
//
// The name is trimmed and lower-cased first, so a whitespace or capitalisation
// change in a deployment manifest does not fork the record.
//
// The hash covers the org id as well as the team id even though a team already
// belongs to exactly one org. It costs nothing and means the derivation cannot
// be quietly weakened by a future change to how teams are scoped.
//
// Eight bytes of SHA-256 is short enough to read in a UI and wide enough that
// an accidental collision inside one team and environment is not a practical
// concern. A deliberate collision is not a threat here: the inputs are already
// fixed by the token's scope, so an attacker who could choose them could simply
// send the instance_key they wanted in the first place.
func derivedInstanceKey(scope Scope, name string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "aiinv-derived-instance-key\x00%s\x00%s\x00%s\x00%s",
		scope.OrgID, scope.TeamID, scope.Environment,
		strings.ToLower(strings.TrimSpace(name)),
	))
	return "derived-" + hex.EncodeToString(sum[:8])
}

// upsertInput assembles the row to write. It reuses db.UpsertAgentInput rather
// than declaring a private twin, so the column set this package writes stays
// tied to the one the storage layer defines.
//
// Team and environment are read from scope and the descriptor is never
// consulted for them — the type does not even carry them, which is the point.
func upsertInput(scope Scope, d domain.Descriptor, instanceKey string, flags []string) (db.UpsertAgentInput, error) {
	platform, err := encodeSection(platformDocument(d.Platform), d.Platform == (domain.Platform{}))
	if err != nil {
		return db.UpsertAgentInput{}, fmt.Errorf("encode platform: %w", err)
	}
	hosting, err := encodeSection(hostingDocument(d.Hosting), d.Hosting == (domain.Hosting{}))
	if err != nil {
		return db.UpsertAgentInput{}, fmt.Errorf("encode hosting: %w", err)
	}
	source, err := encodeSection(sourceDocument(d.Source), d.Source == (domain.Source{}))
	if err != nil {
		return db.UpsertAgentInput{}, fmt.Errorf("encode source: %w", err)
	}

	links := make([]telemetryLinkDocument, len(d.TelemetryLinks))
	for i, link := range d.TelemetryLinks {
		links[i] = telemetryLinkDocument(link)
	}
	telemetry, err := encodeSection(links, len(links) == 0)
	if err != nil {
		return db.UpsertAgentInput{}, fmt.Errorf("encode telemetry links: %w", err)
	}

	attributes, err := encodeSection(d.Attributes, len(d.Attributes) == 0)
	if err != nil {
		return db.UpsertAgentInput{}, fmt.Errorf("encode attributes: %w", err)
	}

	return db.UpsertAgentInput{
		OrgID:       scope.OrgID,
		TeamID:      scope.TeamID,
		InstanceKey: instanceKey,
		Environment: string(scope.Environment),
		DisplayName: strings.TrimSpace(d.Name),
		Purpose:     d.Purpose,
		Platform:    platform,
		Hosting:     hosting,
		Source:      source,
		Telemetry:   telemetry,
		Attributes:  attributes,
		Flags:       flags,
	}, nil
}

// encodeSection marshals a descriptor section, returning a nil []byte when the
// section is empty.
//
// The nil matters more than it looks. json.Marshal of a nil map or nil slice
// produces the JSON literal "null", and the upsert's COALESCE($n,'{}') defends
// against SQL NULL, not against a JSON null: "null"::jsonb is a perfectly valid
// jsonb value and lands in a NOT NULL column without complaint. The column then
// holds a value every reader has to special-case, and jsonb_typeof reports
// 'null' rather than 'object'. A nil []byte binds as SQL NULL, which is what
// COALESCE is actually watching for.
func encodeSection(v any, empty bool) ([]byte, error) {
	if empty {
		return nil, nil
	}
	return json.Marshal(v)
}

// The document types carry the snake_case JSON tags the stored columns use.
// They convert field-for-field from the domain types, which the compiler
// enforces: adding a field on one side without the other stops the build.
//
// omitempty on every field means an absent sub-field is absent from the stored
// document rather than stored as an empty string, so a reader can tell "the
// agent did not say" from "the agent said nothing".

type platformDocument struct {
	Framework        string `json:"framework,omitempty"`
	FrameworkVersion string `json:"framework_version,omitempty"`
}

type hostingDocument struct {
	Provider   string `json:"provider,omitempty"`
	Region     string `json:"region,omitempty"`
	AccountID  string `json:"account_id,omitempty"`
	ResourceID string `json:"resource_id,omitempty"`
}

type sourceDocument struct {
	RepoURL   string `json:"repo_url,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
}

type telemetryLinkDocument struct {
	Kind       string `json:"kind,omitempty"`
	Label      string `json:"label,omitempty"`
	URL        string `json:"url,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
}

// readAgentSnapshot returns the agent's state before this registration, or nil
// when there is none. It is the "before" half of the audit entry: an audit row
// that records only the new value cannot answer what a change actually changed.
func readAgentSnapshot(ctx context.Context, tx pgx.Tx, scope Scope, environment, instanceKey string) ([]byte, error) {
	var snapshot []byte
	err := tx.QueryRow(ctx, readAgentSnapshotSQL, scope.OrgID, environment, instanceKey).Scan(&snapshot)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read prior agent state: %w", err)
	}
	return snapshot, nil
}

const readAgentSnapshotSQL = `
SELECT to_jsonb(a)
FROM agents a
WHERE a.org_id = $1 AND a.environment = $2 AND a.instance_key = $3;
`

// A new row per registration, never a reused one. Twenty replicas of one
// deployment are one agent holding twenty credentials, so that revoking a
// compromised replica's credential does not silently unregister the other
// nineteen.
const issueCredentialSQL = `
INSERT INTO agent_credentials (agent_id, token_hash)
VALUES ($1, $2);
`

const appendRevisionSQL = `
INSERT INTO agent_revisions (agent_id, snapshot, actor)
VALUES ($1, $2::jsonb, $3);
`

const writeAuditSQL = `
INSERT INTO audit_log (org_id, actor, action, target_type, target_id, before, after)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb);
`
