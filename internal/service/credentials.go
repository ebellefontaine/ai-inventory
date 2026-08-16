package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebellefontaine/ai-inventory/internal/auth"
	"github.com/ebellefontaine/ai-inventory/internal/db"
)

// The four failure modes below are distinguished for audit and operator
// diagnostics only: an administrator chasing a fleet that has gone quiet needs
// to know whether a credential was revoked, has expired, never existed, or is
// being presented for the wrong agent.
//
// The HTTP layer in Task 20 must collapse all four into one indistinguishable
// 401. A caller who can tell "revoked" from "not found" has a
// credential-enumeration oracle: they can confirm which guessed credentials are
// real, which is precisely the information a brute-force search needs and the
// response must not carry. ErrCredentialAgentMismatch leaks more than the
// others if returned — it confirms a credential is valid while naming the agent
// it is not valid for — so it must be collapsed with the rest rather than
// treated as the "safe" one to report.
var (
	// ErrCredentialNotFound means no credential with that hash exists.
	ErrCredentialNotFound = errors.New("agent credential not found")
	// ErrCredentialRevoked means the credential exists but an operator killed it.
	ErrCredentialRevoked = errors.New("agent credential revoked")
	// ErrCredentialExpired means the credential exists but is past its expires_at.
	ErrCredentialExpired = errors.New("agent credential expired")
	// ErrCredentialAgentMismatch means the credential is valid but belongs to a
	// different agent than the one the caller names. Without this check, any
	// agent in the installation could keep any other agent looking alive.
	ErrCredentialAgentMismatch = errors.New("agent credential belongs to a different agent")
)

// authenticateAgent is the single definition of what a usable per-agent
// credential is: one that exists, has not been revoked, has not expired, and
// belongs to the agent presenting it. Every operation an agent authenticates
// calls this, so no two of them can come to disagree about what "valid" means —
// a disagreement would show up as one endpoint honouring a credential another
// endpoint had already refused.
//
// It takes a db.DBTX rather than a pool so that the check runs inside the
// caller's transaction. That is what makes a refused call write nothing at all:
// the rejection returns before any write, and the caller's rollback covers the
// transaction it was already holding.
//
// op names the caller's operation and prefixes the wrapped infrastructure
// error, so a failure in the log says which request could not resolve a
// credential. The sentinels are returned bare, never wrapped in op: callers
// return them unchanged so the HTTP layer can match them.
//
// It returns the credential's own id, which the caller needs in order to stamp
// its liveness. The credential's agent id is deliberately not returned — past
// the mismatch check it is equal to agentID by construction.
func authenticateAgent(ctx context.Context, tx db.DBTX, agentID uuid.UUID, token, op string) (uuid.UUID, error) {
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
		return uuid.Nil, ErrCredentialNotFound
	case err != nil:
		return uuid.Nil, fmt.Errorf("%s: resolve credential: %w", op, err)
	}

	// Revocation is reported ahead of expiry, and both ahead of the ownership
	// mismatch. An operator who deliberately killed a credential wants to see
	// that rather than the incidental fact that it also aged out, and a
	// credential that is dead in its own right is not worth reporting as
	// somebody else's.
	switch {
	case revokedAt != nil:
		return uuid.Nil, ErrCredentialRevoked
	case expiresAt != nil && !expiresAt.After(time.Now()):
		return uuid.Nil, ErrCredentialExpired
	case ownerID != agentID:
		return uuid.Nil, ErrCredentialAgentMismatch
	}
	return credentialID, nil
}

// The lookup is by hash alone, and deliberately does not filter on revoked_at,
// expires_at, or agent_id in SQL. A query that returned no row for a revoked
// credential could not tell the audit trail apart from a credential that never
// existed, and that distinction is the whole point of the sentinel errors. The
// same goes for the ownership check: filtering on agent_id here would report a
// stolen credential as one that does not exist.
const resolveCredentialSQL = `
SELECT id, agent_id, expires_at, revoked_at
FROM agent_credentials
WHERE token_hash = $1;
`
