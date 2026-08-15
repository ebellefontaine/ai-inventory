package service_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

// updateFixture is a registration fixture plus the service under test. Every
// credential these tests present is issued by a real registration, so they
// exercise the actual contract between the two services rather than a
// hand-inserted row that happens to look like one.
type updateFixture struct {
	registrationFixture
	upd *service.UpdateService
}

func newUpdateFixture(t *testing.T, slug string) updateFixture {
	t.Helper()

	fx := newRegistrationFixture(t, slug)
	return updateFixture{
		registrationFixture: fx,
		upd:                 service.NewUpdateService(fx.pool),
	}
}

// fullDescriptorFields is a descriptor with every section populated. A partial
// update test is only worth anything if there is something in every column for
// the update to leave alone.
func fullDescriptorFields(instanceKey string) map[string]any {
	return map[string]any{
		"name":         "Invoice triage",
		"purpose":      "Triages accounts payable invoices",
		"instance_key": instanceKey,
		"platform": map[string]any{
			"framework":         "langgraph",
			"framework_version": "0.2.1",
		},
		"hosting": map[string]any{
			"provider":    "aws",
			"region":      "eu-west-1",
			"account_id":  "123456789012",
			"resource_id": "arn:aws:lambda:eu-west-1:123456789012:function:invoice-triage",
		},
		"source": map[string]any{
			"repo_url":   "https://github.com/acme/invoice-triage",
			"commit_sha": "9f2c1ab",
		},
		"telemetry_links": []any{
			map[string]any{
				"kind":        "dashboard",
				"label":       "Grafana",
				"url":         "https://grafana.example.com/d/invoice",
				"external_id": "invoice",
			},
		},
	}
}

func (fx updateFixture) registerFull(t *testing.T, instanceKey string) service.RegisterResult {
	t.Helper()

	res, err := fx.svc.Register(context.Background(), service.RegisterInput{
		EnrollmentToken: fx.enrollment,
		RawDescriptor:   descriptorJSON(t, fullDescriptorFields(instanceKey)),
	})
	require.NoError(t, err)
	return res
}

func (fx updateFixture) patch(t *testing.T, agent service.RegisterResult, fields map[string]any) service.UpdateResult {
	t.Helper()

	res, err := fx.upd.Update(context.Background(), service.UpdateInput{
		AgentID:       agent.AgentID,
		AgentToken:    agent.AgentToken,
		RawDescriptor: descriptorJSON(t, fields),
	})
	require.NoError(t, err)
	return res
}

// agentSnapshot renders every column of one agent, so a test can assert on
// columns it never had to name. A column added to the table later is covered by
// these assertions for free.
func agentSnapshot(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) map[string]json.RawMessage {
	t.Helper()

	var raw []byte
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT to_jsonb(a) FROM agents a WHERE a.id = $1`, id).Scan(&raw))

	var snapshot map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	require.NotEmpty(t, snapshot)
	return snapshot
}

// requireOnlyChanged asserts that exactly the named columns differ between two
// snapshots. It is the assertion that catches a partial update quietly resetting
// a field it was never told about: the column is not listed, so its silent
// change fails the test by name.
func requireOnlyChanged(t *testing.T, before, after map[string]json.RawMessage, allowed ...string) {
	t.Helper()

	require.Equal(t, len(before), len(after), "the column set must not change")

	var changed []string
	for column, was := range before {
		now, ok := after[column]
		require.Truef(t, ok, "column %q vanished from the row", column)
		if string(was) != string(now) {
			changed = append(changed, column)
		}
	}

	slices.Sort(changed)
	want := slices.Clone(allowed)
	slices.Sort(want)
	require.Equal(t, want, changed, "exactly these columns may differ")
}

func markHumanEdited(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, columns ...string) {
	t.Helper()

	tag, err := pool.Exec(context.Background(),
		`UPDATE agents SET human_edited_fields = $2 WHERE id = $1`, id, columns)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
}

// auditEntriesFor narrows the audit log to one agent, so these tests can share
// one database across subtests without counting somebody else's rows.
func auditEntriesFor(t *testing.T, pool *pgxpool.Pool, agentID uuid.UUID) []auditEntry {
	t.Helper()

	rows, err := pool.Query(context.Background(),
		`SELECT org_id, actor, action, target_type, target_id, before::text, after::text
		   FROM audit_log WHERE target_id = $1 ORDER BY id`, agentID.String())
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

// The central promise of PATCH: a key the payload does not mention is not
// touched.
//
// A struct cannot distinguish "absent" from "empty string", so an implementation
// that decodes the payload into domain.Descriptor and writes every field would
// pass every other test in this file while silently wiping platform, hosting,
// source, and telemetry on every partial update. The first subtest below is the
// one that catches it: it asserts on the whole row, so a column reset to its
// zero value fails by name even though no test ever mentions it.
//
// The second subtest is the necessary contrast. Presence is what counts, not
// truthiness — an agent that explicitly sends "" or null is telling the server
// something and must be obeyed.
func TestUpdateAppliesOnlyTheKeysThePayloadContains(t *testing.T) {
	fx := newUpdateFixture(t, "update-partial")

	t.Run("an absent key leaves its column alone", func(t *testing.T) {
		agent := fx.registerFull(t, "invoice-triage")
		before := agentSnapshot(t, fx.pool, agent.AgentID)

		res := fx.patch(t, agent, map[string]any{
			"purpose": "Triages accounts payable invoices and credit notes",
		})
		require.True(t, res.Changed)

		after := agentSnapshot(t, fx.pool, agent.AgentID)
		requireOnlyChanged(t, before, after, "purpose", "last_heartbeat_at", "updated_at")

		// Named explicitly as well, so the failure message says which promise
		// broke rather than only which column moved.
		for _, column := range []string{
			"display_name", "platform", "hosting", "source", "telemetry_links",
			"attributes", "flags", "team_id", "environment", "instance_key",
		} {
			require.Equalf(t, string(before[column]), string(after[column]),
				"a partial update must not touch %q", column)
		}
		require.JSONEq(t, `"Triages accounts payable invoices and credit notes"`,
			string(after["purpose"]))
	})

	t.Run("a present empty value clears its column", func(t *testing.T) {
		agent := fx.registerFull(t, "expense-triage")
		before := agentSnapshot(t, fx.pool, agent.AgentID)
		require.NotEqual(t, "{}", string(before["hosting"]))

		res := fx.patch(t, agent, map[string]any{
			"purpose": "",
			"hosting": nil,
		})
		require.True(t, res.Changed)

		after := agentSnapshot(t, fx.pool, agent.AgentID)
		requireOnlyChanged(t, before, after,
			"purpose", "hosting", "flags", "last_heartbeat_at", "updated_at")

		require.JSONEq(t, `""`, string(after["purpose"]),
			"an explicitly empty purpose clears the column")
		require.JSONEq(t, `{}`, string(after["hosting"]),
			"an explicitly null section clears the column")

		// Clearing a field is a gap in the record, and a gap has to be visible.
		require.Contains(t, res.Flags, domain.FlagMissingPurpose)
		require.Contains(t, res.Flags, domain.FlagMissingHosting)
		require.NotContains(t, res.Flags, domain.FlagMissingPlatform,
			"the platform the update never mentioned is still there")

		var stored []string
		require.NoError(t, json.Unmarshal(after["flags"], &stored))
		require.Equal(t, res.Flags, stored, "returned and stored flags must agree")
	})

	// Ownership comes from the enrollment token at registration and cannot be
	// reassigned by a later update either — otherwise PATCH would be the hole in
	// the guarantee that registration is careful to close.
	t.Run("team and environment are ignored and flagged", func(t *testing.T) {
		agent := fx.registerFull(t, "scope-claim")
		before := agentSnapshot(t, fx.pool, agent.AgentID)

		res := fx.patch(t, agent, map[string]any{
			"team":        "finance",
			"team_id":     uuid.New().String(),
			"environment": "development",
			"env":         "development",
		})

		after := agentSnapshot(t, fx.pool, agent.AgentID)
		require.Equal(t, string(before["team_id"]), string(after["team_id"]),
			"an update must not reassign the owning team")
		require.Equal(t, string(before["environment"]), string(after["environment"]),
			"an update must not move the agent to another environment")

		require.Contains(t, res.Flags, domain.FlagIgnoredScopeFields,
			"an agent that tried to declare its own ownership must be flagged")

		// The refused claims are kept verbatim: an auditor asking "did this agent
		// try to reassign itself?" must be able to answer yes from the record.
		for _, key := range []string{"team", "team_id", "environment", "env"} {
			require.Containsf(t, string(after["attributes"]), key,
				"the ignored claim %q must be preserved as evidence", key)
		}
	})
}

// An administrator's correction outranks the agent's self-report. The guard has
// to be selective, though: protecting one column must not freeze the record, so
// this asserts both halves in one payload.
func TestUpdateSkipsHumanEditedFields(t *testing.T) {
	fx := newUpdateFixture(t, "update-human-edited")

	agent := fx.registerFull(t, "invoice-triage")
	markHumanEdited(t, fx.pool, agent.AgentID, "purpose", "display_name")

	before := agentSnapshot(t, fx.pool, agent.AgentID)

	res := fx.patch(t, agent, map[string]any{
		"name":    "Renamed by the agent",
		"purpose": "Rewritten by the agent",
		"platform": map[string]any{
			"framework":         "langgraph",
			"framework_version": "0.3.0",
		},
	})
	require.True(t, res.Changed, "the unprotected column did change")

	after := agentSnapshot(t, fx.pool, agent.AgentID)
	requireOnlyChanged(t, before, after, "platform", "last_heartbeat_at", "updated_at")

	require.Equal(t, string(before["purpose"]), string(after["purpose"]),
		"a human-edited purpose must survive the agent's self-report")
	require.Equal(t, string(before["display_name"]), string(after["display_name"]),
		"a human-edited display name must survive the agent's self-report")
	require.Contains(t, string(after["platform"]), "0.3.0",
		"a column nobody protected must still be updatable")
}

// The revision history is what an auditor reads. An agent that PATCHes the same
// descriptor on a timer must not fill it with identical snapshots, or the
// handful of real changes become unfindable exactly when someone needs them.
func TestUpdateWritesHistoryOnlyWhenSomethingChanged(t *testing.T) {
	fx := newUpdateFixture(t, "update-history")

	t.Run("a real change writes exactly one revision", func(t *testing.T) {
		agent := fx.registerFull(t, "invoice-triage")
		require.Len(t, revisionSnapshots(t, fx.pool, agent.AgentID), 1,
			"registration wrote the first revision")

		res := fx.patch(t, agent, map[string]any{
			"source": map[string]any{
				"repo_url":   "https://github.com/acme/invoice-triage",
				"commit_sha": "c0ffee1",
			},
		})
		require.True(t, res.Changed)

		snapshots := revisionSnapshots(t, fx.pool, agent.AgentID)
		require.Len(t, snapshots, 2, "an update that changed something writes one revision")
		require.NotEqual(t, snapshots[0], snapshots[1],
			"the two revisions must record different states")
		require.Contains(t, snapshots[1], "c0ffee1")

		require.Equal(t, []string{"agent:invoice-triage", "agent:invoice-triage"},
			revisionActors(t, fx.pool, agent.AgentID),
			"a revision that cannot be attributed is decoration")
	})

	t.Run("a no-op writes no revision and reports no change", func(t *testing.T) {
		agent := fx.registerFull(t, "expense-triage")
		aged := backdateHeartbeat(t, fx.pool, agent.AgentID)
		before := agentSnapshot(t, fx.pool, agent.AgentID)
		revisions := revisionSnapshots(t, fx.pool, agent.AgentID)
		audits := auditEntriesFor(t, fx.pool, agent.AgentID)

		for range 3 {
			res := fx.patch(t, agent, fullDescriptorFields("expense-triage"))
			require.False(t, res.Changed,
				"re-sending the same descriptor changes nothing")
		}

		require.Equal(t, revisions, revisionSnapshots(t, fx.pool, agent.AgentID),
			"an unchanged descriptor must not append to the revision history")
		require.Equal(t, audits, auditEntriesFor(t, fx.pool, agent.AgentID),
			"an unchanged descriptor must not append to the audit log")

		// An update is still proof of life, and updated_at still means "when the
		// record last said something different".
		after := agentSnapshot(t, fx.pool, agent.AgentID)
		requireOnlyChanged(t, before, after, "last_heartbeat_at")
		require.NotEqual(t, aged.String(), string(after["last_heartbeat_at"]))
	})

	t.Run("a change writes one audit entry with before and after", func(t *testing.T) {
		agent := fx.registerFull(t, "audited-triage")
		require.Len(t, auditEntriesFor(t, fx.pool, agent.AgentID), 1,
			"registration wrote the first audit entry")

		fx.patch(t, agent, map[string]any{"purpose": "Triages credit notes"})

		entries := auditEntriesFor(t, fx.pool, agent.AgentID)
		require.Len(t, entries, 2)

		entry := entries[1]
		require.Equal(t, "agent.updated", entry.action)
		require.Equal(t, "agent", entry.targetType)
		require.Equal(t, agent.AgentID.String(), entry.targetID)
		require.Equal(t, fx.orgID, entry.orgID)
		require.Equal(t, "agent:audited-triage", entry.actor)

		require.NotNil(t, entry.before, "an update records the state it replaced")
		require.NotNil(t, entry.after)
		require.NotEqual(t, *entry.before, *entry.after,
			"an audit entry whose before and after agree records nothing")
		require.Contains(t, *entry.after, "Triages credit notes")
		require.NotContains(t, *entry.before, "Triages credit notes")
	})
}

// The four failure modes stay distinguishable here for the operator's sake;
// collapsing them into one indistinguishable 401 is the HTTP layer's job. What
// matters at this layer is that every one of them writes nothing at all.
func TestUpdateRejectsInvalidCredentials(t *testing.T) {
	fx := newUpdateFixture(t, "update-credentials")

	first := fx.registerFull(t, "invoice-triage")
	second := fx.registerFull(t, "invoice-triage")
	require.Equal(t, first.AgentID, second.AgentID, "two replicas, one agent record")

	other := fx.registerFull(t, "expense-triage")
	require.NotEqual(t, first.AgentID, other.AgentID)

	revokeCredential(t, fx.pool, first.AgentToken)
	expireCredential(t, fx.pool, second.AgentToken)

	before := agentSnapshot(t, fx.pool, first.AgentID)
	otherBefore := agentSnapshot(t, fx.pool, other.AgentID)
	revisions := revisionSnapshots(t, fx.pool, first.AgentID)
	audits := auditEntriesFor(t, fx.pool, first.AgentID)

	cases := []struct {
		name  string
		token string
		want  error
	}{
		{"never issued", "aiinv_ac_neverissued", service.ErrCredentialNotFound},
		{"revoked", first.AgentToken, service.ErrCredentialRevoked},
		{"expired", second.AgentToken, service.ErrCredentialExpired},
		{"belongs to another agent", other.AgentToken, service.ErrCredentialAgentMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fx.upd.Update(context.Background(), service.UpdateInput{
				AgentID:       first.AgentID,
				AgentToken:    tc.token,
				RawDescriptor: descriptorJSON(t, map[string]any{"purpose": "Rewritten"}),
			})
			require.ErrorIs(t, err, tc.want)

			require.Equal(t, before, agentSnapshot(t, fx.pool, first.AgentID),
				"a rejected update must leave the agent untouched")
			require.Equal(t, otherBefore, agentSnapshot(t, fx.pool, other.AgentID),
				"and must not touch the credential's real owner either")
			require.Equal(t, revisions, revisionSnapshots(t, fx.pool, first.AgentID))
			require.Equal(t, audits, auditEntriesFor(t, fx.pool, first.AgentID))
		})
	}
}

// An update is proof of life, so it overrides the server's own inference from
// silence. It must not override a human's decision: an agent that keeps
// reporting after being retired is exactly what an operator needs to see, and a
// quarantined agent must not be able to release itself by continuing to run.
func TestUpdateRevivesIdleButNotRetiredAgents(t *testing.T) {
	fx := newUpdateFixture(t, "update-lifecycle")

	agent := fx.registerFull(t, "invoice-triage")

	for _, state := range []domain.LifecycleState{domain.LifecycleIdle, domain.LifecycleStale} {
		setLifecycle(t, fx.pool, agent.AgentID, state)
		fx.patch(t, agent, map[string]any{"purpose": "Triages " + string(state)})

		require.Equalf(t, string(domain.LifecycleActive),
			readAgentLiveness(t, fx.pool, agent.AgentID).lifecycleState,
			"an update must revive a %s agent", state)
	}

	for _, state := range []domain.LifecycleState{domain.LifecycleRetired, domain.LifecycleQuarantined} {
		setLifecycle(t, fx.pool, agent.AgentID, state)
		fx.patch(t, agent, map[string]any{"purpose": "Still running as " + string(state)})

		require.Equalf(t, string(state),
			readAgentLiveness(t, fx.pool, agent.AgentID).lifecycleState,
			"a %s agent must stay %s", state, state)
	}
}

func TestUpdateWritesNothingWhenItCannotComplete(t *testing.T) {
	fx := newUpdateFixture(t, "update-atomicity")

	t.Run("an unparseable payload is a hard error", func(t *testing.T) {
		agent := fx.registerFull(t, "invoice-triage")
		before := agentSnapshot(t, fx.pool, agent.AgentID)
		revisions := revisionSnapshots(t, fx.pool, agent.AgentID)

		_, err := fx.upd.Update(context.Background(), service.UpdateInput{
			AgentID:       agent.AgentID,
			AgentToken:    agent.AgentToken,
			RawDescriptor: []byte(`not json at all`),
		})
		require.Error(t, err)

		require.Equal(t, before, agentSnapshot(t, fx.pool, agent.AgentID))
		require.Equal(t, revisions, revisionSnapshots(t, fx.pool, agent.AgentID))
		require.Len(t, auditEntriesFor(t, fx.pool, agent.AgentID), 1,
			"only the registration's audit entry may exist")
	})

	// The agent row, the revision, and the audit entry are one transaction. An
	// agent row that moved without a revision is an inventory whose history has
	// a hole in it precisely where a change happened.
	//
	// The failure is forced at the database rather than through an injected
	// dependency, so the code under test is exactly the production path: a CHECK
	// constraint no revision row can satisfy makes the revision write fail after
	// the agent row has already been updated.
	t.Run("a failure after the row is updated rolls the whole update back", func(t *testing.T) {
		ctx := context.Background()
		agent := fx.registerFull(t, "expense-triage")
		before := agentSnapshot(t, fx.pool, agent.AgentID)
		revisions := revisionSnapshots(t, fx.pool, agent.AgentID)
		audits := auditEntriesFor(t, fx.pool, agent.AgentID)

		// NOT VALID, because the revisions written by registration are already
		// there and an unsatisfiable constraint cannot be added over them. It
		// still applies to every row inserted from here on, which is the row this
		// test is about.
		_, err := fx.pool.Exec(ctx,
			`ALTER TABLE agent_revisions ADD CONSTRAINT force_update_failure CHECK (false) NOT VALID`)
		require.NoError(t, err)

		changed := map[string]any{"purpose": "Triages expenses and mileage claims"}

		_, err = fx.upd.Update(ctx, service.UpdateInput{
			AgentID:       agent.AgentID,
			AgentToken:    agent.AgentToken,
			RawDescriptor: descriptorJSON(t, changed),
		})
		require.Error(t, err, "an update that cannot write its revision must fail")

		require.Equal(t, before, agentSnapshot(t, fx.pool, agent.AgentID),
			"the agent row must be rolled back with the rest of the update")
		require.Equal(t, revisions, revisionSnapshots(t, fx.pool, agent.AgentID))
		require.Equal(t, audits, auditEntriesFor(t, fx.pool, agent.AgentID))

		// And once the failure is removed the same update succeeds, which proves
		// the assertions above failed for the reason claimed rather than because
		// updating was broken outright.
		_, err = fx.pool.Exec(ctx,
			`ALTER TABLE agent_revisions DROP CONSTRAINT force_update_failure`)
		require.NoError(t, err)

		res := fx.patch(t, agent, changed)
		require.True(t, res.Changed, "the rolled back attempt must not have been applied")
		require.Len(t, revisionSnapshots(t, fx.pool, agent.AgentID), len(revisions)+1)
	})
}
