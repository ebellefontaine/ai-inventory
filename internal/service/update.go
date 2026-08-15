package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ebellefontaine/ai-inventory/internal/auth"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
)

// UpdateService applies a partial descriptor to an agent that is already in the
// inventory.
//
// It exists because an agent's world moves without the agent being restarted:
// it is redeployed to another region, its framework is upgraded, its dashboard
// moves. Re-registering to report one of those would issue a fresh credential
// every time, and requiring the agent to resend everything it ever knew would
// make each report an opportunity to lose a field it happens not to know today.
type UpdateService struct{ pool *pgxpool.Pool }

// NewUpdateService wires the service. It takes no heartbeat interval: an update
// is not something an agent does on a timer, so there is no schedule to hand
// back to it.
func NewUpdateService(pool *pgxpool.Pool) *UpdateService { return &UpdateService{pool: pool} }

// UpdateInput is one PATCH: which record is being changed, the per-agent
// credential that authorises it, and the payload exactly as received.
//
// The descriptor stays raw for a reason this operation depends on entirely. A
// decoded struct cannot distinguish a field the agent omitted from a field the
// agent set to the empty string, and that distinction is the whole meaning of a
// partial update. The raw bytes are the only place the difference still exists.
type UpdateInput struct {
	AgentID       uuid.UUID
	AgentToken    string
	RawDescriptor []byte
}

// UpdateResult is what the record now says about itself, and whether this call
// is why.
type UpdateResult struct {
	// Flags describe the record as it now stands, not merely what this payload
	// contained — an update that supplies a missing platform clears
	// missing_platform, and one that clears a purpose raises missing_purpose.
	Flags []string
	// Changed reports whether any stored value actually moved. It is false for
	// an agent that re-sends what the record already held.
	Changed bool
}

// patchColumns maps a payload key to the agents column it writes.
//
// instance_key is deliberately absent. It is one third of the identity that
// makes this inventory converge — (org, environment, instance_key) — and
// renaming it here would either fork the record on the agent's next
// registration or collide with another agent's. An agent that sends it is
// updating the record its own key already identifies, so the key is redundant
// rather than wrong, and it is ignored rather than refused.
//
// team and environment are absent for the same reason they are absent from
// domain.Descriptor: ownership comes from the enrollment token. They reach
// attributes through the unclaimed-key path below, which is what raises
// ignored_scope_fields.
var patchColumns = map[string]string{
	"name":            "display_name",
	"purpose":         "purpose",
	"platform":        "platform",
	"hosting":         "hosting",
	"source":          "source",
	"telemetry_links": "telemetry_links",
}

// recomputedFlags are the flags this service derives from the record's contents
// on every update. Any other stored flag is carried forward untouched, because
// it records something this operation cannot observe — FlagDerivedInstanceKey,
// for instance, is a fact about how the agent registered, and recomputing the
// flag set without preserving it would erase that on the agent's first PATCH.
var recomputedFlags = []string{
	domain.FlagMissingPurpose,
	domain.FlagMissingPlatform,
	domain.FlagMissingHosting,
	domain.FlagMissingSource,
	domain.FlagIgnoredScopeFields,
	domain.FlagUnknownFields,
}

// Update applies the keys the payload actually contains and leaves every other
// column exactly as it was.
//
// The properties worth stating, because each has a test and none is obvious
// from the signature:
//
//   - Absent is not empty. A key the payload does not carry is not written, so
//     a redeploy that reports only a new region cannot wipe the framework,
//     repository, and dashboards the agent did not mention.
//   - A human's correction wins. Any column named in agents.human_edited_fields
//     is skipped, which is the same guarantee registration gets from the guard
//     in internal/db/agents.go.
//   - Silence in the history is meaningful. A revision and an audit entry are
//     written only when a stored value actually moved, so an agent PATCHing
//     identical data on a timer cannot bury the real changes.
//
// Errors from the credential check are the heartbeat sentinels, unwrapped, for
// the same reason: Task 20's HTTP layer must collapse them into one
// indistinguishable 401, and an operator debugging a rollout needs them apart.
func (s *UpdateService) Update(ctx context.Context, in UpdateInput) (UpdateResult, error) {
	// Decoding happens before the transaction opens. A payload that is not a
	// JSON object at all has nothing to apply and nothing to record, and failing
	// here means it cannot have written anything by construction.
	patch, err := decodePatch(in.RawDescriptor)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("update agent: %w", err)
	}

	// One transaction, for the same reason registration uses one: an agent row
	// that moved without the revision and audit entry that explain it is a
	// history with a hole in it precisely where a change happened.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("update agent: %w", err)
	}
	// Rollback after a successful commit is a no-op, so this is safe on every
	// path — including the credential rejections, which is what guarantees a
	// refused update writes nothing at all.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := authenticateAgent(ctx, tx, in.AgentID, in.AgentToken); err != nil {
		return UpdateResult{}, err
	}

	stored, err := readAgentForUpdate(ctx, tx, in.AgentID)
	if err != nil {
		return UpdateResult{}, err
	}

	next, err := patch.applyTo(stored)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("update agent: %w", err)
	}

	var (
		before  []byte
		after   []byte
		changed bool
	)
	err = tx.QueryRow(ctx, updateAgentSQL, in.AgentID,
		next.displayName, next.purpose,
		next.platform, next.hosting, next.source, next.telemetry, next.attributes, next.flags,
	).Scan(&before, &after, &changed)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("update agent: apply descriptor: %w", err)
	}

	if !changed {
		// The agent is alive and said nothing new. last_heartbeat_at has moved
		// and updated_at has not, which is the distinction that keeps "recently
		// changed" answerable.
		if err := tx.Commit(ctx); err != nil {
			return UpdateResult{}, fmt.Errorf("update agent: %w", err)
		}
		return UpdateResult{Flags: next.flags}, nil
	}

	// updated_at is stamped only now, because whether it should move is decided
	// by the diff and the diff needs the write. Writing it in the statement above
	// would need a second copy of the comparison rule, in a different form, in
	// the same statement — two places for one rule to drift apart.
	if err := tx.QueryRow(ctx, touchAgentUpdatedAtSQL, in.AgentID).Scan(&after); err != nil {
		return UpdateResult{}, fmt.Errorf("update agent: stamp updated_at: %w", err)
	}

	// The actor is the agent itself, named by the identity it reports under —
	// the same form registration uses, so one agent's revisions read as one
	// agent's history regardless of which call produced them.
	actor := "agent:" + stored.instanceKey

	if _, err := tx.Exec(ctx, appendRevisionSQL, in.AgentID, after, actor); err != nil {
		return UpdateResult{}, fmt.Errorf("update agent: append revision: %w", err)
	}

	if _, err := tx.Exec(ctx, writeAuditSQL,
		stored.orgID, actor, actionAgentUpdated, targetTypeAgent, in.AgentID.String(), before, after,
	); err != nil {
		return UpdateResult{}, fmt.Errorf("update agent: write audit entry: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return UpdateResult{}, fmt.Errorf("update agent: %w", err)
	}

	return UpdateResult{Flags: next.flags, Changed: true}, nil
}

// patch is a descriptor payload plus the one thing the decoded struct cannot
// carry: which columns the payload actually spoke about.
type patch struct {
	descriptor domain.Descriptor
	// columns holds agents column names, so it can be compared directly against
	// agents.human_edited_fields without a second translation table.
	columns map[string]bool
}

// decodePatch works out which columns the payload addresses.
//
// The presence set comes from a raw key map rather than from the decoded
// Descriptor, and that is the crux of the whole operation. domain.Descriptor is
// a struct: a payload that omits "purpose" and a payload that sets it to ""
// produce byte-identical structs. Only the raw JSON still knows the difference,
// so the difference is read there, before anything is decoded.
//
// A key that is present but did not decode into the field it names — "platform":
// "oops" — does not count as addressing its column. domain.DecodeDescriptor has
// already routed it to Attributes as evidence, where it raises unknown_fields,
// and treating it as a value would let a malformed section erase a good one.
// That is record-and-flag applied to an update: the odd input is kept and made
// visible, and nothing is destroyed on the strength of it.
func decodePatch(raw []byte) (patch, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return patch{}, fmt.Errorf("decode descriptor: %w", err)
	}

	descriptor, err := domain.DecodeDescriptor(raw)
	if err != nil {
		return patch{}, err
	}

	p := patch{descriptor: descriptor, columns: make(map[string]bool, len(keys))}
	for key := range keys {
		column, claimed := patchColumns[key]
		if !claimed {
			continue
		}
		if _, unclaimed := descriptor.Attributes[key]; unclaimed {
			continue
		}
		p.columns[column] = true
	}

	// The one hard rejection, and it is domain.ErrNameRequired's rather than a
	// new one: a record with no name cannot be shown to a human or reconciled
	// against anything later. Clearing every other field is the agent's business;
	// clearing the name would leave a row nobody can identify.
	if p.columns["display_name"] && strings.TrimSpace(descriptor.Name) == "" {
		return patch{}, domain.ErrNameRequired
	}

	// Unknown keys are evidence and always land somewhere, so any payload that
	// carries one addresses the attributes column.
	if len(descriptor.Attributes) > 0 {
		p.columns["attributes"] = true
	}

	return p, nil
}

// storedAgent is the agent as it stands before the update: the columns a patch
// can move, the ones a human has claimed, and the identity the history is
// attributed to.
type storedAgent struct {
	orgID       uuid.UUID
	instanceKey string
	humanEdited []string
	flags       []string

	displayName string
	purpose     string
	platform    []byte
	hosting     []byte
	source      []byte
	telemetry   []byte
	attributes  []byte
}

// nextAgent is the full set of values to write. Every column is written on every
// update, including the ones nothing changed, because the row is read under a
// lock a statement earlier: writing back an identical value is a no-op the diff
// sees straight through, and it keeps one code path instead of a conditional
// SET list assembled at runtime.
type nextAgent struct {
	displayName string
	purpose     string
	platform    []byte
	hosting     []byte
	source      []byte
	telemetry   []byte
	attributes  []byte
	flags       []string
}

// applyTo merges the patch into the stored row.
//
// The human-edit guard is applied here rather than repeated as a CASE in the
// UPDATE, which is a departure from registration worth naming. Registration
// upserts blind — it has no prior row in hand — so its guard has to live in the
// statement. This path has already read the row under FOR UPDATE, and it must
// know the effective value of every column anyway in order to derive the flags
// and to decide whether anything changed. Deriving those from one set of values
// and writing a different set is how a guarantee quietly becomes untrue, so
// there is exactly one merge and everything downstream reads from it.
func (p patch) applyTo(stored storedAgent) (nextAgent, error) {
	writes := func(column string) bool {
		return p.columns[column] && !slices.Contains(stored.humanEdited, column)
	}

	next := nextAgent{
		displayName: stored.displayName,
		purpose:     stored.purpose,
		platform:    stored.platform,
		hosting:     stored.hosting,
		source:      stored.source,
		telemetry:   stored.telemetry,
		attributes:  stored.attributes,
	}

	if writes("display_name") {
		next.displayName = strings.TrimSpace(p.descriptor.Name)
	}
	if writes("purpose") {
		next.purpose = p.descriptor.Purpose
	}

	var err error
	if writes("platform") {
		if next.platform, err = marshalSection(platformDocument(p.descriptor.Platform), emptyObject); err != nil {
			return nextAgent{}, fmt.Errorf("encode platform: %w", err)
		}
	}
	if writes("hosting") {
		if next.hosting, err = marshalSection(hostingDocument(p.descriptor.Hosting), emptyObject); err != nil {
			return nextAgent{}, fmt.Errorf("encode hosting: %w", err)
		}
	}
	if writes("source") {
		if next.source, err = marshalSection(sourceDocument(p.descriptor.Source), emptyObject); err != nil {
			return nextAgent{}, fmt.Errorf("encode source: %w", err)
		}
	}
	if writes("telemetry_links") {
		links := make([]telemetryLinkDocument, len(p.descriptor.TelemetryLinks))
		for i, link := range p.descriptor.TelemetryLinks {
			links[i] = telemetryLinkDocument(link)
		}
		if next.telemetry, err = marshalSection(links, emptyArray); err != nil {
			return nextAgent{}, fmt.Errorf("encode telemetry links: %w", err)
		}
	}
	if writes("attributes") {
		// Merged, not replaced. Attributes are the record of everything the
		// server was told and would not honour, and a partial update that
		// replaced the map wholesale would erase the evidence of a scope claim
		// made at registration — the exact thing an auditor came to look for.
		if next.attributes, err = mergeAttributes(stored.attributes, p.descriptor.Attributes); err != nil {
			return nextAgent{}, fmt.Errorf("encode attributes: %w", err)
		}
	}

	if next.flags, err = next.deriveFlags(stored.flags); err != nil {
		return nextAgent{}, err
	}
	return next, nil
}

// deriveFlags describes the record as it will stand, not the payload that
// produced it.
//
// The flags are recomputed rather than merged in, because they answer "what is
// wrong with this record now" and a stale missing_platform on an agent that has
// since reported one is worse than no flag at all. It runs through
// domain.Descriptor.Validate on the merged values so that a record reached by
// updating and the same record reached by re-registering carry identical flags —
// there is one definition of what counts as missing, and it is not this file's.
func (n nextAgent) deriveFlags(storedFlags []string) ([]string, error) {
	var (
		platform platformDocument
		hosting  hostingDocument
		source   sourceDocument
		links    []telemetryLinkDocument
	)
	if err := json.Unmarshal(n.platform, &platform); err != nil {
		return nil, fmt.Errorf("decode stored platform: %w", err)
	}
	if err := json.Unmarshal(n.hosting, &hosting); err != nil {
		return nil, fmt.Errorf("decode stored hosting: %w", err)
	}
	if err := json.Unmarshal(n.source, &source); err != nil {
		return nil, fmt.Errorf("decode stored source: %w", err)
	}
	if err := json.Unmarshal(n.telemetry, &links); err != nil {
		return nil, fmt.Errorf("decode stored telemetry links: %w", err)
	}
	var attributes map[string]json.RawMessage
	if err := json.Unmarshal(n.attributes, &attributes); err != nil {
		return nil, fmt.Errorf("decode stored attributes: %w", err)
	}

	descriptor := domain.Descriptor{
		Name:           n.displayName,
		Purpose:        n.purpose,
		Platform:       domain.Platform(platform),
		Hosting:        domain.Hosting(hosting),
		Source:         domain.Source(source),
		TelemetryLinks: make([]domain.TelemetryLink, len(links)),
		Attributes:     attributes,
	}
	for i, link := range links {
		descriptor.TelemetryLinks[i] = domain.TelemetryLink(link)
	}

	flags, err := descriptor.Validate()
	if err != nil {
		return nil, fmt.Errorf("validate updated descriptor: %w", err)
	}

	for _, flag := range storedFlags {
		if !slices.Contains(recomputedFlags, flag) {
			flags = append(flags, flag)
		}
	}
	slices.Sort(flags)
	return slices.Compact(flags), nil
}

// The empty values a jsonb column must hold rather than a JSON null. See
// marshalSection.
var (
	emptyObject = []byte(`{}`)
	emptyArray  = []byte(`[]`)
)

// marshalSection encodes a descriptor section, substituting empty for the JSON
// literal null.
//
// This is the same trap encodeSection guards in register.go, sprung from the
// other side. There the fix is a nil []byte, which binds as SQL NULL and lets
// COALESCE do its work. Here the columns are written unconditionally with no
// COALESCE to fall back on, so a nil slice marshalling to "null" would land a
// jsonb null in a NOT NULL column — perfectly valid to the database, and a value
// every reader then has to special-case.
func marshalSection(v any, empty []byte) ([]byte, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(encoded, []byte("null")) {
		return slices.Clone(empty), nil
	}
	return encoded, nil
}

// mergeAttributes overlays the payload's unclaimed keys onto the stored ones.
func mergeAttributes(stored []byte, incoming map[string]json.RawMessage) ([]byte, error) {
	merged := map[string]json.RawMessage{}
	if err := json.Unmarshal(stored, &merged); err != nil {
		return nil, fmt.Errorf("decode stored attributes: %w", err)
	}
	if merged == nil {
		merged = map[string]json.RawMessage{}
	}
	for key, value := range incoming {
		merged[key] = value
	}
	return json.Marshal(merged)
}

// authenticateAgent repeats the check HeartbeatService makes, deliberately.
//
// The two operations have to agree on what a valid credential is, and the
// sentinels are shared so a caller cannot tell them apart by which error it got.
// Extracting it into a helper the heartbeat also called would be the better
// shape; that is a change to heartbeat.go, and it belongs in a commit that says
// so rather than smuggled into this one.
func authenticateAgent(ctx context.Context, tx pgx.Tx, agentID uuid.UUID, token string) error {
	var (
		credentialID uuid.UUID
		ownerID      uuid.UUID
		expiresAt    *time.Time
		revokedAt    *time.Time
	)
	err := tx.QueryRow(ctx, resolveCredentialSQL, auth.HashToken(token)).Scan(
		&credentialID, &ownerID, &expiresAt, &revokedAt,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrCredentialNotFound
	case err != nil:
		return fmt.Errorf("update agent: resolve credential: %w", err)
	}

	// Revocation ahead of expiry, and both ahead of the ownership mismatch, in
	// the same order the heartbeat reports them: an operator who deliberately
	// killed a credential wants to see that rather than the incidental fact that
	// it also aged out.
	switch {
	case revokedAt != nil:
		return ErrCredentialRevoked
	case expiresAt != nil && !expiresAt.After(time.Now()):
		return ErrCredentialExpired
	case ownerID != agentID:
		return ErrCredentialAgentMismatch
	}
	return nil
}

func readAgentForUpdate(ctx context.Context, tx pgx.Tx, agentID uuid.UUID) (storedAgent, error) {
	var a storedAgent
	err := tx.QueryRow(ctx, readAgentForUpdateSQL, agentID).Scan(
		&a.orgID, &a.instanceKey, &a.humanEdited, &a.flags,
		&a.displayName, &a.purpose,
		&a.platform, &a.hosting, &a.source, &a.telemetry, &a.attributes,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Only a concurrent hard delete of the agent can get here: the
		// credential's foreign key guaranteed the row existed a statement ago,
		// and deleting it cascades to the credential.
		return storedAgent{}, fmt.Errorf("update agent: agent %s no longer exists", agentID)
	case err != nil:
		return storedAgent{}, fmt.Errorf("update agent: read agent: %w", err)
	}
	return a, nil
}

// FOR UPDATE holds the row for the rest of the transaction, which is what makes
// the read-merge-write below safe. Without it, a registration landing in between
// would have its values silently overwritten by values computed from the row as
// it was beforehand — including the human_edited_fields the merge consults.
//
// The jsonb columns are cast to text so they arrive as the bytes PostgreSQL
// itself would render, which is what makes writing an untouched column back a
// genuine no-op rather than a reformatting.
const readAgentForUpdateSQL = `
SELECT org_id, instance_key, human_edited_fields, flags,
       display_name, purpose,
       platform::text, hosting::text, source::text, telemetry_links::text, attributes::text
  FROM agents
 WHERE id = $1
 FOR UPDATE;
`

// updateAgentSQL writes the merged values and reports what moved.
//
// The comparison is made by the database, on the row as it was and as it now
// is, rather than in Go on the values that were sent. That matters: jsonb
// equality is semantic, so a payload that reorders the keys of a section or
// spells a number differently is correctly seen as no change, where a byte
// comparison of the encoded forms would call it one and append a revision
// nobody can tell apart from its predecessor.
//
// last_heartbeat_at and lifecycle_state are excluded from that comparison
// because they are what the server observed, not what the record says. An
// update is proof of life on both counts — it stamps the heartbeat and revives
// an agent the server had written off as idle or stale — but neither is a change
// to the descriptor, and a revision per PATCH for an agent whose only news is
// that it is still running is the noise this comparison exists to prevent.
//
// retired and quarantined are left exactly alone. They are human decisions: an
// agent that keeps reporting after being retired is precisely what an operator
// needs to see, and a quarantined agent must not be able to release itself by
// continuing to run. The CASE mirrors the one in registerAgentSQL and
// recordHeartbeatSQL, for the same reasons.
//
// The prior snapshot is read in the same statement as the update, so it is the
// state this write replaced and not merely the state at some earlier moment.
const updateAgentSQL = `
WITH prior AS (
    SELECT to_jsonb(a) AS snapshot
      FROM agents a
     WHERE a.id = $1
), updated AS (
    UPDATE agents SET
        display_name      = $2,
        purpose           = $3,
        platform          = $4::jsonb,
        hosting           = $5::jsonb,
        source            = $6::jsonb,
        telemetry_links   = $7::jsonb,
        attributes        = $8::jsonb,
        flags             = COALESCE($9::text[],'{}'),
        last_heartbeat_at = now(),
        lifecycle_state   = CASE
            WHEN agents.lifecycle_state IN ('idle','stale') THEN 'active'
            ELSE agents.lifecycle_state
        END
     WHERE agents.id = $1
    RETURNING to_jsonb(agents) AS snapshot
)
SELECT prior.snapshot,
       updated.snapshot,
       (prior.snapshot   - '{last_heartbeat_at,lifecycle_state}'::text[])
         IS DISTINCT FROM
       (updated.snapshot - '{last_heartbeat_at,lifecycle_state}'::text[]) AS changed
  FROM prior, updated;
`

// updated_at answers "when did this record last say something different", which
// is only worth asking if an unchanged update leaves it alone. The snapshot is
// re-read so the revision and the audit entry record the row including this
// stamp, rather than a state that never existed.
const touchAgentUpdatedAtSQL = `
UPDATE agents SET updated_at = now() WHERE id = $1 RETURNING to_jsonb(agents);
`
