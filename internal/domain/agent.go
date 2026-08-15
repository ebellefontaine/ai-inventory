// Package domain defines the vocabulary the service layer speaks about
// agents. It deliberately has no database dependency: these types describe
// what an agent is, not how a row is written.
package domain

// Environment is the deployment environment an agent runs in. The values must
// stay in step with the agents_environment_check constraint in migration
// 00003; a value this package can produce but the database rejects would turn
// a routine registration into a write failure.
type Environment string

const (
	EnvironmentDevelopment Environment = "development"
	EnvironmentStaging     Environment = "staging"
	EnvironmentProduction  Environment = "production"
)

// Valid reports whether e is one of the environments the database accepts.
func (e Environment) Valid() bool {
	switch e {
	case EnvironmentDevelopment, EnvironmentStaging, EnvironmentProduction:
		return true
	default:
		return false
	}
}

// LifecycleState is where an agent sits between first registration and
// retirement. Mirrors agents_lifecycle_check in migration 00003.
type LifecycleState string

const (
	LifecycleActive      LifecycleState = "active"
	LifecycleIdle        LifecycleState = "idle"
	LifecycleStale       LifecycleState = "stale"
	LifecycleRetired     LifecycleState = "retired"
	LifecycleQuarantined LifecycleState = "quarantined"
)

// Valid reports whether s is one of the lifecycle states the database accepts.
func (s LifecycleState) Valid() bool {
	switch s {
	case LifecycleActive, LifecycleIdle, LifecycleStale, LifecycleRetired, LifecycleQuarantined:
		return true
	default:
		return false
	}
}

// RegistrationSource records how an agent came to be in the inventory, which
// is what lets an auditor tell a self-reported record from a discovered one.
// Mirrors agents_registration_source_check in migration 00003.
type RegistrationSource string

const (
	SourceSelfRegistered RegistrationSource = "self_registered"
	SourceDiscovered     RegistrationSource = "discovered"
	SourceManual         RegistrationSource = "manual"
)

// Valid reports whether s is one of the registration sources the database
// accepts.
func (s RegistrationSource) Valid() bool {
	switch s {
	case SourceSelfRegistered, SourceDiscovered, SourceManual:
		return true
	default:
		return false
	}
}

// Flags are the record of everything that was wrong, odd, or ignored about a
// registration. Nothing here rejects a descriptor: an inventory that turns bad
// input away develops blind spots, and blind spots are the failure this
// product exists to eliminate. The flag is how the gap stays visible.
const (
	FlagMissingPurpose  = "missing_purpose"
	FlagMissingPlatform = "missing_platform"
	FlagMissingHosting  = "missing_hosting"
	// FlagMissingSource concerns the code provenance (repo and commit), not
	// the RegistrationSource above.
	FlagMissingSource = "missing_source"
	// FlagIgnoredScopeFields marks an agent that tried to declare its own
	// team or environment. Ownership is inherited from the enrollment token,
	// so the claim is not honoured, but the attempt is worth seeing.
	FlagIgnoredScopeFields = "ignored_scope_fields"
	FlagUnknownFields      = "unknown_fields"
)
