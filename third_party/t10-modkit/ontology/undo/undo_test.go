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
	// Once reverted it is done: it can't be unmarked.
	if err := s.FinishUndo(ctx, "t1", "undo_1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUndo(ctx, "t1", "undo_1"); !got.Undone || !got.Reverted {
		t.Fatalf("finished: %+v", got)
	}
	_ = s.SetUndone(ctx, "t1", "undo_1", false)
	if got, _ := s.GetUndo(ctx, "t1", "undo_1"); !got.Undone || !got.Reverted {
		t.Fatalf("a finished undo was unmarked: %+v", got)
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

	// What agents did: an open undo entry, one already undone (undo_1), and an
	// action with no undo; only the person's own, newest first, since a time.
	open := ontology.UndoEntry{ID: "undo_2", Tenant: "t1", User: "u1", Action: "record.add", Object: "sales:deal:d1",
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	if err := s.SaveUndo(ctx, open); err != nil {
		t.Fatal(err)
	}
	for i, d := range []ontology.DoneEntry{
		{ID: "done_1", Tenant: "t1", User: "u1", Action: "record.add", Object: "sales:deal:d1", Agent: "ti", UndoID: "undo_1", CreatedAt: now},
		{ID: "done_2", Tenant: "t1", User: "u1", Action: "deal.set_stage", Object: "sales:deal:d1", Agent: "ti", CreatedAt: now.Add(time.Minute)},
		{ID: "done_3", Tenant: "t1", User: "u1", Action: "record.add", Object: "sales:deal:d1", Agent: "ti", UndoID: "undo_2", CreatedAt: now.Add(2 * time.Minute)},
		{ID: "done_4", Tenant: "t1", User: "u2", Action: "record.add", Agent: "ti", CreatedAt: now.Add(3 * time.Minute)},
		{ID: "done_0", Tenant: "t1", User: "u1", Action: "record.add", Agent: "ti", CreatedAt: now.Add(-48 * time.Hour)},
	} {
		if err := s.SaveDone(ctx, d); err != nil {
			t.Fatalf("done %d: %v", i, err)
		}
	}
	list, err := s.RecentDone(ctx, "t1", "u1", now.Add(-time.Hour), now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range list {
		ids = append(ids, d.ID)
	}
	if strings.Join(ids, ",") != "done_3,done_2,done_1" {
		t.Fatalf("recent done: %v", ids)
	}
	if !list[0].UndoOpen || !list[0].UndoExpiresAt.Equal(open.ExpiresAt) || list[0].UndoID != "undo_2" || list[0].Agent != "ti" {
		t.Fatalf("an open undo: %+v", list[0])
	}
	if list[1].UndoOpen || list[1].UndoID != "" || list[2].UndoOpen {
		t.Fatalf("no undo, and an undone one: %+v %+v", list[1], list[2])
	}
	// Past its window an entry's undo is closed.
	if later, _ := s.RecentDone(ctx, "t1", "u1", now.Add(-time.Hour), now.Add(25*time.Hour)); len(later) != 3 || later[0].UndoOpen {
		t.Fatalf("after the window: %+v", later)
	}
}

// Done entries older than 30 days are swept when the person's next one is saved.
func TestPostgresDoneSweep(t *testing.T) {
	s := testPostgres(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	old := ontology.DoneEntry{ID: "done_old", Tenant: "t1", User: "u1", Action: "a", CreatedAt: now.Add(-31 * 24 * time.Hour)}
	if err := s.SaveDone(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDone(ctx, ontology.DoneEntry{ID: "done_new", Tenant: "t1", User: "u1", Action: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM "+s.Schema+".ontology_done WHERE id='done_old'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("the old entry is kept: %d %v", n, err)
	}
}
