package domain

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateAcceptsNameOnlyDescriptorWithFlags(t *testing.T) {
	d := Descriptor{Name: "billing-triage"}

	flags, err := d.Validate()

	require.NoError(t, err, "a sparse descriptor is recorded and flagged, never rejected")
	require.Equal(t, []string{
		FlagMissingHosting,
		FlagMissingPlatform,
		FlagMissingPurpose,
		FlagMissingSource,
	}, flags)
}

func TestValidateRequiresName(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":      "",
		"spaces":     "   ",
		"whitespace": "\t\n ",
	} {
		t.Run(name, func(t *testing.T) {
			flags, err := Descriptor{Name: raw}.Validate()
			require.ErrorIs(t, err, ErrNameRequired)
			require.Nil(t, flags)
		})
	}
}

func TestValidateFullyPopulatedDescriptorRaisesNoFlags(t *testing.T) {
	d := Descriptor{
		Name:        "billing-triage",
		Purpose:     "Triages inbound billing disputes.",
		InstanceKey: "billing-triage-7f3a",
		Platform:    Platform{Framework: "langgraph", FrameworkVersion: "0.2.14"},
		Hosting: Hosting{
			Provider:   "aws",
			Region:     "us-east-1",
			AccountID:  "123456789012",
			ResourceID: "arn:aws:ecs:us-east-1:123456789012:service/billing-triage",
		},
		Source: Source{
			RepoURL:   "https://github.com/acme/billing-triage",
			CommitSHA: "9d1c2f0a4b7e6d5c3a2b1f0e9d8c7b6a5f4e3d2c",
		},
		TelemetryLinks: []TelemetryLink{{
			Kind:       "dashboard",
			Label:      "Billing triage overview",
			URL:        "https://grafana.acme.internal/d/abc123",
			ExternalID: "abc123",
		}},
	}

	flags, err := d.Validate()

	require.NoError(t, err)
	require.Empty(t, flags)
}

func TestDecodeDescriptorKeepsUnrecognisedKeys(t *testing.T) {
	d, err := DecodeDescriptor([]byte(`{"name":"billing-triage","model_family":"claude"}`))

	require.NoError(t, err, "an unknown key is recorded, not rejected")
	require.Equal(t, "billing-triage", d.Name)
	require.Contains(t, d.Attributes, "model_family")
	require.JSONEq(t, `"claude"`, string(d.Attributes["model_family"]))

	flags, err := d.Validate()
	require.NoError(t, err)
	require.Contains(t, flags, FlagUnknownFields)
	require.NotContains(t, flags, FlagIgnoredScopeFields)
}

// Ownership is inherited from the enrollment token, never claimed by the
// agent. This test pins both halves of that guarantee: the scope keys are
// routed to Attributes and flagged, and the type itself cannot express them.
func TestDecodeDescriptorRefusesToHonourScopeFields(t *testing.T) {
	raw := []byte(`{
		"name": "billing-triage",
		"team": "platform",
		"team_id": "6f3b0d5e-1f3a-4a2b-8c9d-0e1f2a3b4c5d",
		"environment": "production",
		"env": "prod"
	}`)

	d, err := DecodeDescriptor(raw)
	require.NoError(t, err)

	for _, key := range []string{"team", "team_id", "environment", "env"} {
		require.Contains(t, d.Attributes, key, "scope key %q must be preserved as evidence, not dropped", key)
	}

	flags, err := d.Validate()
	require.NoError(t, err)
	require.Contains(t, flags, FlagIgnoredScopeFields)

	// The structural guarantee: no field can carry self-reported ownership.
	typ := reflect.TypeOf(Descriptor{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		require.NotContains(t, []string{"Team", "TeamID", "Environment", "Env"}, f.Name,
			"Descriptor must not be able to express ownership")
		require.NotEqual(t, reflect.TypeOf(Environment("")), f.Type,
			"Descriptor must not carry an Environment")
	}
}

func TestDecodeDescriptorRejectsUnparseableJSON(t *testing.T) {
	for name, raw := range map[string]string{
		"truncated":  `{"name":`,
		"not object": `["name"]`,
		"empty":      ``,
		"garbage":    `not json at all`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeDescriptor([]byte(raw))
			require.Error(t, err)
		})
	}
}

func TestValidateFlagsAreSortedAndDeduplicated(t *testing.T) {
	d, err := DecodeDescriptor([]byte(`{
		"name": "billing-triage",
		"team": "platform",
		"environment": "production",
		"model_family": "claude",
		"cost_centre": "CC-8817"
	}`))
	require.NoError(t, err)

	flags, err := d.Validate()

	require.NoError(t, err)
	require.Equal(t, []string{
		FlagIgnoredScopeFields,
		FlagMissingHosting,
		FlagMissingPlatform,
		FlagMissingPurpose,
		FlagMissingSource,
		FlagUnknownFields,
	}, flags, "two scope keys and two unknown keys must still yield one flag each, sorted")
}

func TestDecodeDescriptorRoundTripsAFullPayload(t *testing.T) {
	raw := []byte(`{
		"name": "billing-triage",
		"purpose": "Triages inbound billing disputes.",
		"instance_key": "billing-triage-7f3a",
		"platform": {"framework": "langgraph", "framework_version": "0.2.14"},
		"hosting": {
			"provider": "aws",
			"region": "us-east-1",
			"account_id": "123456789012",
			"resource_id": "arn:aws:ecs:us-east-1:123456789012:service/billing-triage"
		},
		"source": {
			"repo_url": "https://github.com/acme/billing-triage",
			"commit_sha": "9d1c2f0a4b7e6d5c3a2b1f0e9d8c7b6a5f4e3d2c"
		},
		"telemetry_links": [
			{"kind": "dashboard", "label": "Overview", "url": "https://grafana.acme.internal/d/abc123", "external_id": "abc123"},
			{"kind": "trace", "label": "Traces", "url": "https://tempo.acme.internal/x", "external_id": "x"}
		]
	}`)

	d, err := DecodeDescriptor(raw)
	require.NoError(t, err)

	require.Equal(t, Descriptor{
		Name:        "billing-triage",
		Purpose:     "Triages inbound billing disputes.",
		InstanceKey: "billing-triage-7f3a",
		Platform:    Platform{Framework: "langgraph", FrameworkVersion: "0.2.14"},
		Hosting: Hosting{
			Provider:   "aws",
			Region:     "us-east-1",
			AccountID:  "123456789012",
			ResourceID: "arn:aws:ecs:us-east-1:123456789012:service/billing-triage",
		},
		Source: Source{
			RepoURL:   "https://github.com/acme/billing-triage",
			CommitSHA: "9d1c2f0a4b7e6d5c3a2b1f0e9d8c7b6a5f4e3d2c",
		},
		TelemetryLinks: []TelemetryLink{
			{Kind: "dashboard", Label: "Overview", URL: "https://grafana.acme.internal/d/abc123", ExternalID: "abc123"},
			{Kind: "trace", Label: "Traces", URL: "https://tempo.acme.internal/x", ExternalID: "x"},
		},
	}, d)

	flags, err := d.Validate()
	require.NoError(t, err)
	require.Empty(t, flags)
}

// A known key whose value has the wrong shape is still evidence: it is kept in
// Attributes rather than turning the whole registration into a hard failure.
func TestDecodeDescriptorSalvagesMistypedKnownFields(t *testing.T) {
	d, err := DecodeDescriptor([]byte(`{"name":"billing-triage","platform":"langgraph"}`))

	require.NoError(t, err)
	require.Equal(t, Platform{}, d.Platform)
	require.Contains(t, d.Attributes, "platform")
	require.JSONEq(t, `"langgraph"`, string(d.Attributes["platform"]))
}

func TestDecodeDescriptorLeavesAttributesNilWhenNothingIsUnrecognised(t *testing.T) {
	d, err := DecodeDescriptor([]byte(`{"name":"billing-triage","purpose":"p"}`))

	require.NoError(t, err)
	require.Nil(t, d.Attributes)
	var _ map[string]json.RawMessage = d.Attributes
}

func TestEnumsMatchMigrationCheckConstraints(t *testing.T) {
	for _, e := range []Environment{EnvironmentDevelopment, EnvironmentStaging, EnvironmentProduction} {
		require.True(t, e.Valid(), "%s must be a valid environment", e)
	}
	require.False(t, Environment("prod").Valid())
	require.False(t, Environment("").Valid())

	for _, s := range []LifecycleState{
		LifecycleActive, LifecycleIdle, LifecycleStale, LifecycleRetired, LifecycleQuarantined,
	} {
		require.True(t, s.Valid(), "%s must be a valid lifecycle state", s)
	}
	require.False(t, LifecycleState("deleted").Valid())

	for _, s := range []RegistrationSource{SourceSelfRegistered, SourceDiscovered, SourceManual} {
		require.True(t, s.Valid(), "%s must be a valid registration source", s)
	}
	require.False(t, RegistrationSource("imported").Valid())

	require.Equal(t, "development", string(EnvironmentDevelopment))
	require.Equal(t, "self_registered", string(SourceSelfRegistered))
	require.Equal(t, "quarantined", string(LifecycleQuarantined))
}
