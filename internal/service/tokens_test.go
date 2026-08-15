package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/ebellefontaine/ai-inventory/internal/db/testdb"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
	"github.com/ebellefontaine/ai-inventory/internal/service"
)

// seedOrgAndTeam inserts one organization and one team and returns their ids.
// Each call uses a fresh slug so several scopes can coexist in one database.
func seedOrgAndTeam(t *testing.T, pool *pgxpool.Pool, slug string) (orgID, teamID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ('Acme', $1) RETURNING id`, slug,
	).Scan(&orgID))

	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO teams (org_id, name, slug) VALUES ($1, 'Platform', $2) RETURNING id`,
		orgID, slug,
	).Scan(&teamID))

	return orgID, teamID
}

// lastUsedAt returns the last_used_at of one token, nil when it has never
// been used.
func lastUsedAt(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) *time.Time {
	t.Helper()

	var at *time.Time
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT last_used_at FROM enrollment_tokens WHERE id = $1`, id).Scan(&at))
	return at
}

// rowAsJSON returns the whole token row rendered as text. Rendering every
// column rather than the ones we happen to think about means a column added
// later is covered by the "plaintext is never stored" test for free.
func rowAsJSON(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()

	var doc string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT to_jsonb(t)::text FROM enrollment_tokens t WHERE t.id = $1`, id).Scan(&doc))
	return doc
}

// countTokens returns the number of rows in enrollment_tokens.
func countTokens(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()

	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM enrollment_tokens`).Scan(&n))
	return n
}

func TestMintAndResolve(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	svc := service.NewTokenService(pool)
	org, team := seedOrgAndTeam(t, pool, "mint-resolve")

	minted, err := svc.Mint(ctx, service.MintInput{
		OrgID:       org,
		TeamID:      team,
		Environment: domain.EnvironmentProduction,
		Name:        "prod platform enrollment",
		CreatedBy:   "admin@example.com",
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, minted.ID)
	require.True(t, strings.HasPrefix(minted.Plaintext, "aiinv_et_"),
		"plaintext must carry the enrollment token prefix, got %q", minted.Plaintext)

	scope, err := svc.Resolve(ctx, minted.Plaintext)
	require.NoError(t, err)
	require.Equal(t, minted.ID, scope.TokenID)
	require.Equal(t, org, scope.OrgID)
	require.Equal(t, team, scope.TeamID)
	require.Equal(t, domain.EnvironmentProduction, scope.Environment)
}

func TestResolveRejectsRevokedToken(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	svc := service.NewTokenService(pool)
	org, team := seedOrgAndTeam(t, pool, "revoked")

	minted, err := svc.Mint(ctx, service.MintInput{
		OrgID: org, TeamID: team,
		Environment: domain.EnvironmentStaging,
		Name:        "to be revoked",
		CreatedBy:   "admin@example.com",
	})
	require.NoError(t, err)

	// It resolves before revocation, so the failure afterwards is the
	// revocation and not some unrelated defect.
	_, err = svc.Resolve(ctx, minted.Plaintext)
	require.NoError(t, err)

	require.NoError(t, svc.Revoke(ctx, minted.ID))

	_, err = svc.Resolve(ctx, minted.Plaintext)
	require.ErrorIs(t, err, service.ErrTokenRevoked)
}

func TestResolveRejectsExpiredToken(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	svc := service.NewTokenService(pool)
	org, team := seedOrgAndTeam(t, pool, "expired")

	past := time.Now().Add(-time.Hour)
	minted, err := svc.Mint(ctx, service.MintInput{
		OrgID: org, TeamID: team,
		Environment: domain.EnvironmentDevelopment,
		Name:        "already expired",
		CreatedBy:   "admin@example.com",
		ExpiresAt:   &past,
	})
	require.NoError(t, err)

	_, err = svc.Resolve(ctx, minted.Plaintext)
	require.ErrorIs(t, err, service.ErrTokenExpired)
}

// A token with an expiry in the future must still resolve. Without this the
// expiry check could be inverted — or reject every token that has an expiry
// at all — and the expired-token test above would still pass.
func TestResolveAcceptsTokenExpiringInTheFuture(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	svc := service.NewTokenService(pool)
	org, team := seedOrgAndTeam(t, pool, "future-expiry")

	future := time.Now().Add(24 * time.Hour)
	minted, err := svc.Mint(ctx, service.MintInput{
		OrgID: org, TeamID: team,
		Environment: domain.EnvironmentProduction,
		Name:        "expires tomorrow",
		CreatedBy:   "admin@example.com",
		ExpiresAt:   &future,
	})
	require.NoError(t, err)

	scope, err := svc.Resolve(ctx, minted.Plaintext)
	require.NoError(t, err)
	require.Equal(t, minted.ID, scope.TokenID)
	require.Equal(t, domain.EnvironmentProduction, scope.Environment)
}

func TestResolveRejectsUnknownToken(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	svc := service.NewTokenService(pool)

	for _, plaintext := range []string{
		"aiinv_et_neverminted",
		"garbage",
		"",
	} {
		_, err := svc.Resolve(ctx, plaintext)
		require.ErrorIsf(t, err, service.ErrTokenNotFound, "plaintext %q", plaintext)
	}
}

func TestResolveRecordsLastUsedAt(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	svc := service.NewTokenService(pool)
	org, team := seedOrgAndTeam(t, pool, "last-used")

	minted, err := svc.Mint(ctx, service.MintInput{
		OrgID: org, TeamID: team,
		Environment: domain.EnvironmentStaging,
		Name:        "tracks usage",
		CreatedBy:   "admin@example.com",
	})
	require.NoError(t, err)
	require.Nil(t, lastUsedAt(t, pool, minted.ID), "a freshly minted token has never been used")

	_, err = svc.Resolve(ctx, minted.Plaintext)
	require.NoError(t, err)

	used := lastUsedAt(t, pool, minted.ID)
	require.NotNil(t, used, "resolving must record that the token was used")
	require.WithinDuration(t, time.Now(), *used, time.Minute)
}

// The whole security argument for enrollment tokens rests on the plaintext
// never reaching storage. Assert it against the row rather than trusting the
// insert statement to have listed the right columns.
func TestPlaintextIsNeverStored(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	svc := service.NewTokenService(pool)
	org, team := seedOrgAndTeam(t, pool, "no-plaintext")

	minted, err := svc.Mint(ctx, service.MintInput{
		OrgID: org, TeamID: team,
		Environment: domain.EnvironmentProduction,
		Name:        "hash only",
		CreatedBy:   "admin@example.com",
	})
	require.NoError(t, err)

	row := rowAsJSON(t, pool, minted.ID)
	require.NotContains(t, row, minted.Plaintext, "no column may hold the plaintext token")

	// The secret part alone must not appear either, in case something stored
	// the token with the prefix stripped.
	secret := strings.TrimPrefix(minted.Plaintext, "aiinv_et_")
	require.NotEmpty(t, secret)
	require.NotContains(t, row, secret, "no column may hold the token secret")

	// And the row must actually hold a hash, so the assertions above are not
	// passing because nothing was written at all.
	var hash string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT token_hash FROM enrollment_tokens WHERE id = $1`, minted.ID).Scan(&hash))
	require.Len(t, hash, 64, "token_hash must be a hex-encoded SHA-256")
}

func TestMintProducesDistinctTokens(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	svc := service.NewTokenService(pool)
	org, team := seedOrgAndTeam(t, pool, "distinct")

	in := service.MintInput{
		OrgID: org, TeamID: team,
		Environment: domain.EnvironmentProduction,
		Name:        "same name, different secret",
		CreatedBy:   "admin@example.com",
	}

	first, err := svc.Mint(ctx, in)
	require.NoError(t, err)
	second, err := svc.Mint(ctx, in)
	require.NoError(t, err)

	require.NotEqual(t, first.ID, second.ID)
	require.NotEqual(t, first.Plaintext, second.Plaintext)

	var firstHash, secondHash string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT token_hash FROM enrollment_tokens WHERE id = $1`, first.ID).Scan(&firstHash))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT token_hash FROM enrollment_tokens WHERE id = $1`, second.ID).Scan(&secondHash))
	require.NotEqual(t, firstHash, secondHash)

	// Each resolves to itself, which is what proves the two rows are really
	// independent credentials and not one row written twice.
	firstScope, err := svc.Resolve(ctx, first.Plaintext)
	require.NoError(t, err)
	require.Equal(t, first.ID, firstScope.TokenID)

	secondScope, err := svc.Resolve(ctx, second.Plaintext)
	require.NoError(t, err)
	require.Equal(t, second.ID, secondScope.TokenID)
}

// An operator minting a token for environment "prod" made a typo. Rejecting it
// here is correct: the token would be useless and the mistake is cheap to fix
// now. Nothing may reach the database.
func TestMintRejectsInvalidEnvironment(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	svc := service.NewTokenService(pool)
	org, team := seedOrgAndTeam(t, pool, "bad-env")

	_, err := svc.Mint(ctx, service.MintInput{
		OrgID: org, TeamID: team,
		Environment: domain.Environment("prod"),
		Name:        "typo",
		CreatedBy:   "admin@example.com",
	})
	require.Error(t, err)
	require.Equal(t, 0, countTokens(t, pool), "an invalid environment must not reach the database")
}

func TestRevokeIsIdempotentAndReportsUnknownTokens(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	svc := service.NewTokenService(pool)
	org, team := seedOrgAndTeam(t, pool, "revoke-twice")

	minted, err := svc.Mint(ctx, service.MintInput{
		OrgID: org, TeamID: team,
		Environment: domain.EnvironmentProduction,
		Name:        "revoked twice",
		CreatedBy:   "admin@example.com",
	})
	require.NoError(t, err)

	require.NoError(t, svc.Revoke(ctx, minted.ID))

	var firstRevokedAt time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT revoked_at FROM enrollment_tokens WHERE id = $1`, minted.ID).Scan(&firstRevokedAt))

	require.NoError(t, svc.Revoke(ctx, minted.ID), "revoking twice must not be an error")

	var secondRevokedAt time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT revoked_at FROM enrollment_tokens WHERE id = $1`, minted.ID).Scan(&secondRevokedAt))
	require.Equal(t, firstRevokedAt, secondRevokedAt,
		"the first revocation time is the audit record and must not be overwritten")

	require.ErrorIs(t, svc.Revoke(ctx, uuid.New()), service.ErrTokenNotFound)
}
