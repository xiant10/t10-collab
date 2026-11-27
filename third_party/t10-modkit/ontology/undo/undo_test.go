package undo_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xiant10/t10-modkit/ontology"
	"github.com/xiant10/t10-modkit/ontology/undo"
	"github.com/xiant10/t10-modkit/pgdb"
)

func testPostgres(t *testing.T) undo.Postgres {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL not set")
		}
		t.Skip("set TEST_DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgdb.Open(ctx, url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	drop := func() { _, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS ontology_undo_test CASCADE") }
	drop()
	t.Cleanup(drop)
	if err := pgdb.Migrate(ctx, pool, undo.Migrations("ontology_undo_test", 9_100_002)); err != nil {
		t.Fatal(err)
	}
	return undo.Postgres{Pool: pool, Schema: "ontology_undo_test"}
}

func TestMemory(t *testing.T) { exercise(t, &undo.Memory{}) }

func TestPostgres(t *testing.T) { exercise(t, testPostgres(t)) }

func exercise(t *testing.T, s ontology.UndoStore) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	entry := ontology.UndoEntry{ID: "undo_1", Tenant: "t1", User: "u1", Action: "record.add", Object: "sales:deal:d1",
		Payload: []byte(`{"record":"r1"}`), CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	if err := s.SaveUndo(ctx, entry); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetUndo(ctx, "t1", "undo_1")
	if err != nil || got.User != "u1" || got.Undone || !got.ExpiresAt.Equal(entry.ExpiresAt) || got.Object != entry.Object {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := s.GetUndo(ctx, "t2", "undo_1"); !errors.Is(err, ontology.ErrNotFound) {
		t.Fatalf("another tenant's entry: %v", err)
	}
	if err := s.SetUndone(ctx, "t1", "undo_1", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUndone(ctx, "t1", "undo_1", true); !errors.Is(err, ontology.ErrUndone) {
		t.Fatalf("undoing twice: %v", err)
	}
	if err := s.SetUndone(ctx, "t1", "undo_1", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUndo(ctx, "t1", "undo_1"); got.Undone {
		t.Fatal("a failed undo stays undone")
	}
	if err := s.SetUndone(ctx, "t1", "undo_nope", true); !errors.Is(err, ontology.ErrNotFound) {
		t.Fatalf("an unknown entry: %v", err)
	}

	if _, found, err := s.GetReplay(ctx, "t1", "u1", "record.add", "k1"); found || err != nil {
		t.Fatalf("no replay yet: %v %v", found, err)
	}
	first := ontology.Replay{Tenant: "t1", User: "u1", Action: "record.add", Key: "k1", Hash: "h1"}
	if _, reserved, err := s.ReserveReplay(ctx, first); err != nil || !reserved {
		t.Fatalf("reserve: %v %v", reserved, err)
	}
	// While the first request runs, the key is held, with no answer.
	kept, reserved, err := s.ReserveReplay(ctx, ontology.Replay{Tenant: "t1", User: "u1", Action: "record.add", Key: "k1", Hash: "h2"})
	if err != nil || reserved || kept.Hash != "h1" || kept.Answer != nil {
		t.Fatalf("a second reserve: %+v %v %v", kept, reserved, err)
	}
	// A request that changed nothing gives its key back.
	if err := s.ReleaseReplay(ctx, "t1", "u1", "record.add", "k1"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetReplay(ctx, "t1", "u1", "record.add", "k1"); found {
		t.Fatal("a released key is still held")
	}
	if _, reserved, _ := s.ReserveReplay(ctx, first); !reserved {
		t.Fatal("a released key can't be reserved again")
	}
	first.Answer = []byte(`{"undo":{"id":"undo_1"}}`)
	if err := s.FinishReplay(ctx, first); err != nil {
		t.Fatal(err)
	}
	// A finished key keeps its answer, and isn't released.
	_ = s.ReleaseReplay(ctx, "t1", "u1", "record.add", "k1")
	kept, found, err := s.GetReplay(ctx, "t1", "u1", "record.add", "k1")
	if err != nil || !found || kept.Hash != "h1" || !strings.Contains(string(kept.Answer), "undo_1") {
		t.Fatalf("the finished answer: %+v %v %v", kept, found, err)
	}
	// Keys are per person.
	if _, found, _ := s.GetReplay(ctx, "t1", "u2", "record.add", "k1"); found {
		t.Fatal("another person's key")
	}
}
