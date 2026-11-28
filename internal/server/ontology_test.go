package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// The ontology over collab: the same policy as the routes, items' standing,
// item types as the tenant layer, and actions with their tiers.

func ref(id string) string { return "collab:work_item:" + id }

// object reads one object as a person, and answers its status and body.
func (h *harness) object(user, typ, id string) (int, map[string]any) {
	h.t.Helper()
	return h.as(user, "GET", "/v1/objects/"+typ+"/"+id, nil)
}

// search answers every object a person's search finds, page by page, and the
// number of pages.
func (h *harness) search(user, typ string, body map[string]any) ([]map[string]any, int) {
	h.t.Helper()
	var all []map[string]any
	pages := 0
	for cursor := ""; ; {
		if cursor != "" {
			body["cursor"] = cursor
		}
		out := h.want(user, "POST", "/v1/objects/"+typ+"/search", body, http.StatusOK)
		pages++
		for _, o := range out["objects"].([]any) {
			all = append(all, o.(map[string]any))
		}
		next, _ := out["next_cursor"].(string)
		if next == "" {
			return all, pages
		}
		cursor = next
	}
}

func refs(objects []map[string]any) []string {
	out := []string{}
	for _, o := range objects {
		out = append(out, o["ref"].(string))
	}
	return out
}

// apply takes an action as a person, with an idempotency key when given one.
func (h *harness) apply(user, action string, body map[string]any, key string) (int, map[string]any) {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", h.url+"/v1/actions/"+action+"/apply", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+acme+"/"+user)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func props(o map[string]any) map[string]any { return o["properties"].(map[string]any) }

func actionState(o map[string]any, action string) (bool, string) {
	for _, raw := range o["actions"].([]any) {
		a := raw.(map[string]any)
		if a["action"] == action {
			reason, _ := a["reason"].(string)
			return a["allowed"] == true, reason
		}
	}
	return false, "missing"
}

func TestAnObjectIsWhatItsRouteAnswers(t *testing.T) {
	h := newHarness(t)
	id := h.want("ada", "POST", "/v1/work-items", map[string]any{"title": "Quote", "description": "For Contoso", "priority": "high", "due_at": "2030-01-02T03:04:05Z"}, http.StatusCreated)["id"].(string)
	h.want("ada", "POST", "/v1/work-items/"+id+"/comments", map[string]any{"body": "Started"}, http.StatusCreated)
	route := h.want("ada", "GET", "/v1/work-items/"+id, nil, http.StatusOK)
	status, o := h.object("ada", "work_item", id)
	if status != http.StatusOK {
		t.Fatalf("get = %d %v", status, o)
	}
	p := props(o)
	if o["ref"] != ref(id) || o["revision"] != route["version"] || p["title"] != route["title"] || p["description"] != route["description"] ||
		p["status"] != route["status"] || p["priority"] != route["priority"] || p["creator"] != "auth:user:ada" {
		t.Errorf("object %v\nroute %v", o, route)
	}
	if due, _ := time.Parse(time.RFC3339, p["deadline"].(string)); !due.Equal(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("deadline = %v", p["deadline"])
	}
	if p["relation"] != "owner" || p["comment_count"] != float64(1) || p["pending_offers"] != float64(0) || p["type"] != "collab.task" {
		t.Errorf("standing = %v", p)
	}
	owners := h.want("ada", "GET", "/v1/objects/work_item/"+id+"/links/owners", nil, http.StatusOK)["objects"].([]any)
	if len(owners) != 1 || owners[0].(map[string]any)["ref"] != "auth:user:ada" {
		t.Errorf("owners = %v", owners)
	}
	comments := h.want("ada", "GET", "/v1/objects/work_item/"+id+"/links/comments", nil, http.StatusOK)["objects"].([]any)
	if len(comments) != 1 || props(comments[0].(map[string]any))["body"] != "Started" {
		t.Errorf("comments = %v", comments)
	}
}

func TestAStrangerFindsNothing(t *testing.T) {
	h := newHarness(t)
	id := h.item("ada")
	h.want("ada", "POST", "/v1/work-items/"+id+"/comments", map[string]any{"body": "Mine"}, http.StatusCreated)
	if status, _ := h.object("bob", "work_item", id); status != http.StatusNotFound {
		t.Errorf("bob's get = %d", status)
	}
	resolved := h.want("bob", "POST", "/v1/objects/resolve", map[string]any{"refs": []string{ref(id)}}, http.StatusOK)
	if got := resolved["objects"].([]any); len(got) != 0 {
		t.Errorf("bob resolved %v", got)
	}
	for _, typ := range []string{"work_item", "comment", "offer"} {
		if found, _ := h.search("bob", typ, map[string]any{}); len(found) != 0 {
			t.Errorf("bob's %s search = %v", typ, found)
		}
	}
	if status, _ := h.request(globex+"/gus", "GET", "/v1/objects/work_item/"+id, nil); status != http.StatusNotFound {
		t.Errorf("another tenant's member = %d", status)
	}
}

func TestAParticipantsItemResolvesForItsTwoPeople(t *testing.T) {
	h := newHarness(t)
	h.types()
	h.setRole("eve", "admin")
	h.want("eve", "PUT", "/v1/admin/settings", map[string]string{"manager_visibility": "all_reports"}, http.StatusOK)
	ask := h.ask("ada")
	h.send("ada", ask, "bob", "transfer")
	for user, want := range map[string]int{"ada": 1, "bob": 1, "carol": 0, "dan": 0} {
		got := h.want(user, "POST", "/v1/objects/resolve", map[string]any{"refs": []string{ref(ask)}}, http.StatusOK)["objects"].([]any)
		if len(got) != want {
			t.Errorf("%s resolved %v", user, got)
		}
	}
	if found, _ := h.search("carol", "work_item", map[string]any{}); len(found) != 0 {
		t.Errorf("a manager found a participants item: %v", refs(found))
	}
}

func TestAManagerFindsWhatTheirReportsOwn(t *testing.T) {
	h := newHarness(t)
	h.org.direct[acme+"/carol"] = []string{"bob"}
	h.setRole("eve", "admin")
	h.want("eve", "PUT", "/v1/admin/settings", map[string]string{"manager_visibility": "direct_reports"}, http.StatusOK)
	owned := h.item("bob")
	followed := h.item("dan")
	h.want("dan", "PUT", "/v1/work-items/"+followed+"/collaborators/bob", nil, http.StatusNoContent)
	found, _ := h.search("carol", "work_item", map[string]any{})
	if got := refs(found); !slices.Equal(got, []string{ref(owned)}) {
		t.Fatalf("carol found %v", got)
	}
	if p := props(found[0]); p["relation"] != "manager" {
		t.Errorf("relation = %v", p["relation"])
	}
	// A manager's read is read-only.
	if allowed, reason := actionState(found[0], "work_item.edit"); allowed || reason != "forbidden" {
		t.Errorf("edit = %v %s", allowed, reason)
	}
	h.wantError("carol", "PATCH", "/v1/work-items/"+owned, map[string]any{"version": 1, "title": "Mine"}, http.StatusNotFound, "not_found")
}

func TestMyOverdueTasksPage(t *testing.T) {
	h := newHarness(t)
	past := time.Now().Add(-48 * time.Hour).UTC()
	var mine []string
	for i := range 5 {
		due := past.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		mine = append(mine, ref(h.want("ada", "POST", "/v1/work-items", map[string]any{"title": "Late", "due_at": due}, http.StatusCreated)["id"].(string)))
	}
	// Due later, not owned, and with no due date: none of them is overdue for ada.
	h.want("ada", "POST", "/v1/work-items", map[string]any{"title": "Later", "due_at": time.Now().Add(48 * time.Hour).Format(time.RFC3339)}, http.StatusCreated)
	h.item("ada")
	followed := h.want("bob", "POST", "/v1/work-items", map[string]any{"title": "Bob's", "due_at": past.Format(time.RFC3339)}, http.StatusCreated)["id"].(string)
	h.want("bob", "PUT", "/v1/work-items/"+followed+"/collaborators/ada", nil, http.StatusNoContent)

	// overdue says the same as a deadline before now, for a client with no clock.
	overdue, _ := h.search("ada", "work_item", map[string]any{"filter": map[string]any{"and": []any{
		map[string]any{"property": "relation", "op": "eq", "value": "owner"},
		map[string]any{"property": "overdue", "op": "eq", "value": true},
	}}})
	if len(overdue) != len(mine) {
		t.Errorf("overdue = %v", refs(overdue))
	}
	soon, _ := h.search("ada", "work_item", map[string]any{"filter": map[string]any{"property": "due_soon", "op": "eq", "value": true}})
	if len(soon) != 0 {
		t.Errorf("due soon = %v", refs(soon))
	}
	for _, sort := range []map[string]any{nil, {"property": "deadline"}, {"property": "deadline", "desc": true}, {"property": "title"}} {
		body := map[string]any{"limit": 2, "filter": map[string]any{"and": []any{
			map[string]any{"property": "relation", "op": "eq", "value": "owner"},
			map[string]any{"property": "deadline", "op": "lt", "value": time.Now().UTC().Format(time.RFC3339)},
		}}}
		if sort != nil {
			body["sort"] = sort
		}
		found, pages := h.search("ada", "work_item", body)
		for _, o := range found {
			if props(o)["overdue"] != true || props(o)["due_soon"] != false {
				t.Errorf("%v isn't overdue", props(o))
			}
		}
		got := refs(found)
		slices.Sort(got)
		want := slices.Clone(mine)
		slices.Sort(want)
		if !slices.Equal(got, want) || pages != 3 {
			t.Errorf("sort %v: %d pages of %v, want %v", sort, pages, got, want)
		}
	}
}

func TestItemTypesAreTheTenantLayer(t *testing.T) {
	h := newHarness(t)
	core := h.want("ada", "GET", "/v1/ontology", nil, http.StatusOK)
	h.types()
	d := h.want("ada", "GET", "/v1/ontology", nil, http.StatusOK)
	layer := d["tenant"].(map[string]any)
	item := layer["object_types"].(map[string]any)["work_item"].(map[string]any)
	values := item["values"].(map[string]any)
	if !slices.Contains(strs(values["type"]), "sales.presales") || !slices.Contains(strs(values["state"]), "delivered") {
		t.Errorf("values = %v", values)
	}
	properties := item["properties"].(map[string]any)
	if p, _ := properties["sales_presales__when"].(map[string]any); p["type"] != "text" || p["name"] != "When" {
		t.Errorf("when = %v", properties["sales_presales__when"])
	}
	if p, _ := properties["sales_presales__for_whom"].(map[string]any); p["type"] != "enum" || !slices.Equal(strs(p["values"]), []string{"buyer", "boss"}) {
		t.Errorf("for_whom = %v", properties["sales_presales__for_whom"])
	}
	if p, _ := properties["property_followup__rooms"].(map[string]any); p["type"] != "integer" {
		t.Errorf("rooms = %v", properties["property_followup__rooms"])
	}
	moves := layer["action_types"].(map[string]any)["work_item.move"].(map[string]any)["values"].(map[string]any)["move"]
	if !slices.Contains(strs(moves), "deliver") {
		t.Errorf("moves = %v", moves)
	}
	// The core is the same with or without types.
	for _, key := range []string{"object_types", "action_types", "version"} {
		a, _ := json.Marshal(core[key])
		b, _ := json.Marshal(d[key])
		if !bytes.Equal(a, b) {
			t.Errorf("the core's %s changed", key)
		}
	}
	// A tenant version that adds a field changes the layer and its revision.
	before := layer["revision"].(float64)
	tailored := with(presales, map[string]any{"fields": map[string]any{"type": "object", "properties": map[string]any{
		"when":     map[string]any{"type": "string", "maxLength": 200, "title": "When"},
		"what":     map[string]any{"type": "string", "maxLength": 2000},
		"for_whom": map[string]any{"type": "string", "enum": []any{"buyer", "boss"}},
		"budget":   map[string]any{"type": "number"},
	}}})
	raw, _ := json.Marshal(tailored)
	req, _ := http.NewRequest("PUT", h.url+"/internal/v1/types/sales.presales/tenants/"+acme, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer t10-sales")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tailor = %d", resp.StatusCode)
	}
	after := h.want("ada", "GET", "/v1/ontology", nil, http.StatusOK)["tenant"].(map[string]any)
	added, _ := after["object_types"].(map[string]any)["work_item"].(map[string]any)["properties"].(map[string]any)["sales_presales__budget"].(map[string]any)
	if after["revision"].(float64) <= before || added["type"] != "decimal" {
		t.Errorf("revision %v then %v, budget %v", before, after["revision"], added)
	}
	// Another tenant has the default, without this tenant's field.
	_, other := h.request(globex+"/gus", "GET", "/v1/ontology", nil)
	if _, ok := other["tenant"].(map[string]any)["object_types"].(map[string]any)["work_item"].(map[string]any)["properties"].(map[string]any)["sales_presales__budget"]; ok {
		t.Error("another tenant sees this tenant's field")
	}
}

func TestAnAssistantDraftsASendThePersonConfirms(t *testing.T) {
	h := newHarness(t)
	ts := h.mcpServer()
	id := h.item("ada")
	drafted := call(t, ts.URL, "agent-assistant", "work_propose", map[string]any{
		"action": "work_item.send", "object": ref(id), "params": map[string]any{"to": "auth:user:bob", "note": "You know Contoso"},
	})
	if drafted.failed {
		t.Fatalf("draft = %+v", drafted)
	}
	if sent := h.want("ada", "GET", "/v1/offers?box=sent", nil, http.StatusOK)["offers"].([]any); len(sent) != 0 {
		t.Fatalf("a draft sent: %v", sent)
	}
	proposals := h.want("ada", "GET", "/v1/proposals", nil, http.StatusOK)["proposals"].([]any)
	if len(proposals) != 1 {
		t.Fatalf("proposals = %v", proposals)
	}
	p := proposals[0].(map[string]any)
	h.want("ada", "POST", "/v1/proposals/"+p["id"].(string)+"/confirm", map[string]any{}, http.StatusOK)
	sent := h.want("ada", "GET", "/v1/offers?box=sent", nil, http.StatusOK)["offers"].([]any)
	if len(sent) != 1 || sent[0].(map[string]any)["to_user_id"] != "bob" || sent[0].(map[string]any)["note"] != "You know Contoso" {
		t.Errorf("sent = %v", sent)
	}
	// It's an offer bob answers, not an action anyone can draft.
	if got := call(t, ts.URL, "agent-assistant", "work_propose", map[string]any{"action": "offer.accept", "object": "collab:offer:" + sent[0].(map[string]any)["id"].(string)}); !got.failed {
		t.Errorf("an accept was drafted: %+v", got)
	}
}

// The person let their assistant act without asking (work:apply): a send goes
// out with no draft, and the item's history says how.
func TestAnAssistantSendsWhenThePersonLetsIt(t *testing.T) {
	h := newHarness(t)
	ts := h.mcpServer()
	id := h.item("ada")
	send := func(token, request string) toolReply {
		return call(t, ts.URL, token, "work_act", map[string]any{
			"action": "work_item.send", "object": ref(id), "revision": 1,
			"params": map[string]any{"to": "auth:user:bob", "note": "You know Contoso"}, "request_id": request,
		})
	}
	if got := send("agent-assistant", "req-send-01"); !got.failed || !strings.HasPrefix(got.text, "not_allowed_for_agents") {
		t.Fatalf("a send without work:apply = %+v", got)
	}
	described := call(t, ts.URL, "agent-assistant", "work_describe", map[string]any{})
	if a := described.data["action_types"].(map[string]any)["work_item.send"].(map[string]any); a["agent_applies"] != false {
		t.Errorf("work_item.send without work:apply: %v", a)
	}
	described = call(t, ts.URL, "agent-apply", "work_describe", map[string]any{})
	if a := described.data["action_types"].(map[string]any)["work_item.send"].(map[string]any); a["agent_applies"] != true {
		t.Errorf("work_item.send with work:apply: %v", a)
	}
	sent := send("agent-apply", "req-send-02")
	if sent.failed || sent.data["undo"] != nil {
		t.Fatalf("a send with work:apply = %+v", sent)
	}
	offers := h.want("ada", "GET", "/v1/offers?box=sent", nil, http.StatusOK)["offers"].([]any)
	if len(offers) != 1 || offers[0].(map[string]any)["to_user_id"] != "bob" {
		t.Fatalf("sent = %v", offers)
	}
	if p := h.want("ada", "GET", "/v1/proposals", nil, http.StatusOK)["proposals"].([]any); len(p) != 0 {
		t.Errorf("a draft was stored: %v", p)
	}
	// A retried comment posts once, and it says it was applied without asking.
	for range 2 {
		if got := call(t, ts.URL, "agent-apply", "work_act", map[string]any{
			"action": "work_item.comment", "object": ref(id), "revision": 1, "params": map[string]any{"body": "Sent to Bob"}, "request_id": "req-comment-01",
		}); got.failed {
			t.Fatalf("a comment with work:apply = %+v", got)
		}
	}
	events := h.want("ada", "GET", "/v1/work-items/"+id+"/events", nil, http.StatusOK)["events"].([]any)
	comments, marked := 0, 0
	for _, raw := range events {
		e := raw.(map[string]any)
		detail, _ := e["detail"].(map[string]any)
		if e["kind"] == "comment.added" {
			comments++
		}
		if detail["auto_apply"] == true {
			marked++
		}
	}
	if comments != 1 || marked < 2 {
		t.Errorf("events: %d comments, %d marked auto_apply: %v", comments, marked, events)
	}
	// The person sees both in what was done for them, without Undo.
	done := h.want("ada", "GET", "/v1/done", nil, http.StatusOK)["done"].([]any)
	if len(done) != 2 || done[0].(map[string]any)["action"] != "work_item.comment" || done[0].(map[string]any)["undo_id"] != nil {
		t.Errorf("done = %v", done)
	}
}

func TestADraftOnAChangedItemIsStale(t *testing.T) {
	h := newHarness(t)
	ts := h.mcpServer()
	id := h.item("ada")
	if r := call(t, ts.URL, "agent-assistant", "work_propose", map[string]any{"action": "work_item.edit", "object": ref(id), "params": map[string]any{"title": "Quote for Contoso"}}); r.failed {
		t.Fatalf("draft = %+v", r)
	}
	h.want("ada", "PATCH", "/v1/work-items/"+id, map[string]any{"version": 1, "priority": "high"}, http.StatusOK)
	p := h.want("ada", "GET", "/v1/proposals", nil, http.StatusOK)["proposals"].([]any)[0].(map[string]any)
	path := "/v1/proposals/" + p["id"].(string) + "/confirm"
	h.wantError("ada", "POST", path, map[string]any{}, http.StatusConflict, "stale_revision")
	// Confirmed anyway, with the revision the person now sees.
	h.want("ada", "POST", path, map[string]any{"revision": 2}, http.StatusOK)
	if got := h.want("ada", "GET", "/v1/work-items/"+id, nil, http.StatusOK); got["title"] != "Quote for Contoso" || got["priority"] != "high" {
		t.Errorf("item = %v", got)
	}
}

func TestAnAssistantsTaskCanBeUndone(t *testing.T) {
	h := newHarness(t)
	ts := h.mcpServer()
	made := call(t, ts.URL, "agent-assistant", "work_act", map[string]any{
		"action": "work_item.create", "params": map[string]any{"title": "Call Contoso"}, "request_id": "req-undo-01",
	})
	if made.failed {
		t.Fatalf("act = %+v", made)
	}
	undo := made.data["undo"].(map[string]any)["id"].(string)
	id := strings.TrimPrefix(made.data["object"].(map[string]any)["ref"].(string), "collab:work_item:")
	h.wantError("bob", "POST", "/v1/undo/"+undo, map[string]any{}, http.StatusNotFound, "not_found")
	h.want("ada", "POST", "/v1/undo/"+undo, map[string]any{}, http.StatusOK)
	h.wantError("ada", "GET", "/v1/work-items/"+id, nil, http.StatusNotFound, "not_found")
	if status, _ := h.as("ada", "POST", "/v1/undo/"+undo, map[string]any{}); status != http.StatusConflict {
		t.Errorf("a second undo = %d", status)
	}

	// Once someone else is on it, it stays.
	for name, after := range map[string]func(id string){
		"sent": func(id string) { h.send("ada", id, "bob", "transfer") },
		"followed": func(id string) {
			h.want("ada", "PUT", "/v1/work-items/"+id+"/collaborators/bob", nil, http.StatusNoContent)
		},
		"listed": func(id string) {
			h.want("ada", "PUT", "/v1/work-items/"+id+"/list", map[string]any{"list_id": h.list("ada")}, http.StatusOK)
		},
	} {
		status, out := h.apply("ada", "work_item.create", map[string]any{"params": map[string]any{"title": name}}, "key-"+name)
		if status != http.StatusOK {
			t.Fatalf("%s: apply = %d %v", name, status, out)
		}
		after(strings.TrimPrefix(out["object"].(map[string]any)["ref"].(string), "collab:work_item:"))
		h.wantError("ada", "POST", "/v1/undo/"+out["undo"].(map[string]any)["id"].(string), map[string]any{}, http.StatusConflict, "undo_conflict")
	}
}

// Each action's availability agrees with its route on a refused case.
func TestActionsAgreeWithTheirRoutes(t *testing.T) {
	h := newHarness(t)
	h.types()
	id := h.item("ada")
	h.want("ada", "PUT", "/v1/work-items/"+id+"/collaborators/bob", nil, http.StatusNoContent)
	_, o := h.object("bob", "work_item", id)
	// A collaborator edits but doesn't send or cancel.
	if allowed, _ := actionState(o, "work_item.edit"); !allowed {
		t.Error("bob can't edit")
	}
	if allowed, reason := actionState(o, "work_item.send"); allowed || reason != "forbidden" {
		t.Errorf("send = %v %s", allowed, reason)
	}
	h.wantError("bob", "POST", "/v1/work-items/"+id+"/send", map[string]any{"to": "dan"}, http.StatusForbidden, "forbidden")
	h.wantError("bob", "POST", "/v1/actions/work_item.set_status/apply", map[string]any{"object": ref(id), "revision": 1, "params": map[string]any{"to": "cancelled"}}, http.StatusForbidden, "forbidden")
	h.wantError("bob", "PATCH", "/v1/work-items/"+id, map[string]any{"version": 1, "status": "cancelled"}, http.StatusForbidden, "forbidden")
	// A closed item isn't sent.
	h.want("ada", "PATCH", "/v1/work-items/"+id, map[string]any{"version": 1, "status": "done"}, http.StatusOK)
	_, o = h.object("ada", "work_item", id)
	if allowed, reason := actionState(o, "work_item.send"); allowed || reason != "wrong_state" {
		t.Errorf("send on a done item = %v %s", allowed, reason)
	}
	h.wantError("ada", "POST", "/v1/work-items/"+id+"/send", map[string]any{"to": "bob"}, http.StatusConflict, "item_closed")
	// A typed item moves by its steps, not its status.
	ask := h.ask("ada")
	_, o = h.object("ada", "work_item", ask)
	if allowed, reason := actionState(o, "work_item.set_status"); allowed || reason != "wrong_state" {
		t.Errorf("set_status on a typed item = %v %s", allowed, reason)
	}
	if allowed, _ := actionState(o, "work_item.move"); !allowed {
		t.Error("ada can't cancel her ask")
	}
	h.wantError("ada", "POST", "/v1/actions/work_item.move/apply", map[string]any{"object": ref(ask), "revision": 1, "params": map[string]any{"move": "deliver"}}, http.StatusBadRequest, "invalid_params")
	h.want("ada", "POST", "/v1/actions/work_item.move/apply", map[string]any{"object": ref(ask), "revision": 1, "params": map[string]any{"move": "cancel"}}, http.StatusOK)
	h.wantState("ada", ask, "cancelled", "cancelled")
}

// docs/ontology.json pins the core descriptor, which every tenant shares.
// UPDATE_ONTOLOGY=1 writes it.
func TestTheCoreDescriptorIsPinned(t *testing.T) {
	got, err := json.MarshalIndent(New(Deps{}).ontology.Registry().Core(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("UPDATE_ONTOLOGY") != "" {
		if err := os.WriteFile("../../docs/ontology.json", got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("../../docs/ontology.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("docs/ontology.json differs from the core: a change to the core raises ontology.Version, then UPDATE_ONTOLOGY=1 go test")
	}
}
