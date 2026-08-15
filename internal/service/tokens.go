// Package service holds the operations the HTTP and MCP surfaces call. It
// speaks the domain vocabulary and owns the rules that are not the database's
// to enforce.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ebellefontaine/ai-inventory/internal/auth"
	"github.com/ebellefontaine/ai-inventory/internal/domain"
)

// enrollmentTokenPrefix makes a leaked token identifiable on sight — in a log,
// a commit, or a secret scanner — as an AI Inventory enrollment token.
const enrollmentTokenPrefix = "aiinv_et"

// The three failure modes below are distinguished for audit and operator
// diagnostics only: an administrator debugging a failing rollout needs to know
// whether the token was revoked, has expired, or never existed.
//
// The HTTP layer must collapse all three into one indistinguishable 401. A
// caller who can tell "revoked" from "not found" has a token-enumeration
// oracle: they can confirm which guessed tokens are real, which is precisely
// the information a brute-force search needs and the response must not carry.
var (
	// ErrTokenNotFound means no token with that hash exists.
	ErrTokenNotFound = errors.New("enrollment token not found")
	// ErrTokenRevoked means the token exists but an operator killed it.
	ErrTokenRevoked = errors.New("enrollment token revoked")
	// ErrTokenExpired means the token exists but is past its expires_at.
	ErrTokenExpired = errors.New("enrollment token expired")
	// ErrInvalidEnvironment means Mint was given an environment the database
	// would refuse. See Mint for why this one rejects rather than flags.
	ErrInvalidEnvironment = errors.New("invalid environment")
)

// MintInput describes the token an administrator is asking for. The scope —
// one owning team, one environment — is fixed here and never negotiable later.
type MintInput struct {
	OrgID       uuid.UUID
	TeamID      uuid.UUID
	Environment domain.Environment
	Name        string
	CreatedBy   string
	ExpiresAt   *time.Time // nil means the token never expires
}

// MintResult carries the one and only copy of the plaintext token. It is
// returned to the operator once and is not recoverable afterwards: only the
// hash is stored.
type MintResult struct {
	ID        uuid.UUID
	Plaintext string
}

// Scope is what an agent inherits by registering with a token. Ownership and
// environment come from here rather than from the agent's payload, which is
// what makes the inventory an audited list rather than a list of claims.
type Scope struct {
	TokenID     uuid.UUID
	OrgID       uuid.UUID
	TeamID      uuid.UUID
	Environment domain.Environment
}

// TokenService mints, resolves, and revokes enrollment tokens.
type TokenService struct{ pool *pgxpool.Pool }

func NewTokenService(pool *pgxpool.Pool) *TokenService { return &TokenService{pool: pool} }

const mintTokenSQL = `
INSERT INTO enrollment_tokens (
    org_id, team_id, environment, name, token_hash, created_by, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id;
`

// Mint issues a new enrollment token scoped to one team and one environment.
//
// An invalid environment is refused rather than recorded and flagged, which is
// the opposite of how this system treats a registering agent. The asymmetry is
// deliberate: a flag on an agent preserves a real thing we could not fully
// describe, whereas a token minted for environment "prod" describes nothing —
// it is an operator's typo, caught while the operator is still at the keyboard
// and can fix it. Storing it would only guarantee a confusing failure later.
func (s *TokenService) Mint(ctx context.Context, in MintInput) (MintResult, error) {
	if !in.Environment.Valid() {
		return MintResult{}, fmt.Errorf("mint token: %w: %q", ErrInvalidEnvironment, in.Environment)
	}

	plain, hash, err := auth.GenerateToken(enrollmentTokenPrefix)
	if err != nil {
		return MintResult{}, fmt.Errorf("mint token: %w", err)
	}

	var id uuid.UUID
	err = s.pool.QueryRow(ctx, mintTokenSQL,
		in.OrgID, in.TeamID, string(in.Environment), in.Name, hash, in.CreatedBy, in.ExpiresAt,
	).Scan(&id)
	if err != nil {
		return MintResult{}, fmt.Errorf("mint token: %w", err)
	}

	return MintResult{ID: id, Plaintext: plain}, nil
}

// The lookup is by hash alone. It deliberately does not filter on revoked_at
// or expires_at in SQL: a query that returned no row for a revoked token could
// not tell the audit trail apart from a token that never existed, and that
// distinction is the whole point of the sentinel errors.
const resolveTokenSQL = `
SELECT id, org_id, team_id, environment, expires_at, revoked_at
FROM enrollment_tokens
WHERE token_hash = $1;
`

const touchTokenSQL = `
UPDATE enrollment_tokens SET last_used_at = now() WHERE id = $1;
`

// Resolve exchanges a plaintext enrollment token for the scope an agent
// registering with it inherits.
//
// Only the hash ever leaves this function's stack, so a stored-token compromise
// yields nothing usable and the lookup index carries no secret material.
//
// Revocation is reported ahead of expiry when a token is both: an operator who
// deliberately killed a token wants to see that, not the incidental fact that
// it also aged out.
func (s *TokenService) Resolve(ctx context.Context, plaintext string) (Scope, error) {
	var (
		scope     Scope
		env       string
		expiresAt *time.Time
		revokedAt *time.Time
	)

	err := s.pool.QueryRow(ctx, resolveTokenSQL, auth.HashToken(plaintext)).Scan(
		&scope.TokenID, &scope.OrgID, &scope.TeamID, &env, &expiresAt, &revokedAt,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Scope{}, ErrTokenNotFound
	case err != nil:
		return Scope{}, fmt.Errorf("resolve token: %w", err)
	}

	if revokedAt != nil {
		return Scope{}, ErrTokenRevoked
	}
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return Scope{}, ErrTokenExpired
	}
	scope.Environment = domain.Environment(env)

	// last_used_at is what tells an operator a minted token was never picked
	// up, or is still in circulation months after the rollout it was cut for.
	//
	// This is a write on every registration, on the hot path, in its own round
	// trip. At phase-1 volumes that is fine. If registrations get heavy this is
	// the obvious thing to batch — collect touches and flush them periodically,
	// accepting a slightly stale timestamp for a diagnostic field.
	//
	// The error is discarded on purpose. The scope is already resolved and
	// correct; failing an agent's registration because a diagnostic timestamp
	// could not be written would trade a real capability for a nicety. Once
	// there is a logger in this package, this belongs at warning level.
	_, _ = s.pool.Exec(ctx, touchTokenSQL, scope.TokenID)

	return scope, nil
}

// COALESCE keeps the first revocation time rather than overwriting it. When a
// token is revoked twice, the moment it stopped being trusted is the first
// one, and that is the moment an audit needs.
const revokeTokenSQL = `
UPDATE enrollment_tokens
SET revoked_at = COALESCE(revoked_at, now())
WHERE id = $1;
`

// Revoke kills a token. It is idempotent: revoking an already-revoked token
// succeeds and changes nothing, because the caller's intent — that this token
// no longer works — is already true.
func (s *TokenService) Revoke(ctx context.Context, tokenID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, revokeTokenSQL, tokenID)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTokenNotFound
	}
	return nil
}
