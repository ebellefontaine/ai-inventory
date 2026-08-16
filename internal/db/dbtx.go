package db

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX is satisfied by both *pgxpool.Pool and pgx.Tx, so a store method can
// run standalone or inside a caller's transaction.
//
// It exists because atomicity is a property of the caller, not of the
// statement. A service that must write an agent row together with a credential,
// a revision, and an audit entry has to issue all of them on one connection; a
// store that holds only a pool would hand it a different connection and let the
// agent row commit on its own. Rather than have such a caller keep its own copy
// of the statement — which is how the human-edit guard would drift out of
// agreement with itself — the store takes whichever executor the caller already
// has.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Compile-time proof that the two executors this package is used with actually
// satisfy the interface. Without these, a signature drift in pgx would only
// surface at the call sites, one package away from the definition it broke.
var (
	_ DBTX = (*pgxpool.Pool)(nil)
	_ DBTX = (pgx.Tx)(nil)
)
