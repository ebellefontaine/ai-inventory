package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A self-hosted installation is one organisation. There is no sign-up flow to
// name it and nothing in phase 1 that can have two, so the alternative to
// creating it on first start is an operator who cannot mint a token until they
// have written an INSERT by hand — which is not "one binary plus a connection
// string".
//
// The slug is what makes the creation idempotent, and it is deliberately a
// constant rather than configuration: an installation that changed its slug
// between restarts would create a second organisation and split the inventory
// in two without ever reporting an error.
const (
	DefaultOrgSlug = "default"
	DefaultOrgName = "Default"
)

// ErrTeamSlugRequired means a team was asked for by no name at all. Like an
// invalid environment in Mint, this is refused rather than recorded: it is a
// typo caught while the operator is still at the keyboard, and a team nobody can
// name again is not a team anybody can mint a second token for.
var ErrTeamSlugRequired = errors.New("team slug is required")

// BootstrapService creates the rows an installation cannot function without and
// that nothing else in phase 1 owns: the single organisation, and the teams an
// operator names when minting an enrollment token.
//
// It is separate from TokenService because minting is an operation on a scope
// that exists, and this is what makes a scope exist. Folding it into Mint would
// mean every token minted for a mistyped team silently founded one.
type BootstrapService struct{ pool *pgxpool.Pool }

func NewBootstrapService(pool *pgxpool.Pool) *BootstrapService {
	return &BootstrapService{pool: pool}
}

// ON CONFLICT ... DO UPDATE rather than DO NOTHING, because DO NOTHING returns
// no row and would need a second SELECT — which is a race in disguise: between
// the two statements another process may have created the row this one was
// about to read. Setting slug to itself changes nothing observable, keeps the
// existing name if an administrator has since renamed the organisation, and
// makes RETURNING report the surviving row on both paths.
const ensureOrgSQL = `
INSERT INTO organizations (name, slug) VALUES ($1, $2)
ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug
RETURNING id;
`

// EnsureOrg returns the installation's organisation, creating it if this is the
// first start. It is safe to call on every start and from two processes at once.
func (s *BootstrapService) EnsureOrg(ctx context.Context) (uuid.UUID, error) {
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, ensureOrgSQL, DefaultOrgName, DefaultOrgSlug).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("ensure default organisation: %w", err)
	}
	return id, nil
}

const ensureTeamSQL = `
INSERT INTO teams (org_id, name, slug) VALUES ($1, $2, $3)
ON CONFLICT (org_id, slug) DO UPDATE SET slug = EXCLUDED.slug
RETURNING id;
`

// EnsureTeam returns the team with this slug, creating it if it does not exist.
//
// Creating on demand is what keeps `aiinv token create --team finance-platform`
// the first command an operator runs. The alternative — a separate `team create`
// — buys nothing at this stage: there is no team attribute to set that minting
// does not already imply, and a token minted for a team that has to be declared
// first is two commands where one will do.
//
// The display name starts as the slug. There is nowhere in phase 1 to type a
// prettier one, and a name derived by guessing at capitalisation would be a
// value an administrator has to correct rather than one they chose.
func (s *BootstrapService) EnsureTeam(ctx context.Context, orgID uuid.UUID, slug string) (uuid.UUID, error) {
	// Normalised before it is looked up, so the same team typed two ways is one
	// team. A slug is an identifier an operator retypes months later from
	// memory; case and stray whitespace must not fork the inventory.
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return uuid.Nil, fmt.Errorf("ensure team: %w", ErrTeamSlugRequired)
	}

	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, ensureTeamSQL, orgID, slug, slug).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("ensure team %q: %w", slug, err)
	}
	return id, nil
}
