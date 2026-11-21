// Package pgdb opens a service's Postgres pool and applies its embedded
// migrations. Each service keeps its own schema, migration table and advisory
// lock key; this package only runs them, the same way everywhere.
package pgdb

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open connects to Postgres and verifies the connection. maxConns caps the pool
// for work that must not take connections from serving; zero keeps pgx's default.
func Open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Migrations is one service's migration set.
type Migrations struct {
	// FS holds the files at migrations/*.sql, as `//go:embed migrations/*.sql`
	// gives them. A file's path, such as migrations/0001_init.sql, is the name
	// recorded once it's applied, so files are never renamed.
	FS fs.FS
	// Schema owns the schema_migrations table, and is created if missing.
	Schema string
	// Lock is the advisory lock key that serializes replicas at startup. Each
	// service has its own; see the table in t10-modkit's docs/api.md.
	Lock int64
}

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Migrate applies every migration not yet recorded, in name order, each in its
// own transaction, while holding the advisory lock.
func Migrate(ctx context.Context, pool *pgxpool.Pool, m Migrations) error {
	if !identifier.MatchString(m.Schema) {
		return fmt.Errorf("migrate: schema %q isn't a plain identifier", m.Schema)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", m.Lock); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		// The connection returns to the pool, so the lock must not outlive
		// this call even when ctx has already been cancelled.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", m.Lock)
	}()

	if _, err := conn.Exec(ctx, fmt.Sprintf(`
		CREATE SCHEMA IF NOT EXISTS %[1]s;
		CREATE TABLE IF NOT EXISTS %[1]s.schema_migrations (
			name       text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`, m.Schema)); err != nil {
		return fmt.Errorf("prepare migrations table: %w", err)
	}

	names, err := fs.Glob(m.FS, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		if err := applyOne(ctx, conn.Conn(), m, name); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func applyOne(ctx context.Context, conn *pgx.Conn, m Migrations, name string) error {
	var applied bool
	err := conn.QueryRow(ctx,
		fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s.schema_migrations WHERE name = $1)", m.Schema), name).Scan(&applied)
	if err != nil || applied {
		return err
	}
	body, err := fs.ReadFile(m.FS, name)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s.schema_migrations (name) VALUES ($1)", m.Schema), name)
		return err
	})
}

// IsUniqueViolation reports whether err broke the named unique index.
func IsUniqueViolation(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == constraint
}
