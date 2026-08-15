package service_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/auth"
	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

// testHeartbeatInterval is deliberately not the production default, so a
// service that returns a hard-coded 60 fails these tests.
const testHeartbeatInterval = 45

// registrationFixture is one org, one team, one enrollment token, and the two
// services under test — everything a registration needs and nothing else.
type registrationFixture struct {
	pool  *pgxpool.Pool
	svc   *service.RegistrationService
	token *service.TokenService

	orgID  uuid.UUID
	teamID uuid.UUID

	// enrollment is the plaintext token an agent presents, scoped to teamID
	// and production.
	enrollment string
	tokenID    uuid.UUID
}

func newRegistrationFixture(t *testing.T, slug string) registrationFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t)
	tokens := service.NewTokenService(pool)
	org, team := seedOrgAndTeam(t, pool, slug)

	minted, err := tokens.Mint(ctx, service.MintInput{
		OrgID:       org,
		TeamID:      team,
		Environment: domain.EnvironmentProduction,
		Name:        slug + " enrollment",
		CreatedBy:   "admin@example.com",
	})
	require.NoError(t, err)

	return registrationFixture{
		pool:       pool,
		svc:        service.NewRegistrationService(pool, tokens, testHeartbeatInterval),
		token:      tokens,
		orgID:      org,
		teamID:     team,
		enrollment: minted.Plaintext,
		tokenID:    minted.ID,
	}
}

// descriptorJSON renders a descriptor payload the way an agent would send it:
// as raw JSON, so a test can put keys in it that the Descriptor type refuses to
// carry — which is exactly what the ownership test needs.
func descriptorJSON(t *testing.T, fields map[string]any) []byte {
	t.Helper()

	raw, err := json.Marshal(fields)
	require.NoError(t, err)
	return raw
}

// minimalDescriptor is a well-formed descriptor: enough to register, sparse
// enough that the missing-section flags are raised.
func minimalDescriptor(t *testing.T, instanceKey string) []byte {
	t.Helper()

	return descriptorJSON(t, map[string]any{
		"name":         "Invoice triage",
		"purpose":      "Triages accounts payable invoices",
		"instance_key": instanceKey,
	})
}

// storedAgent is one registered agent as the database holds it. attributesType
// comes from jsonb_typeof so a JSON null can be told apart from an object.
type storedAgent struct {
	id             uuid.UUID
	teamID         *uuid.UUID
	environment    string
	instanceKey    string
	displayName    string
	purpose        string
	platform       string
	attributes     string
	attributesType string
	flags          []string
	lifecycleState string
	source         string
}

func readStoredAgent(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) storedAgent {
	t.Helper()

	var a storedAgent
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT id, team_id, environment, instance_key, display_name, purpose,
		       platform::text, attributes::text, jsonb_typeof(attributes),
		       flags, lifecycle_state, registration_source
		  FROM agents WHERE id = $1`, id,
	).Scan(
		&a.id, &a.teamID, &a.environment, &a.instanceKey, &a.displayName, &a.purpose,
		&a.platform, &a.attributes, &a.attributesType,
		&a.flags, &a.lifecycleState, &a.source,
	))
	return a
}

// countRows counts a whole table. The table name is a constant supplied by the
// test, never by input, so interpolating it is safe here.
func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()

	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n))
	return n
}

// requireNothingWritten asserts that a rejected registration left no trace in
// any of the four tables a successful one touches.
func requireNothingWritten(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	require.Equal(t, 0, countRows(t, pool, "agents"), "no agent may be written")
	require.Equal(t, 0, countRows(t, pool, "agent_credentials"), "no credential may be issued")
	require.Equal(t, 0, countRows(t, pool, "agent_revisions"), "no revision may be written")
	require.Equal(t, 0, countRows(t, pool, "audit_log"), "no audit entry may be written")
}

// credentialHashes returns every stored credential hash for an agent.
func credentialHashes(t *testing.T, pool *pgxpool.Pool, agentID uuid.UUID) []string {
	t.Helper()

	rows, err := pool.Query(context.Background(),
		`SELECT token_hash FROM agent_credentials WHERE agent_id = $1 ORDER BY issued_at, id`, agentID)
	require.NoError(t, err)
	defer rows.Close()

	var hashes []string
	for rows.Next() {
		var h string
		require.NoError(t, rows.Scan(&h))
		hashes = append(hashes, h)
	}
	require.NoError(t, rows.Err())
	return hashes
}

// revisionSnapshots returns an agent's revision snapshots, oldest first.
func revisionSnapshots(t *testing.T, pool *pgxpool.Pool, agentID uuid.UUID) []string {
	t.Helper()

	rows, err := pool.Query(context.Background(),
		`SELECT snapshot::text FROM agent_revisions WHERE agent_id = $1 ORDER BY id`, agentID)
	require.NoError(t, err)
	defer rows.Close()

	var snapshots []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		snapshots = append(snapshots, s)
	}
	require.NoError(t, rows.Err())
	return snapshots
}

// revisionActors returns the actor recorded on each of an agent's revisions.
func revisionActors(t *testing.T, pool *pgxpool.Pool, agentID uuid.UUID) []string {
	t.Helper()

	rows, err := pool.Query(context.Background(),
		`SELECT actor FROM agent_revisions WHERE agent_id = $1 ORDER BY id`, agentID)
	require.NoError(t, err)
	defer rows.Close()

	var actors []string
	for rows.Next() {
		var a string
		require.NoError(t, rows.Scan(&a))
		actors = append(actors, a)
	}
	require.NoError(t, rows.Err())
	return actors
}

// auditEntry is one audit_log row.
type auditEntry struct {
	orgID      uuid.UUID
	actor      string
	action     string
	targetType string
	targetID   string
	before     *string
	after      *string
}

func auditEntries(t *testing.T, pool *pgxpool.Pool) []auditEntry {
	t.Helper()

	rows, err := pool.Query(context.Background(),
		`SELECT org_id, actor, action, target_type, target_id, before::text, after::text
		   FROM audit_log ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	var entries []auditEntry
	for rows.Next() {
		var e auditEntry
		require.NoError(t, rows.Scan(
			&e.orgID, &e.actor, &e.action, &e.targetType, &e.targetID, &e.before, &e.after))
		entries = append(entries, e)
	}
	require.NoError(t, rows.Err())
	return entries
}

// TestRegisterTakesOwnershipFromTheTokenNotTheDescriptor is the product's audit
// story in executable form.
//
// The agent below does everything it can to claim a different owner and a
// different environment: it sends "team", "team_id", "env", and "environment",
// all pointing somewhere other than the token's scope. Every one of them must be
// ignored, and the attempt must be visible afterwards as a flag.
//
// If this test ever passes trivially — because the descriptor's claims happen to
// match the token's scope — it proves nothing, so the fixture deliberately
// registers a second team and a second environment for the descriptor to lie
// with.
func TestRegisterTakesOwnershipFromTheTokenNotTheDescriptor(t *testing.T) {
	fx := newRegistrationFixture(t, "ownership")
	ctx := context.Background()

	// A team that really exists but is not the token's team, so a service that
	// honoured the descriptor would produce a row that looks perfectly valid.
	var otherTeam uuid.UUID
	require.NoError(t, fx.pool.QueryRow(ctx,
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, 'Finance', 'finance') RETURNING id`,
		fx.orgID).Scan(&otherTeam))
	require.NotEqual(t, fx.teamID, otherTeam)

	res, err := fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor: descriptorJSON(t, map[string]any{
			"name":         "Invoice triage",
			"purpose":      "Triages accounts payable invoices",
			"instance_key": "invoice-triage",
			"team":         "finance",
			"team_id":      otherTeam.String(),
			"env":          "development",
			"environment":  "development",
		}),
	})
	require.NoError(t, err)

	stored := readStoredAgent(t, fx.pool, res.AgentID)

	require.NotNil(t, stored.teamID)
	require.Equal(t, fx.teamID, *stored.teamID,
		"the owning team comes from the enrollment token, never from the descriptor")
	require.NotEqual(t, otherTeam, *stored.teamID,
		"the descriptor's claimed team must not be honoured")
	require.Equal(t, string(domain.EnvironmentProduction), stored.environment,
		"the environment comes from the enrollment token, never from the descriptor")

	require.Contains(t, res.Flags, domain.FlagIgnoredScopeFields,
		"an agent that tried to declare its own ownership must be flagged")
	require.Contains(t, stored.flags, domain.FlagIgnoredScopeFields,
		"the flag must be persisted, not just returned")

	// The rejected claims are kept verbatim as evidence of the attempt. An
	// auditor asking "did this agent try to reassign itself?" needs to be able
	// to answer yes from the record.
	for _, key := range []string{"team", "team_id", "env", "environment"} {
		require.Contains(t, stored.attributes, key,
			"the ignored claim %q must be preserved as evidence", key)
	}
	require.Contains(t, stored.attributes, otherTeam.String())
}

func TestRegisterIsIdempotent(t *testing.T) {
	fx := newRegistrationFixture(t, "idempotent")
	ctx := context.Background()

	in := service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   minimalDescriptor(t, "invoice-triage"),
	}

	first, err := fx.svc.Register(ctx, in)
	require.NoError(t, err)
	require.True(t, first.Created, "the first registration creates the record")

	second, err := fx.svc.Register(ctx, in)
	require.NoError(t, err)
	require.False(t, second.Created, "the second registration updates the same record")

	require.Equal(t, first.AgentID, second.AgentID)
	require.Equal(t, 1, countRows(t, fx.pool, "agents"),
		"a restarting agent must not multiply into two inventory records")
}

// Twenty replicas of one deployment are one agent holding twenty credentials.
// Reusing a credential across replicas would make revocation all-or-nothing.
func TestRegisterIssuesOneCredentialPerRegistration(t *testing.T) {
	fx := newRegistrationFixture(t, "per-replica-credentials")
	ctx := context.Background()

	in := service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   minimalDescriptor(t, "invoice-triage"),
	}

	first, err := fx.svc.Register(ctx, in)
	require.NoError(t, err)
	second, err := fx.svc.Register(ctx, in)
	require.NoError(t, err)

	require.Equal(t, first.AgentID, second.AgentID)
	require.NotEqual(t, first.AgentToken, second.AgentToken,
		"each replica must receive its own credential")

	hashes := credentialHashes(t, fx.pool, first.AgentID)
	require.Len(t, hashes, 2, "one agent, two credentials")
	require.NotEqual(t, hashes[0], hashes[1])

	require.True(t, auth.VerifyToken(first.AgentToken, hashes[0]))
	require.True(t, auth.VerifyToken(second.AgentToken, hashes[1]))
	require.False(t, auth.VerifyToken(first.AgentToken, hashes[1]),
		"a replica's credential must not verify against another replica's hash")
}

func TestRegisterReturnsACredentialThatIsNeverStored(t *testing.T) {
	fx := newRegistrationFixture(t, "credential-not-stored")
	ctx := context.Background()

	res, err := fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   minimalDescriptor(t, "invoice-triage"),
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(res.AgentToken, "aiinv_ac_"),
		"the per-agent credential must carry its own prefix, got %q", res.AgentToken)
	require.Equal(t, testHeartbeatInterval, res.HeartbeatIntervalSeconds)

	hashes := credentialHashes(t, fx.pool, res.AgentID)
	require.Len(t, hashes, 1)
	require.True(t, auth.VerifyToken(res.AgentToken, hashes[0]),
		"the returned credential must verify against the stored hash")

	// Render every column of every credential row, so a column added later is
	// covered by this assertion for free.
	var doc string
	require.NoError(t, fx.pool.QueryRow(ctx,
		`SELECT coalesce(jsonb_agg(to_jsonb(c))::text, '[]') FROM agent_credentials c`).Scan(&doc))
	require.NotContains(t, doc, res.AgentToken, "no column may hold the plaintext credential")

	secret := strings.TrimPrefix(res.AgentToken, "aiinv_ac_")
	require.NotEmpty(t, secret)
	require.NotContains(t, doc, secret, "no column may hold the credential secret")
}

func TestRegisterWithRevokedTokenWritesNothing(t *testing.T) {
	fx := newRegistrationFixture(t, "revoked")
	ctx := context.Background()

	require.NoError(t, fx.token.Revoke(ctx, fx.tokenID))

	_, err := fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   minimalDescriptor(t, "invoice-triage"),
	})
	require.ErrorIs(t, err, service.ErrTokenRevoked)
	requireNothingWritten(t, fx.pool)
}

// The three token failures stay distinguishable at this layer. Collapsing them
// into one indistinguishable 401 is the HTTP layer's job; doing it here would
// destroy the diagnostic an operator needs.
func TestRegisterPropagatesTokenFailuresUnchanged(t *testing.T) {
	fx := newRegistrationFixture(t, "token-failures")
	ctx := context.Background()

	_, err := fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: "aiinv_et_neverminted",
		RawDescriptor:   minimalDescriptor(t, "invoice-triage"),
	})
	require.ErrorIs(t, err, service.ErrTokenNotFound)
	requireNothingWritten(t, fx.pool)
}

func TestRegisterPersistsValidationFlags(t *testing.T) {
	fx := newRegistrationFixture(t, "flags")
	ctx := context.Background()

	res, err := fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor: descriptorJSON(t, map[string]any{
			"name":         "Invoice triage",
			"instance_key": "invoice-triage",
			"unexpected":   "value",
		}),
	})
	require.NoError(t, err)

	stored := readStoredAgent(t, fx.pool, res.AgentID)
	for _, flag := range []string{
		domain.FlagMissingPurpose,
		domain.FlagMissingPlatform,
		domain.FlagMissingHosting,
		domain.FlagMissingSource,
		domain.FlagUnknownFields,
	} {
		require.Containsf(t, res.Flags, flag, "returned flags must include %q", flag)
		require.Containsf(t, stored.flags, flag, "stored flags must include %q", flag)
	}
	require.Equal(t, res.Flags, stored.flags, "returned and stored flags must agree")
}

func TestRegisterWritesARevisionEachTime(t *testing.T) {
	fx := newRegistrationFixture(t, "revisions")
	ctx := context.Background()

	first, err := fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   minimalDescriptor(t, "invoice-triage"),
	})
	require.NoError(t, err)

	snapshots := revisionSnapshots(t, fx.pool, first.AgentID)
	require.Len(t, snapshots, 1, "registering writes a revision")
	require.Contains(t, snapshots[0], "Invoice triage")

	_, err = fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor: descriptorJSON(t, map[string]any{
			"name":         "Invoice triage (renamed)",
			"purpose":      "Triages accounts payable invoices and credit notes",
			"instance_key": "invoice-triage",
		}),
	})
	require.NoError(t, err)

	snapshots = revisionSnapshots(t, fx.pool, first.AgentID)
	require.Len(t, snapshots, 2, "re-registering with changed values writes a second revision")
	require.Contains(t, snapshots[1], "Invoice triage (renamed)")
	require.NotEqual(t, snapshots[0], snapshots[1],
		"the two revisions must record different states")

	// The revision must say who changed it, or it cannot be attributed.
	for _, actor := range revisionActors(t, fx.pool, first.AgentID) {
		require.Equal(t, "agent:invoice-triage", actor)
	}
}

func TestRegisterWritesAnAuditEntry(t *testing.T) {
	fx := newRegistrationFixture(t, "audit")
	ctx := context.Background()

	in := service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   minimalDescriptor(t, "invoice-triage"),
	}

	res, err := fx.svc.Register(ctx, in)
	require.NoError(t, err)

	entries := auditEntries(t, fx.pool)
	require.Len(t, entries, 1)
	require.Equal(t, fx.orgID, entries[0].orgID)
	require.Equal(t, "agent.registered", entries[0].action)
	require.Equal(t, "agent", entries[0].targetType)
	require.Equal(t, res.AgentID.String(), entries[0].targetID)
	require.Equal(t, "agent:invoice-triage", entries[0].actor)
	require.Nil(t, entries[0].before, "a newly created agent has no prior state")
	require.NotNil(t, entries[0].after)

	_, err = fx.svc.Register(ctx, in)
	require.NoError(t, err)

	entries = auditEntries(t, fx.pool)
	require.Len(t, entries, 2)
	require.Equal(t, "agent.updated", entries[1].action,
		"a second registration updates rather than creates")
	require.NotNil(t, entries[1].before, "an update records the state it replaced")
}

// A nil Attributes map marshals to the JSON literal null, which the upsert's
// COALESCE does not catch — it guards against SQL NULL, not JSON null. A jsonb
// null in a NOT NULL column type-checks fine and then breaks every reader that
// expects an object.
func TestRegisterStoresEmptyAttributesAsAnObject(t *testing.T) {
	fx := newRegistrationFixture(t, "empty-attributes")
	ctx := context.Background()

	res, err := fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   minimalDescriptor(t, "invoice-triage"),
	})
	require.NoError(t, err)

	stored := readStoredAgent(t, fx.pool, res.AgentID)
	require.Equal(t, "object", stored.attributesType,
		"attributes must be a JSON object, not a JSON null")
	require.Equal(t, "{}", stored.attributes)

	// The same trap applies to every jsonb column an absent descriptor section
	// leaves empty.
	require.Equal(t, "{}", stored.platform)

	var telemetryType string
	require.NoError(t, fx.pool.QueryRow(ctx,
		`SELECT jsonb_typeof(telemetry_links) FROM agents WHERE id = $1`, res.AgentID).Scan(&telemetryType))
	require.Equal(t, "array", telemetryType)
}

func TestRegisterRejectsDescriptorWithoutName(t *testing.T) {
	fx := newRegistrationFixture(t, "no-name")
	ctx := context.Background()

	_, err := fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor: descriptorJSON(t, map[string]any{
			"purpose":      "Triages accounts payable invoices",
			"instance_key": "invoice-triage",
		}),
	})
	require.ErrorIs(t, err, domain.ErrNameRequired)
	requireNothingWritten(t, fx.pool)
}

func TestRegisterRejectsUnparseableDescriptor(t *testing.T) {
	fx := newRegistrationFixture(t, "unparseable")
	ctx := context.Background()

	_, err := fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   []byte(`not json at all`),
	})
	require.Error(t, err)
	requireNothingWritten(t, fx.pool)
}

// Identity depends on instance_key, but record-and-flag says a missing one is
// not grounds for rejection. The derived key must be deterministic, or an
// unnamed-key agent restarting would multiply into a new record every time.
func TestRegisterDerivesAMissingInstanceKey(t *testing.T) {
	fx := newRegistrationFixture(t, "derived-key")
	ctx := context.Background()

	in := service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor: descriptorJSON(t, map[string]any{
			"name":    "Invoice triage",
			"purpose": "Triages accounts payable invoices",
		}),
	}

	first, err := fx.svc.Register(ctx, in)
	require.NoError(t, err)
	require.True(t, first.Created)
	require.Contains(t, first.Flags, service.FlagDerivedInstanceKey,
		"a synthesised identity must be visible as a flag")

	stored := readStoredAgent(t, fx.pool, first.AgentID)
	require.NotEmpty(t, stored.instanceKey)
	require.Contains(t, stored.flags, service.FlagDerivedInstanceKey)

	second, err := fx.svc.Register(ctx, in)
	require.NoError(t, err)
	require.False(t, second.Created, "the derived key must be deterministic")
	require.Equal(t, first.AgentID, second.AgentID)
	require.Equal(t, 1, countRows(t, fx.pool, "agents"),
		"an agent with no instance_key must converge to one record, not multiply")

	// A different agent name under the same scope is a different agent, so the
	// derived keys must not collide.
	other, err := fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor: descriptorJSON(t, map[string]any{
			"name":    "Expense triage",
			"purpose": "Triages expense claims",
		}),
	})
	require.NoError(t, err)
	require.True(t, other.Created)
	require.NotEqual(t, first.AgentID, other.AgentID)
}

// An administrator's correction to the owning team must survive the agent's
// next self-report. The guard lives in the upsert statement; this test is here
// because the registration service issues that statement on its own
// transaction, and a divergence between the two copies would silently undo
// OWN-4.
func TestRegisterPreservesHumanEditedFields(t *testing.T) {
	fx := newRegistrationFixture(t, "human-edited")
	ctx := context.Background()

	in := service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   minimalDescriptor(t, "invoice-triage"),
	}

	first, err := fx.svc.Register(ctx, in)
	require.NoError(t, err)

	var correctedTeam uuid.UUID
	require.NoError(t, fx.pool.QueryRow(ctx,
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, 'Finance', 'finance') RETURNING id`,
		fx.orgID).Scan(&correctedTeam))

	_, err = fx.pool.Exec(ctx, `
		UPDATE agents
		   SET team_id = $2, display_name = 'AP invoice triage',
		       human_edited_fields = '{team_id,display_name}'
		 WHERE id = $1`, first.AgentID, correctedTeam)
	require.NoError(t, err)

	_, err = fx.svc.Register(ctx, in)
	require.NoError(t, err)

	stored := readStoredAgent(t, fx.pool, first.AgentID)
	require.NotNil(t, stored.teamID)
	require.Equal(t, correctedTeam, *stored.teamID,
		"a human's ownership correction must not be reverted by self-registration")
	require.Equal(t, "AP invoice triage", stored.displayName)
}

// Steps five through eight are one transaction. An agent row that exists
// without a credential can never heartbeat, so it would go idle and then stale
// while running perfectly — an inventory entry that lies.
//
// The failure is forced at the database rather than through an injected
// dependency, so the code under test is exactly the production path: a CHECK
// constraint that no credential row can satisfy makes step six fail after step
// five has already written the agent.
func TestRegisterIsAtomic(t *testing.T) {
	fx := newRegistrationFixture(t, "atomic")
	ctx := context.Background()

	_, err := fx.pool.Exec(ctx,
		`ALTER TABLE agent_credentials ADD CONSTRAINT force_registration_failure CHECK (false)`)
	require.NoError(t, err)

	_, err = fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   minimalDescriptor(t, "invoice-triage"),
	})
	require.Error(t, err, "a registration that cannot issue a credential must fail")
	requireNothingWritten(t, fx.pool)

	// And once the failure is removed the same registration succeeds, which
	// proves the assertions above failed for the reason claimed rather than
	// because registration was broken outright.
	_, err = fx.pool.Exec(ctx,
		`ALTER TABLE agent_credentials DROP CONSTRAINT force_registration_failure`)
	require.NoError(t, err)

	res, err := fx.svc.Register(ctx, service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   minimalDescriptor(t, "invoice-triage"),
	})
	require.NoError(t, err)
	require.True(t, res.Created, "the rolled back attempt must not have left a record behind")
}
