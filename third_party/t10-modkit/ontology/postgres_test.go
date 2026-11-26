package ontology_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/xiant10/t10-modkit/ontology"
	"github.com/xiant10/t10-modkit/ontology/proposals"
	"github.com/xiant10/t10-modkit/ontology/testdata/ledgermod"
	"github.com/xiant10/t10-modkit/ontology/undo"
	"github.com/xiant10/t10-modkit/pgdb"
)

// TestFlowsOnPostgres runs the proposal and undo rules (expiry, the limit,
// windows, owners, replays) through the Postgres stores.
func TestFlowsOnPostgres(t *testing.T) {
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
	const schema = "ontology_flow_test"
	drop := func() { _, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE") }
	drop()
	t.Cleanup(drop)
	for _, m := range []pgdb.Migrations{proposals.Migrations(schema, 9_100_003), undo.Migrations(schema, 9_100_003)} {
		if err := pgdb.Migrate(ctx, pool, m); err != nil {
			t.Fatal(err)
		}
	}

	clk := &clock{now: time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)}
	m := ledgermod.NewWith(clk.Now, proposals.Postgres{Pool: pool, Schema: schema}, undo.Postgres{Pool: pool, Schema: schema})
	m.SetPack("t1", pack)
	w := &world{t: t, m: m, clock: clk, rep: ledgermod.Rep("t1", "rep1"), rival: ledgermod.Rep("t1", "rep2"), manager: ledgermod.Manager("t1", "boss")}
	w.account = m.AddAccount(w.rep, "Example Works")
	w.deal = m.AddDeal(w.rep, w.account, "Line 2 upgrade", 1, "")
	mux := http.NewServeMux()
	ontology.Mount(mux, m.Registry.Routes())
	w.srv = httptest.NewServer(mux)
	t.Cleanup(w.srv.Close)
	ref := dealRef(w.deal)

	// Replay and undo.
	body := map[string]any{"object": ref, "revision": 1, "params": map[string]any{"kind": "拜访", "summary": "met"}}
	a := w.call("POST", "/v1/actions/record.add/apply", w.as(w.rep), body, "Idempotency-Key", "pg-1")
	b := w.call("POST", "/v1/actions/record.add/apply", w.as(w.rep), body, "Idempotency-Key", "pg-1")
	if get(a.body, "undo", "id") != get(b.body, "undo", "id") || len(m.Records()) != 1 {
		t.Fatalf("replay: %s %s", a.raw, b.raw)
	}
	id := get(a.body, "undo", "id").(string)
	w.call("POST", "/v1/undo/"+id, w.as(w.manager), nil).want(t, 404, "not_found")
	w.call("POST", "/v1/undo/"+id, w.as(w.rep), nil).want(t, 200, "")
	w.call("POST", "/v1/undo/"+id, w.as(w.rep), nil).want(t, 409, "undone")
	c := w.call("POST", "/v1/actions/record.add/apply", w.as(w.rep), map[string]any{"object": ref, "revision": w.revision(w.rep, ref), "params": map[string]any{"kind": "拜访", "summary": "late"}}, "Idempotency-Key", "pg-2")
	clk.add(25 * time.Hour)
	w.call("POST", "/v1/undo/"+get(c.body, "undo", "id").(string), w.as(w.rep), nil).want(t, 409, "undo_expired")

	// Drafts: confirm, someone else's, the limit, expiry.
	out, _ := w.tool("sales_propose", w.rep, map[string]any{"action": "ticket.send", "object": ref, "params": map[string]any{"title": "Quote"}})
	draft := toolData(t, out)["proposal"].(string)
	w.call("POST", "/v1/proposals/"+draft+"/confirm", w.as(w.rival), nil).want(t, 404, "not_found")
	w.call("POST", "/v1/proposals/"+draft+"/confirm", w.as(w.rep), nil).want(t, 200, "")
	w.call("POST", "/v1/proposals/"+draft+"/confirm", w.as(w.rep), nil).want(t, 409, "proposal_closed")
	for range ontology.ProposalLimit {
		w.tool("sales_propose", w.rep, map[string]any{"action": "ticket.send", "object": ref, "params": map[string]any{"title": "t"}})
	}
	if out, isErr := w.tool("sales_propose", w.rep, map[string]any{"action": "ticket.send", "object": ref, "params": map[string]any{"title": "t"}}); !isErr {
		t.Fatalf("over the limit: %v", out)
	}
	list := w.call("GET", "/v1/proposals", w.as(w.rep), nil)
	first := list.body["proposals"].([]any)[0].(map[string]any)["id"].(string)
	clk.add(8 * 24 * time.Hour)
	w.call("POST", "/v1/proposals/"+first+"/confirm", w.as(w.rep), nil).want(t, 409, "proposal_expired")
	if n := len(w.call("GET", "/v1/proposals", w.as(w.rep), nil).body["proposals"].([]any)); n != 0 {
		t.Fatalf("%d expired drafts still listed", n)
	}
}
