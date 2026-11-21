package pgdb

import (
	"context"
	"os"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL not set")
		}
		t.Skip("set TEST_DATABASE_URL")
	}
	pool, err := Open(context.Background(), url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func freshSchema(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	drop := func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+name+" CASCADE")
	}
	drop()
	t.Cleanup(drop)
}

func applied(t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT name FROM "+schema+".schema_migrations ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

func TestMigrateAppliesInOrderOnce(t *testing.T) {
	pool := testPool(t)
	freshSchema(t, pool, "pgdb_order")
	files := fstest.MapFS{
		// The second depends on the first, so the order has to be by name.
		"migrations/0002_more.sql": {Data: []byte("ALTER TABLE pgdb_order.things ADD COLUMN note text")},
		"migrations/0001_init.sql": {Data: []byte("CREATE TABLE pgdb_order.things (id int PRIMARY KEY)")},
		"migrations/README.md":     {Data: []byte("not a migration")},
	}
	m := Migrations{FS: files, Schema: "pgdb_order", Lock: 7410_9001}
	ctx := context.Background()
	if err := Migrate(ctx, pool, m); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, m); err != nil {
		t.Fatalf("second run: %v", err)
	}
	got := applied(t, pool, "pgdb_order")
	if len(got) != 2 || got[0] != "migrations/0001_init.sql" || got[1] != "migrations/0002_more.sql" {
		t.Fatalf("applied %v", got)
	}
}

func TestMigrateFailureRecordsNothing(t *testing.T) {
	pool := testPool(t)
	freshSchema(t, pool, "pgdb_fail")
	files := fstest.MapFS{
		"migrations/0001_init.sql": {Data: []byte("CREATE TABLE pgdb_fail.a (id int)")},
		"migrations/0002_bad.sql":  {Data: []byte("CREATE TABLE pgdb_fail.b (id int); SELECT no_such_column FROM pgdb_fail.a")},
	}
	err := Migrate(context.Background(), pool, Migrations{FS: files, Schema: "pgdb_fail", Lock: 7410_9002})
	if err == nil {
		t.Fatal("want an error from the bad migration")
	}
	if got := applied(t, pool, "pgdb_fail"); len(got) != 1 {
		t.Fatalf("applied %v, want only the first", got)
	}
	var exists bool
	if err := pool.QueryRow(context.Background(), "SELECT to_regclass('pgdb_fail.b') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("the failed migration's table survived its rollback")
	}
}

func TestMigrateReplicasSerialize(t *testing.T) {
	pool := testPool(t)
	freshSchema(t, pool, "pgdb_race")
	// Without the lock, two replicas would both try to create the table.
	files := fstest.MapFS{
		"migrations/0001_init.sql": {Data: []byte("CREATE TABLE pgdb_race.things (id int); SELECT pg_sleep(0.2)")},
	}
	m := Migrations{FS: files, Schema: "pgdb_race", Lock: 7410_9003}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = Migrate(context.Background(), pool, m)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d: %v", i, err)
		}
	}
	if got := applied(t, pool, "pgdb_race"); len(got) != 1 {
		t.Fatalf("applied %v", got)
	}
}

func TestMigrateRejectsOddSchema(t *testing.T) {
	err := Migrate(context.Background(), nil, Migrations{FS: fstest.MapFS{}, Schema: "x; DROP TABLE y"})
	if err == nil {
		t.Fatal("want an error")
	}
}
