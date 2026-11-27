package proposals_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xiant10/t10-modkit/ontology"
	"github.com/xiant10/t10-modkit/ontology/proposals"
	"github.com/xiant10/t10-modkit/pgdb"
)

func testPostgres(t *testing.T) proposals.Postgres {
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
	drop := func() { _, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS ontology_proposals_test CASCADE") }
	drop()
	t.Cleanup(drop)
	if err := pgdb.Migrate(ctx, pool, proposals.Migrations("ontology_proposals_test", 9_100_001)); err != nil {
		t.Fatal(err)
	}
	// A second run applies nothing again.
	if err := pgdb.Migrate(ctx, pool, proposals.Migrations("ontology_proposals_test", 9_100_001)); err != nil {
		t.Fatal(err)
	}
	return proposals.Postgres{Pool: pool, Schema: "ontology_proposals_test"}
}

func TestMemory(t *testing.T) { exercise(t, &proposals.Memory{}) }

func TestPostgres(t *testing.T) { exercise(t, testPostgres(t)) }

// exercise holds every store to the same rules.
func exercise(t *testing.T, s ontology.ProposalStore) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	draft := func(id, user string, at time.Time) ontology.Proposal {
		return ontology.Proposal{ID: id, Tenant: "t1", User: user, Agent: "agent:a", Action: "ticket.send", Object: "sales:deal:d1", Revision: 3,
			Params: []byte(`{"title":"x"}`), State: "open", CreatedAt: at, ExpiresAt: at.Add(ontology.ProposalLife)}
	}
	// An old draft has expired, so it neither lists nor counts toward the limit.
	if err := s.CreateProposal(ctx, draft("p_old", "u1", now.Add(-8*24*time.Hour)), 2, now); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"p_1", "p_2"} {
		if err := s.CreateProposal(ctx, draft(id, "u1", now.Add(time.Duration(i)*time.Minute)), 2, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateProposal(ctx, draft("p_3", "u1", now), 2, now); !errors.Is(err, ontology.ErrTooManyProposals) {
		t.Fatalf("over the limit: %v", err)
	}
	// Another person's limit is their own.
	if err := s.CreateProposal(ctx, draft("p_other", "u2", now), 2, now); err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenProposals(ctx, "t1", "u1", now)
	if err != nil || len(open) != 2 || open[0].ID != "p_2" || open[1].ID != "p_1" || string(open[0].Params) != `{"title": "x"}` && string(open[0].Params) != `{"title":"x"}` {
		t.Fatalf("open, newest first: %+v %v", open, err)
	}
	if _, err := s.GetProposal(ctx, "t1", "u2", "p_1"); !errors.Is(err, ontology.ErrNotFound) {
		t.Fatalf("someone else's draft: %v", err)
	}
	if _, err := s.GetProposal(ctx, "t2", "u1", "p_1"); !errors.Is(err, ontology.ErrNotFound) {
		t.Fatalf("another tenant's draft: %v", err)
	}
	if err := s.DecideProposal(ctx, "t1", "u1", "p_1", "open", "confirmed"); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideProposal(ctx, "t1", "u1", "p_1", "open", "dismissed"); !errors.Is(err, ontology.ErrProposalClosed) {
		t.Fatalf("deciding twice: %v", err)
	}
	if err := s.DecideProposal(ctx, "t1", "u1", "p_nope", "open", "dismissed"); !errors.Is(err, ontology.ErrNotFound) {
		t.Fatalf("an unknown draft: %v", err)
	}
	// A failed confirm reopens it.
	if err := s.DecideProposal(ctx, "t1", "u1", "p_1", "confirmed", "open"); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetProposal(ctx, "t1", "u1", "p_1")
	if err != nil || p.State != "open" || p.Agent != "agent:a" || p.Object != "sales:deal:d1" || p.Revision != 3 {
		t.Fatalf("reopened: %+v %v", p, err)
	}
}
