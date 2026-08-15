package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrNameRequired is the one hard rejection in this package. A record with no
// name cannot be shown to a human or reconciled against anything later, so it
// is worse than useless in an inventory.
var ErrNameRequired = errors.New("descriptor name is required")

type Platform struct {
	Framework        string
	FrameworkVersion string
}

type Hosting struct {
	Provider   string
	Region     string
	AccountID  string
	ResourceID string
}

type Source struct {
	RepoURL   string
	CommitSHA string
}

type TelemetryLink struct {
	Kind       string
	Label      string
	URL        string
	ExternalID string
}

// Descriptor is what an agent says about itself.
//
// There is deliberately no Team and no Environment field. Ownership is
// inherited from the enrollment token's scope, and an agent cannot misreport
// what the type cannot express — that is what makes the inventory defensible
// to an auditor rather than a list of self-reported assertions.
type Descriptor struct {
	Name           string
	Purpose        string
	InstanceKey    string
	Platform       Platform
	Hosting        Hosting
	Source         Source
	TelemetryLinks []TelemetryLink
	Attributes     map[string]json.RawMessage
}

// scopeKeys are the payload keys that assert ownership. They are recognised
// only so they can be refused: an agent does not get to choose its own team or
// environment, that comes from the enrollment token.
var scopeKeys = []string{"env", "environment", "team", "team_id"}

// DecodeDescriptor parses a registration payload.
//
// It decodes into a raw key map first rather than straight into Descriptor so
// that nothing is silently dropped: every key the struct does not claim lands
// in Attributes, where Validate can flag it and a human can later see exactly
// what the agent sent. That includes the scope keys, which are preserved as
// evidence of the attempt rather than honoured.
//
// The only error is a payload that is not a JSON object at all. A field of the
// wrong shape is odd, not fatal — it is kept verbatim in Attributes.
func DecodeDescriptor(raw []byte) (Descriptor, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Descriptor{}, fmt.Errorf("decode descriptor: %w", err)
	}

	var d Descriptor
	for key, value := range fields {
		var err error
		switch key {
		case "name":
			err = json.Unmarshal(value, &d.Name)
		case "purpose":
			err = json.Unmarshal(value, &d.Purpose)
		case "instance_key":
			err = json.Unmarshal(value, &d.InstanceKey)
		case "platform":
			var w platformWire
			if err = json.Unmarshal(value, &w); err == nil {
				d.Platform = Platform(w)
			}
		case "hosting":
			var w hostingWire
			if err = json.Unmarshal(value, &w); err == nil {
				d.Hosting = Hosting(w)
			}
		case "source":
			var w sourceWire
			if err = json.Unmarshal(value, &w); err == nil {
				d.Source = Source(w)
			}
		case "telemetry_links":
			var w []telemetryLinkWire
			if err = json.Unmarshal(value, &w); err == nil {
				d.TelemetryLinks = make([]TelemetryLink, len(w))
				for i, link := range w {
					d.TelemetryLinks[i] = TelemetryLink(link)
				}
			}
		default:
			// Unrecognised, or a scope key we refuse to honour.
			err = errUnclaimedKey
		}
		if err != nil {
			if d.Attributes == nil {
				d.Attributes = make(map[string]json.RawMessage, len(fields))
			}
			d.Attributes[key] = value
		}
	}
	return d, nil
}

// errUnclaimedKey is an internal sentinel: it routes a key to Attributes
// through the same path as a known key that failed to decode.
var errUnclaimedKey = errors.New("key not claimed by descriptor")

// Validate returns the flags raised by d, sorted and deduplicated so that
// stored flags and test assertions are deterministic.
//
// The only error is a missing name. Everything else is recorded and flagged:
// refusing a sparse descriptor would leave the agent invisible, which is
// strictly worse than an incomplete record of it.
func (d Descriptor) Validate() ([]string, error) {
	if strings.TrimSpace(d.Name) == "" {
		return nil, ErrNameRequired
	}

	var flags []string
	if strings.TrimSpace(d.Purpose) == "" {
		flags = append(flags, FlagMissingPurpose)
	}
	// A section is flagged only when the agent said nothing at all about it.
	// Partial information is still information and does not count as missing.
	if d.Platform == (Platform{}) {
		flags = append(flags, FlagMissingPlatform)
	}
	if d.Hosting == (Hosting{}) {
		flags = append(flags, FlagMissingHosting)
	}
	if d.Source == (Source{}) {
		flags = append(flags, FlagMissingSource)
	}
	for key := range d.Attributes {
		if slices.Contains(scopeKeys, key) {
			flags = append(flags, FlagIgnoredScopeFields)
		} else {
			flags = append(flags, FlagUnknownFields)
		}
	}

	slices.Sort(flags)
	return slices.Compact(flags), nil
}

// The wire types below carry the snake_case JSON tags so the exported structs
// do not have to. They convert field-for-field, which the compiler enforces:
// adding a field to one without the other stops the build.

type platformWire struct {
	Framework        string `json:"framework"`
	FrameworkVersion string `json:"framework_version"`
}

type hostingWire struct {
	Provider   string `json:"provider"`
	Region     string `json:"region"`
	AccountID  string `json:"account_id"`
	ResourceID string `json:"resource_id"`
}

type sourceWire struct {
	RepoURL   string `json:"repo_url"`
	CommitSHA string `json:"commit_sha"`
}

type telemetryLinkWire struct {
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	URL        string `json:"url"`
	ExternalID string `json:"external_id"`
}
