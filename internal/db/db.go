// Package db opens the Postgres pool and applies the embedded migrations, with
// t10-modkit's pgdb. What's this service's own is here: its schema and lock key.
package db

import (
	"context"
	"embed"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xiant10/t10-modkit/ontology/proposals"
	"github.com/xiant10/t10-modkit/ontology/undo"
	"github.com/xiant10/t10-modkit/pgdb"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationLock is the advisory lock key that serializes replicas at startup.
const migrationLock = 7410_0002

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	return pgdb.Open(ctx, url, 0)
}

// Migrate applies every migration not yet recorded in collab.schema_migrations:
// collab's own, then the ontology's drafts and undo entries, in the same schema.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	for _, m := range []pgdb.Migrations{
		{FS: migrations, Schema: "collab", Lock: migrationLock},
		proposals.Migrations("collab", migrationLock),
		undo.Migrations("collab", migrationLock),
	} {
		if err := pgdb.Migrate(ctx, pool, m); err != nil {
			return err
		}
	}
	return nil
}

// IsUniqueViolation reports whether err broke the named unique index.
func IsUniqueViolation(err error, constraint string) bool {
	return pgdb.IsUniqueViolation(err, constraint)
}
