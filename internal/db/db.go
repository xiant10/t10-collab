// Package db opens the Postgres pool and applies the embedded migrations, with
// t10-modkit's pgdb. What's this service's own is here: its schema and lock key.
package db

import (
	"context"
	"embed"

	"github.com/jackc/pgx/v5/pgxpool"
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

// Migrate applies every migration not yet recorded in collab.schema_migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return pgdb.Migrate(ctx, pool, pgdb.Migrations{FS: migrations, Schema: "collab", Lock: migrationLock})
}

// IsUniqueViolation reports whether err broke the named unique index.
func IsUniqueViolation(err error, constraint string) bool {
	return pgdb.IsUniqueViolation(err, constraint)
}
