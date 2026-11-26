// Command stub serves ledgermod over HTTP, with a property-shaped work_order
// type beside it, for the workspace's contracts/ontology/conformance.ts. Its
// -break flag breaks one rule of the contract, so the suite's self-test can
// show that it fails for each.
//
//	go run ./ontology/testdata/ledgermod/stub -addr 127.0.0.1:18300 [-break scope]
//
// Its tokens are ledgermod's: person:t1:rep1 sees building 12, person:t1:rep2
// building 3, and agent:t1:rep1 is a delegated token.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"time"

	"github.com/xiant10/t10-modkit/ontology"
	"github.com/xiant10/t10-modkit/ontology/testdata/ledgermod"
)

// The rules -break can break, each caught by one of the suite's checks.
var breaks = []string{"scope", "paging", "hidden", "unknown-type", "resolve", "delegated", "forbidden", "wrong-state", "stale",
	"replay", "undo-owner", "undo-conflict", "tier", "undoable", "tenant-action", "label", "events", "interface"}

type order struct {
	ID, Building, State, Summary string
	Deadline                     time.Time
	Revision                     int64
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18300", "where to listen")
	broken := flag.String("break", "", "the rule to break: "+strings.Join(breaks, ", "))
	flag.Parse()
	if *broken != "" && !slices.Contains(breaks, *broken) {
		log.Fatalf("unknown -break %q", *broken)
	}
	is := func(name string) bool { return *broken == name }

	m := ledgermod.New(time.Now)
	m.SetPack("t1", ledgermod.Pack{Version: 3, Stages: []string{"线索", "商机", "在跟"}, Roles: []string{"决策人"}, Kinds: []string{"拜访", "情报收集"},
		Fields: []ledgermod.Field{{Key: "F", Name: "Family", NameZh: "家庭", Sensitive: true}}})
	rep := ledgermod.Rep("t1", "rep1")
	account := m.AddAccount(rep, "Example Works")
	m.AddDeal(rep, account, "Line 2 upgrade", 1_000_00, "")
	m.AddDeal(ledgermod.Rep("t1", "rep2"), m.AddAccount(ledgermod.Rep("t1", "rep2"), "Rival"), "Hidden", 1, "")

	// 40 overdue work orders, 3 of them in building 12, and one completed there.
	var orders []*order
	for i := range 40 {
		building := "3"
		if i%13 == 1 {
			building = "12"
		}
		orders = append(orders, &order{ID: fmt.Sprintf("wo_%02d", i), Building: building, State: []string{"pending", "processing"}[i%2],
			Summary: fmt.Sprintf("Order %d", i), Deadline: time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC), Revision: 1})
	}
	orders = append(orders, &order{ID: "wo_done", Building: "12", State: "completed", Summary: "Done", Deadline: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Revision: 1})
	scope := map[string]string{"rep1": "12", "rep2": "3"}
	sees := func(c ledgermod.Caller, o *order) bool { return is("hidden") || scope[c.User] == o.Building }
	facts := func(o *order) map[string]any {
		return map[string]any{"summary": o.Summary, "state": o.State, "building": o.Building, "deadline": o.Deadline.Format(time.RFC3339), "assignee": "auth:user:" + o.Building}
	}
	ontology.AddObject(m.Registry, ontology.ObjectType[ledgermod.Caller, *order]{
		Name: "work_order", Label: "Work order", LabelZh: "工单", Title: "summary", Implements: []string{"t10:Assignable", "t10:HasDeadline"},
		Properties: map[string]ontology.Property{
			"summary":  {Type: ontology.String},
			"state":    {Type: ontology.Enum, Values: []string{"pending", "processing", "completed"}},
			"building": {Type: ontology.String},
			"deadline": {Type: ontology.Timestamp},
			"assignee": {Type: ontology.Ref, To: "auth:user"},
		},
		Search: func(_ context.Context, c ledgermod.Caller, q ontology.Query) ([]*order, error) {
			var rows []*order
			for _, o := range orders {
				if (is("scope") || is("paging") || sees(c, o)) && (q.Filter == nil || matches(*q.Filter, o)) {
					rows = append(rows, o)
				}
			}
			slices.SortFunc(rows, func(a, b *order) int { return cmp.Or(a.Deadline.Compare(b.Deadline), cmp.Compare(a.ID, b.ID)) })
			if q.After != nil {
				i := slices.IndexFunc(rows, func(o *order) bool {
					at, _ := q.After.Value.(time.Time)
					return o.Deadline.After(at) || (o.Deadline.Equal(at) && o.ID > q.After.ID)
				})
				if i < 0 {
					i = len(rows)
				}
				rows = rows[i:]
			}
			if len(rows) > q.Limit+1 {
				rows = rows[:q.Limit+1]
			}
			if is("paging") {
				// Scope after paging: a page comes back short.
				rows = slices.DeleteFunc(rows, func(o *order) bool { return !sees(c, o) })
			}
			return rows, nil
		},
		DefaultSort: ontology.Sort{Property: "deadline"},
		Get: func(_ context.Context, c ledgermod.Caller, id string) (*order, error) {
			for _, o := range orders {
				if o.ID == id && sees(c, o) {
					return o, nil
				}
			}
			return nil, ontology.ErrNotFound
		},
		View: func(_ context.Context, _ ledgermod.Caller, o *order) ontology.Object {
			return ontology.Object{ID: o.ID, Title: o.Summary, Revision: o.Revision, Properties: facts(o)}
		},
	})
	ontology.AddAction(m.Registry, ontology.ActionType[ledgermod.Caller, *order]{
		Name: "work_order.accept", Label: "Accept", LabelZh: "受理", On: "work_order", Permission: "sales:deal.update", Agent: ontology.Propose,
		Params: map[string]ontology.Param{"note": {Type: ontology.Text}},
		Available: func(_ context.Context, _ ledgermod.Caller, o *order) string {
			if o.State != "pending" && !is("wrong-state") {
				return "wrong_state"
			}
			return ""
		},
		Apply: func(_ context.Context, _ ledgermod.Caller, o *order, _ ontology.Input) (ontology.Result, error) {
			o.State = "processing"
			o.Revision++
			return ontology.Result{ID: o.ID}, nil
		},
	})
	if err := m.Registry.Check(); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	ontology.Mount(mux, m.Registry.Routes())
	events := map[string]any{"deal.stage_changed": map[string]string{"label": "Stage changed", "label_zh": "阶段变更"}}
	if is("events") {
		events = map[string]any{}
	}
	manifest, _ := json.Marshal(map[string]any{"id": "sales", "api_version": 1, "action_prefix": "sales", "enabled": true, "features": []string{"ontology"}, "events": events})
	mux.HandleFunc("GET /v1/module", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(manifest)
	})
	log.Printf("ontology stub on %s, break %q", *addr, *broken)
	log.Fatal(http.ListenAndServe(*addr, breaking(*broken, mux)))
}

func matches(f ontology.Filter, o *order) bool {
	if f.And != nil {
		for _, sub := range f.And {
			if !matches(sub, o) {
				return false
			}
		}
		return true
	}
	switch f.Property {
	case "state":
		values, _ := f.Value.([]any)
		if f.Op == ontology.Eq {
			values = []any{f.Value}
		}
		return slices.Contains(values, any(o.State))
	case "deadline":
		at, _ := f.Value.(time.Time)
		return (f.Op == ontology.Lt && o.Deadline.Before(at)) || (f.Op == ontology.Gt && o.Deadline.After(at))
	case "building":
		return f.Op == ontology.Eq && f.Value == o.Building
	}
	return true
}

// breaking breaks the rules a module's own functions can't, by changing what
// the routes answer.
func breaking(rule string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rule == "delegated" {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "Bearer agent:", "Bearer person:", 1))
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		status, body := rec.Code, rec.Body.Bytes()
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		edit := func(f func()) {
			if v != nil {
				f()
				body, _ = json.Marshal(v)
			}
		}
		path := r.URL.Path
		switch {
		case rule == "unknown-type" && v["error"] == "unknown_type":
			status, body = 200, []byte(`{"objects":[]}`)
		case rule == "resolve" && path == "/v1/objects/resolve":
			edit(func() {
				v["objects"] = append(v["objects"].([]any), map[string]any{"ref": "sales:deal:deal_0004", "type": "deal", "title": "Hidden", "actions": []any{}})
			})
		case rule == "forbidden" && status == 403:
			status = 404
		case rule == "stale" && v["error"] == "stale_revision":
			status, body = 200, []byte(`{"object":null}`)
		case rule == "replay" && strings.HasSuffix(path, "/apply") && v["undo"] != nil:
			edit(func() { v["undo"].(map[string]any)["id"] = fmt.Sprintf("undo_%d", time.Now().UnixNano()) })
		case rule == "undo-owner" && strings.HasPrefix(path, "/v1/undo/") && status == 404:
			status, body = 200, []byte(`{"object":null}`)
		case rule == "undo-conflict" && v["error"] == "undo_conflict":
			status, body = 200, []byte(`{"object":null}`)
		case path == "/v1/ontology" && status == 200:
			edit(func() {
				actions := v["action_types"].(map[string]any)
				switch rule {
				case "tier":
					actions["deal.set_discount"].(map[string]any)["agent"] = "propose"
				case "undoable":
					actions["record.add"].(map[string]any)["undo"] = false
					delete(actions["record.add"].(map[string]any), "undo_window")
				case "tenant-action":
					v["tenant"].(map[string]any)["action_types"].(map[string]any)["deal.close"] = map[string]any{"values": map[string]any{}}
				case "label":
					delete(actions["record.add"].(map[string]any)["params"].(map[string]any)["kind"].(map[string]any), "label")
				case "interface":
					v["object_types"].(map[string]any)["work_order"].(map[string]any)["properties"].(map[string]any)["deadline"] = map[string]any{"type": "date"}
				}
			})
		}
		for k, vs := range rec.Header() {
			w.Header()[k] = vs
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(status)
		w.Write(bytes.TrimSpace(body))
	})
}
