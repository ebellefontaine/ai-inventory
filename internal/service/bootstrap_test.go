package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

// countRows lives in register_test.go.

// TestEnsureOrgIsIdempotent is the property that lets bootstrap run on every
// start: a second start must find the organisation the first one created rather
// than failing on the unique slug or creating a second installation.
func TestEnsureOrgIsIdempotent(t *testing.T) {
	pool := testdb.New(t)
	bootstrap := service.NewBootstrapService(pool)
	ctx := context.Background()

	first, err := bootstrap.EnsureOrg(ctx)
	require.NoError(t, err)

	second, err := bootstrap.EnsureOrg(ctx)
	require.NoError(t, err)

	require.Equal(t, first, second, "the second start must adopt the first start's organisation")
	require.Equal(t, 1, countRows(t, pool, "organizations"))
}

// TestEnsureTeamCreatesThenReuses is what makes `aiinv token create --team x`
// usable on a fresh install: the first mention of a team creates it, and every
// later mention lands on the same one.
func TestEnsureTeamCreatesThenReuses(t *testing.T) {
	pool := testdb.New(t)
	bootstrap := service.NewBootstrapService(pool)
	ctx := context.Background()

	orgID, err := bootstrap.EnsureOrg(ctx)
	require.NoError(t, err)

	created, err := bootstrap.EnsureTeam(ctx, orgID, "finance-platform")
	require.NoError(t, err)

	reused, err := bootstrap.EnsureTeam(ctx, orgID, "finance-platform")
	require.NoError(t, err)

	require.Equal(t, created, reused, "a team that already exists must be reused, not duplicated")
	require.Equal(t, 1, countRows(t, pool, "teams"))

	other, err := bootstrap.EnsureTeam(ctx, orgID, "data-science")
	require.NoError(t, err)
	require.NotEqual(t, created, other, "a different slug is a different team")
	require.Equal(t, 2, countRows(t, pool, "teams"))
}

// TestEnsureTeamNormalisesSlug: a slug is an identifier, and an operator who
// types "Finance-Platform " once and "finance-platform" the next day must not
// end up owning two teams and two half-populated inventories.
func TestEnsureTeamNormalisesSlug(t *testing.T) {
	pool := testdb.New(t)
	bootstrap := service.NewBootstrapService(pool)
	ctx := context.Background()

	orgID, err := bootstrap.EnsureOrg(ctx)
	require.NoError(t, err)

	created, err := bootstrap.EnsureTeam(ctx, orgID, "finance-platform")
	require.NoError(t, err)

	reused, err := bootstrap.EnsureTeam(ctx, orgID, "  Finance-Platform ")
	require.NoError(t, err)

	require.Equal(t, created, reused)
	require.Equal(t, 1, countRows(t, pool, "teams"))
}

// TestEnsureTeamRejectsEmptySlug: an unnamed team is an operator's mistake at
// the keyboard, and a row nobody can ask for again by name.
func TestEnsureTeamRejectsEmptySlug(t *testing.T) {
	pool := testdb.New(t)
	bootstrap := service.NewBootstrapService(pool)
	ctx := context.Background()

	orgID, err := bootstrap.EnsureOrg(ctx)
	require.NoError(t, err)

	_, err = bootstrap.EnsureTeam(ctx, orgID, "   ")
	require.ErrorIs(t, err, service.ErrTeamSlugRequired)
	require.Equal(t, 0, countRows(t, pool, "teams"))
}
