package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The two fixture types stand in for real modules: a request only its
// participants see, and a team follow-up about a record.
var (
	presales = map[string]any{
		"id": "sales.presales", "owner_module": "sales", "version": 1, "label": "Presales",
		"fields": map[string]any{"type": "object", "properties": map[string]any{
			"when":     map[string]any{"type": "string", "maxLength": 200, "title": "When"},
			"what":     map[string]any{"type": "string", "maxLength": 2000},
			"for_whom": map[string]any{"type": "string", "enum": []any{"buyer", "boss"}},
		}},
		"subject":    []string{"sales:deal", "sales:meeting"},
		"visibility": "participants", "departure": "cancel",
		// The proposal's lifecycle, as a sales module would declare it.
		"states": map[string]any{
			"draft":     map[string]any{"phase": "todo", "label": "Draft"},
			"sent":      map[string]any{"phase": "todo", "label": "Sent"},
			"accepted":  map[string]any{"phase": "doing", "label": "Accepted"},
			"delivered": map[string]any{"phase": "doing", "label": "Delivered"},
			"reviewed":  map[string]any{"phase": "done", "label": "Reviewed"},
			"declined":  map[string]any{"phase": "cancelled", "label": "Declined"},
			"cancelled": map[string]any{"phase": "cancelled", "label": "Cancelled"},
		},
		"initial": "draft", "cancelled": "cancelled",
		"offers": map[string]any{
			"send":     map[string]any{"from": []string{"draft"}, "to": "sent", "requires": []string{"what", "for_whom"}},
			"accept":   map[string]any{"to": "accepted"},
			"decline":  map[string]any{"to": "declined", "note": "required"},
			"withdraw": map[string]any{"to": "draft"},
		},
		"transitions": []any{
			map[string]any{"id": "deliver", "label": "Deliver", "from": []string{"accepted"}, "to": "delivered", "by": "owner", "payload": []string{"reply", "links"}},
			map[string]any{"id": "review", "from": []string{"delivered"}, "to": "reviewed", "by": "creator"},
			map[string]any{"id": "cancel", "from": []string{"draft", "sent", "accepted"}, "to": "cancelled", "by": "creator"},
		},
	}
	followup = map[string]any{
		"id": "property.followup", "owner_module": "property", "version": 1, "label": "Follow-up",
		"fields": map[string]any{"properties": map[string]any{
			"rooms":  map[string]any{"type": "integer"},
			"urgent": map[string]any{"type": "boolean"},
		}},
		"subject": []string{"property:complaint"},
	}
)

func with(def map[string]any, changes map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range def {
		out[k] = v
	}
	for k, v := range changes {
		out[k] = v
	}
	return out
}

// pushType puts a type's default as a service, and returns the status and error code.
func (h *harness) pushType(service string, def map[string]any) (int, string) {
	h.t.Helper()
	raw, _ := json.Marshal(def)
	req, _ := http.NewRequest("PUT", h.url+"/internal/v1/types/"+def["id"].(string), bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+service)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, &out)
	code, _ := out["error"].(string)
	return resp.StatusCode, code
}

func (h *harness) types() {
	h.t.Helper()
	for service, def := range map[string]map[string]any{"t10-sales": presales, "t10-property": followup} {
		if got, code := h.pushType(service, def); got != http.StatusOK {
			h.t.Fatalf("push %s = %d %s", def["id"], got, code)
		}
	}
}

func (h *harness) typed(user string, body map[string]any) string {
	h.t.Helper()
	return h.want(user, "POST", "/v1/work-items", with(map[string]any{"title": "Ask"}, body), http.StatusCreated)["id"].(string)
}

// ask creates a presales item with what a send requires.
func (h *harness) ask(user string) string {
	h.t.Helper()
	return h.typed(user, map[string]any{"type": "sales.presales", "fields": map[string]any{"what": "A deck", "for_whom": "boss"}})
}

func ids(items any) []string {
	out := []string{}
	for _, raw := range items.([]any) {
		out = append(out, raw.(map[string]any)["id"].(string))
	}
	slices.Sort(out)
	return out
}

func TestOnlyTheOwningModulePushesItsTypes(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		token string
		def   map[string]any
		want  int
		code  string
	}{
		{"", presales, http.StatusUnauthorized, "invalid_token"},
		{acme + "/ada", presales, http.StatusForbidden, "forbidden"},
		{"t10-property", presales, http.StatusForbidden, "forbidden"},
		{"t10-sales", with(presales, map[string]any{"id": "collab.task", "owner_module": "collab"}), http.StatusBadRequest, "invalid_type"},
		{"t10-sales", with(presales, map[string]any{"states": map[string]any{}}), http.StatusBadRequest, "invalid_type"},
		{"t10-sales", with(presales, map[string]any{"departure": "reassign"}), http.StatusBadRequest, "invalid_type"},
		{"t10-sales", with(presales, map[string]any{"visibility": "scoped"}), http.StatusBadRequest, "invalid_type"},
		{"t10-sales", with(presales, map[string]any{"subject": []string{"deal"}}), http.StatusBadRequest, "invalid_type"},
		{"t10-sales", with(presales, map[string]any{"fields": map[string]any{"properties": map[string]any{
			"x": map[string]any{"type": "array"}}}}), http.StatusBadRequest, "invalid_type"},
		{"t10-sales", presales, http.StatusOK, ""},
		{"t10-sales", presales, http.StatusOK, ""}, // Every replica pushes at start.
		{"t10-sales", with(presales, map[string]any{"label": "Other"}), http.StatusConflict, "type_version_stale"},
		{"t10-sales", with(presales, map[string]any{"version": 2, "departure": "creator"}), http.StatusConflict, "type_fixed"},
		{"t10-sales", with(presales, map[string]any{"version": 2, "label": "Presales v2"}), http.StatusOK, ""},
		{"t10-sales", presales, http.StatusConflict, "type_version_stale"},
	} {
		if got, code := h.pushType(c.token, c.def); got != c.want || code != c.code {
			t.Errorf("%s pushing %v: %d %q, want %d %q", c.token, c.def["id"], got, code, c.want, c.code)
		}
	}
	got := []string{}
	for _, raw := range h.want("bob", "GET", "/v1/types", nil, http.StatusOK)["types"].([]any) {
		ty := raw.(map[string]any)
		got = append(got, ty["id"].(string))
		if ty["id"] == "sales.presales" && ty["label"] != "Presales v2" {
			t.Errorf("label = %v", ty["label"])
		}
	}
	if !slices.Equal(got, []string{"collab.task", "sales.presales"}) {
		t.Errorf("types = %v", got)
	}
}

func TestATypedItemChecksItsFields(t *testing.T) {
	h := newHarness(t)
	h.types()
	for _, body := range []map[string]any{
		{"type": "sales.presales", "fields": map[string]any{"when": strings.Repeat("x", 201)}},
		{"type": "sales.presales", "fields": map[string]any{"for_whom": "someone"}},
		{"type": "sales.presales", "fields": map[string]any{"colour": "red"}},
		{"type": "property.followup", "fields": map[string]any{"rooms": 1.5}},
		{"type": "property.followup", "fields": map[string]any{"urgent": "yes"}},
		{"type": "sales.presales", "subject_ref": "property:complaint:c_1"},
		{"type": "sales.presales", "subject_ref": "sales:deal"},
		{"subject_ref": "not a reference"},
	} {
		h.wantError("ada", "POST", "/v1/work-items", with(map[string]any{"title": "Ask"}, body), http.StatusBadRequest, "invalid_request")
	}
	h.wantError("ada", "POST", "/v1/work-items", map[string]any{"title": "Ask", "type": "sales.nothing"}, http.StatusUnprocessableEntity, "unknown_type")

	item := h.want("ada", "POST", "/v1/work-items", map[string]any{
		"title": "Deck for Acme", "type": "sales.presales", "subject_ref": "sales:deal:d_12",
		"fields": map[string]any{"when": "Friday", "for_whom": "boss"},
	}, http.StatusCreated)
	if item["type"] != "sales.presales" || item["visibility"] != "participants" || item["subject_ref"] != "sales:deal:d_12" ||
		item["fields"].(map[string]any)["when"] != "Friday" || item["type_version"] != 1.0 {
		t.Fatalf("item = %v", item)
	}
	id := item["id"].(string)
	plain := h.want("ada", "POST", "/v1/work-items", map[string]any{"title": "Plain", "subject_ref": "property:room:r_9"}, http.StatusCreated)
	if plain["type"] != "collab.task" || plain["visibility"] != "team" {
		t.Errorf("plain item = %v", plain)
	}

	// A patch merges, and null clears.
	out := h.want("ada", "PATCH", "/v1/work-items/"+id, map[string]any{
		"version": 1, "fields": map[string]any{"when": nil, "what": "A deck"},
	}, http.StatusOK)
	if f := out["fields"].(map[string]any); f["when"] != nil || f["what"] != "A deck" || f["for_whom"] != "boss" {
		t.Errorf("fields = %v", f)
	}
	h.wantError("ada", "PATCH", "/v1/work-items/"+id, map[string]any{"version": 2, "fields": map[string]any{"colour": "red"}}, http.StatusBadRequest, "invalid_request")

	// A later version that drops a field keeps its value, read-only.
	fields := map[string]any{"properties": map[string]any{"what": map[string]any{"type": "string"}, "for_whom": map[string]any{"type": "string"}}}
	if got, code := h.pushType("t10-sales", with(presales, map[string]any{"version": 2, "fields": fields})); got != http.StatusOK {
		t.Fatalf("push v2 = %d %s", got, code)
	}
	out = h.want("ada", "PATCH", "/v1/work-items/"+id, map[string]any{"version": 2, "fields": map[string]any{"for_whom": "anyone"}}, http.StatusOK)
	if out["type_version"] != 2.0 || out["fields"].(map[string]any)["what"] != "A deck" {
		t.Errorf("after v2: %v", out)
	}
	h.wantError("ada", "PATCH", "/v1/work-items/"+id, map[string]any{"version": 3, "fields": map[string]any{"when": "Monday"}}, http.StatusBadRequest, "invalid_request")
	var detail string
	if err := h.pool.QueryRow(context.Background(),
		"SELECT detail::text FROM collab.events WHERE item_id = $1 AND kind = 'item.updated' ORDER BY seq LIMIT 1", id).Scan(&detail); err != nil ||
		!strings.Contains(detail, "fields.what") {
		t.Errorf("history = %s, %v", detail, err)
	}
}

func TestListsFilterByTypeModuleAndSubject(t *testing.T) {
	h := newHarness(t)
	h.types()
	ask := h.typed("ada", map[string]any{"type": "sales.presales", "subject_ref": "sales:deal:d_12"})
	other := h.typed("ada", map[string]any{"type": "sales.presales", "subject_ref": "sales:deal:d_13"})
	follow := h.typed("ada", map[string]any{"type": "property.followup", "subject_ref": "property:complaint:c_1"})
	task := h.item("ada")
	for query, want := range map[string][]string{
		"type=sales.presales":     {ask, other},
		"module=sales":            {ask, other},
		"module=property":         {follow},
		"subject=sales:deal:d_12": {ask},
		"type=collab.task":        {task},
	} {
		got := ids(h.want("ada", "GET", "/v1/work-items?"+query, nil, http.StatusOK)["items"])
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", query, got, want)
		}
	}
	h.wantError("ada", "GET", "/v1/work-items?module=sa%25", nil, http.StatusBadRequest, "invalid_request")
	// Holding a reference grants nothing.
	if got := ids(h.want("bob", "GET", "/v1/work-items?subject=sales:deal:d_12", nil, http.StatusOK)["items"]); len(got) != 0 {
		t.Errorf("bob sees %v", got)
	}
}

func TestOnlyParticipantsReadAParticipantsItem(t *testing.T) {
	h := newHarness(t)
	h.types()
	h.setRole("eve", "admin")
	h.want("eve", "PUT", "/v1/admin/settings", map[string]string{"manager_visibility": "all_reports"}, http.StatusOK)
	list := h.list("ada", "dan")

	ask := h.ask("ada")
	team := h.typed("ada", map[string]any{"type": "property.followup"})
	// carol manages ada: she reads the team item, never the participants one.
	h.want("carol", "GET", "/v1/work-items/"+team, nil, http.StatusOK)
	h.wantError("carol", "GET", "/v1/work-items/"+ask, nil, http.StatusNotFound, "not_found")
	if got := ids(h.want("carol", "GET", "/v1/work-items?view=reports", nil, http.StatusOK)["items"]); !slices.Equal(got, []string{team}) {
		t.Errorf("carol's reports view = %v", got)
	}
	h.wantError("carol", "GET", "/v1/work-items/"+ask+"/events", nil, http.StatusNotFound, "not_found")
	h.wantError("carol", "GET", "/v1/work-items/"+ask+"/comments", nil, http.StatusNotFound, "not_found")

	// It never reaches a list or a collaborator.
	h.wantError("ada", "PUT", "/v1/work-items/"+ask+"/collaborators/dan", nil, http.StatusConflict, "not_for_type")
	h.wantError("ada", "PUT", "/v1/work-items/"+ask+"/list", map[string]any{"list_id": list}, http.StatusConflict, "not_for_type")
	h.wantError("ada", "POST", "/v1/work-items", map[string]any{"title": "Ask", "type": "sales.presales", "list_id": list}, http.StatusConflict, "not_for_type")
	h.want("ada", "PUT", "/v1/work-items/"+team+"/collaborators/dan", nil, http.StatusNoContent)

	// The recipient reads it while the offer is pending, and not after declining.
	offer := h.send("ada", ask, "bob", "transfer")
	h.want("bob", "GET", "/v1/work-items/"+ask, nil, http.StatusOK)
	// The decline says where it left the item, as bob can't read it after.
	declined := h.want("bob", "POST", "/v1/offers/"+offer+"/decline", map[string]any{"note": "busy"}, http.StatusOK)
	if item, _ := declined["item"].(map[string]any); declined["state"] != "declined" || item["state"] != "declined" || item["status"] != "cancelled" || item["version"] == nil {
		t.Errorf("decline = %v", declined)
	}
	h.wantError("bob", "GET", "/v1/work-items/"+ask, nil, http.StatusNotFound, "not_found")

	// Accepted, bob owns it, and his manager carol still can't read it.
	ask = h.ask("ada")
	accepted := h.want("bob", "POST", "/v1/offers/"+h.send("ada", ask, "bob", "transfer")+"/accept", nil, http.StatusOK)
	if item, _ := accepted["item"].(map[string]any); item["state"] != "accepted" || item["status"] != "doing" {
		t.Errorf("accept = %v", accepted)
	}
	h.want("bob", "GET", "/v1/work-items/"+ask, nil, http.StatusOK)
	h.want("ada", "GET", "/v1/work-items/"+ask, nil, http.StatusOK) // the creator
	h.wantError("carol", "GET", "/v1/work-items/"+ask, nil, http.StatusNotFound, "not_found")
	h.wantError("dan", "GET", "/v1/work-items/"+ask, nil, http.StatusNotFound, "not_found")
}

func TestTheTypeDecidesWhatADepartureDoes(t *testing.T) {
	h := newHarness(t)
	h.types()
	creator := with(presales, map[string]any{"id": "sales.review", "departure": "creator"})
	if got, code := h.pushType("t10-sales", creator); got != http.StatusOK {
		t.Fatalf("push = %d %s", got, code)
	}
	h.setRole("eve", "admin")
	h.managedBy("bob", "carol")

	// cancel: ends at once, and the creator is told.
	cancelled := h.ask("ada")
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", cancelled, "bob", "transfer")+"/accept", nil, http.StatusOK)
	// creator: offered to the creator only, never the manager.
	offered := h.typed("ada", map[string]any{"type": "sales.review", "fields": map[string]any{"what": "A deck", "for_whom": "boss"}})
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", offered, "bob", "transfer")+"/accept", nil, http.StatusOK)
	// creator, with the creator gone too: cancelled.
	orphan := h.typed("dan", map[string]any{"type": "sales.review", "fields": map[string]any{"what": "A deck", "for_whom": "boss"}})
	h.want("bob", "POST", "/v1/offers/"+h.send("dan", orphan, "bob", "transfer")+"/accept", nil, http.StatusOK)

	h.org.set("inactive", acme+"/bob", acme+"/dan")
	h.managedBy("bob", "carol")
	h.sweep()
	h.sweep()

	if got := h.want("ada", "GET", "/v1/work-items/"+cancelled, nil, http.StatusOK)["status"]; got != "cancelled" {
		t.Errorf("cancel type: status %v", got)
	}
	var told int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM collab.notifications n JOIN collab.events e ON e.tenant_id = n.tenant_id AND e.id = n.event_id
		 WHERE n.user_id = 'ada' AND e.item_id = $1 AND e.detail->>'reason' = 'owners_departed'`, cancelled).Scan(&told); err != nil || told != 1 {
		t.Errorf("creator told %d times: %v", told, err)
	}
	if got := h.want("ada", "GET", "/v1/work-items/"+offered, nil, http.StatusOK)["state"]; got != "accepted" {
		t.Errorf("creator type: status %v", got)
	}
	h.wantInbox("ada", 1)
	h.wantInbox("carol", 0)
	if got := h.queue("eve"); len(got) != 0 {
		t.Errorf("queue = %v", got)
	}
	var status string
	if err := h.pool.QueryRow(context.Background(), "SELECT status FROM collab.work_items WHERE id = $1", orphan).Scan(&status); err != nil || status != "cancelled" {
		t.Errorf("orphan: %s %v", status, err)
	}

	// Declined by the creator, the item ends rather than going to managers.
	h.want("ada", "POST", "/v1/offers/"+h.wantInbox("ada", 1)+"/decline", nil, http.StatusOK)
	h.sweep()
	if err := h.pool.QueryRow(context.Background(), "SELECT status FROM collab.work_items WHERE id = $1", offered).Scan(&status); err != nil || status != "cancelled" {
		t.Errorf("declined: %s %v", status, err)
	}
	h.wantInbox("carol", 0)
}

func TestAnAdminCantShowAParticipantsItemToSomeoneNew(t *testing.T) {
	h := newHarness(t)
	h.types()
	h.setRole("eve", "admin")
	item := h.ask("ada")
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", item, "bob", "transfer")+"/accept", nil, http.StatusOK)
	h.suspend("bob")
	h.sweep()
	if got := h.queue("eve"); got[item] != "owner_suspended" {
		t.Fatalf("queue = %v", got)
	}
	h.wantError("eve", "POST", "/v1/admin/departures/"+item+"/reassign", map[string]string{"to": "dan"}, http.StatusConflict, "not_for_type")
	h.want("eve", "POST", "/v1/admin/departures/"+item+"/reassign", map[string]string{"to": "ada"}, http.StatusOK)
}

// TestNoTypeIsNamedInCode is the proposal's test of generality: collab enforces
// what modules declare, and its own code names none of their types.
func TestNoTypeIsNamedInCode(t *testing.T) {
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// "property" is also the ontology's word, so only a string naming
			// the module counts: "property: or "property.
			for _, name := range []string{"sales", `"property:`, `"property.`, "presales", "followup"} {
				if bytes.Contains(bytes.ToLower(src), []byte(name)) {
					t.Errorf("%s names %q", path, name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestAgentsCreateAndFindTypedItems(t *testing.T) {
	h := newHarness(t)
	h.types()
	ts := h.teammateServer()
	created := call(t, ts.URL, "teammate:acc_lead", "work_create", map[string]any{
		"title": "Deck", "type": "sales.presales", "subject_ref": "sales:deal:d_12", "fields": map[string]any{"what": "A deck"},
	})
	if created.failed || created.data["type"] != "sales.presales" {
		t.Fatalf("create = %+v", created)
	}
	if bad := call(t, ts.URL, "teammate:acc_lead", "work_create", map[string]any{"title": "Deck", "type": "sales.presales", "fields": map[string]any{"x": 1}}); !bad.failed {
		t.Errorf("an undeclared field = %+v", bad)
	}
	updated := call(t, ts.URL, "teammate:acc_lead", "work_update", map[string]any{
		"id": created.data["id"], "version": created.data["version"], "fields": map[string]any{"when": "Friday"},
	})
	if updated.failed || updated.data["fields"].(map[string]any)["when"] != "Friday" {
		t.Errorf("update = %+v", updated)
	}
	found := call(t, ts.URL, "teammate:acc_lead", "work_search", map[string]any{"type": "work_item", "filter": map[string]any{"and": []any{
		map[string]any{"property": "type", "op": "eq", "value": "sales.presales"},
		map[string]any{"property": "subject", "op": "eq", "value": "sales:deal:d_12"},
		map[string]any{"property": "sales_presales__when", "op": "eq", "value": "Friday"},
	}}})
	if found.failed || len(found.data["objects"].([]any)) != 1 {
		t.Errorf("search = %+v", found)
	}
}

func moveIDs(item map[string]any) []string {
	out := []string{}
	for _, raw := range item["moves"].([]any) {
		out = append(out, raw.(map[string]any)["id"].(string))
	}
	return out
}

func (h *harness) wantState(reader, item, state, status string) map[string]any {
	h.t.Helper()
	out := h.want(reader, "GET", "/v1/work-items/"+item, nil, http.StatusOK)
	if out["state"] != state || out["status"] != status {
		h.t.Fatalf("%s: state %v, status %v, want %s, %s", item, out["state"], out["status"], state, status)
	}
	return out
}

func TestATypedItemMovesThroughItsLifecycle(t *testing.T) {
	h := newHarness(t)
	h.types()
	item := h.typed("ada", map[string]any{"type": "sales.presales"})
	if got := moveIDs(h.wantState("ada", item, "draft", "todo")); !slices.Equal(got, []string{"cancel"}) {
		t.Errorf("ada's moves in draft = %v", got)
	}
	h.wantError("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 1, "status": "doing"}, http.StatusConflict, "transition_not_allowed")

	// A send needs what the type requires, and fails whole without it.
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "bob"}, http.StatusUnprocessableEntity, "requirements_unmet")
	h.wantInbox("bob", 0)
	version := h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK)["version"]
	h.want("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": version, "fields": map[string]any{"what": "A deck", "for_whom": "boss"}}, http.StatusOK)
	offer := h.send("ada", item, "bob", "transfer")
	h.wantState("ada", item, "sent", "todo")
	if got := moveIDs(h.wantState("bob", item, "sent", "todo")); len(got) != 0 {
		t.Errorf("bob's moves before accepting = %v", got)
	}
	h.wantError("bob", "POST", "/v1/work-items/"+item+"/transition", map[string]any{"transition": "deliver"}, http.StatusConflict, "transition_not_allowed")

	// Withdrawn, it's a draft again; sent again and declined, it's over.
	h.want("ada", "POST", "/v1/offers/"+offer+"/withdraw", nil, http.StatusOK)
	h.wantState("ada", item, "draft", "todo")
	offer = h.send("ada", item, "bob", "transfer")
	h.wantError("bob", "POST", "/v1/offers/"+offer+"/decline", map[string]any{}, http.StatusUnprocessableEntity, "requirements_unmet")
	h.want("bob", "POST", "/v1/offers/"+offer+"/decline", map[string]any{"note": "Not this week"}, http.StatusOK)
	h.wantState("ada", item, "declined", "cancelled")

	// Accepted, bob delivers and ada reviews.
	item = h.ask("ada")
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", item, "bob", "transfer")+"/accept", nil, http.StatusOK)
	if got := moveIDs(h.wantState("bob", item, "accepted", "doing")); !slices.Equal(got, []string{"deliver"}) {
		t.Errorf("bob's moves = %v", got)
	}
	if got := moveIDs(h.wantState("ada", item, "accepted", "doing")); !slices.Equal(got, []string{"cancel"}) {
		t.Errorf("ada's moves = %v", got)
	}
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/transition", map[string]any{"transition": "deliver"}, http.StatusForbidden, "forbidden")
	h.wantError("bob", "POST", "/v1/work-items/"+item+"/transition", map[string]any{"transition": "deliver", "payload": map[string]any{"price": 1}}, http.StatusBadRequest, "invalid_request")
	h.wantError("bob", "POST", "/v1/work-items/"+item+"/transition", map[string]any{
		"transition": "deliver", "payload": map[string]any{"links": []any{map[string]any{"url": "http://x"}}},
	}, http.StatusBadRequest, "invalid_request")
	h.wantError("bob", "POST", "/v1/work-items/"+item+"/transition", map[string]any{"transition": "deliver", "version": 1}, http.StatusConflict, "version_conflict")
	out := h.want("bob", "POST", "/v1/work-items/"+item+"/transition", map[string]any{
		"transition": "deliver", "note": "Here it is",
		"payload": map[string]any{"reply": "Deck attached", "links": []any{map[string]any{"url": "https://docs.example/deck", "title": "Deck"}}},
	}, http.StatusOK)
	if out["state"] != "delivered" {
		t.Fatalf("deliver = %v", out)
	}
	h.want("ada", "POST", "/v1/work-items/"+item+"/transition", map[string]any{"transition": "review"}, http.StatusOK)
	h.wantState("bob", item, "reviewed", "done")

	var detail string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT e.detail::text FROM collab.events e JOIN collab.notifications n ON n.tenant_id = e.tenant_id AND n.event_id = e.id
		 WHERE e.item_id = $1 AND e.kind = 'item.state_changed' AND e.detail->>'transition' = 'deliver' AND n.user_id = 'ada'`, item).Scan(&detail); err != nil {
		t.Fatalf("ada wasn't told of the delivery: %v", err)
	}
	for _, want := range []string{`"to_state": "delivered"`, `"reply": "Deck attached"`, `"note": "Here it is"`} {
		if !strings.Contains(detail, want) {
			t.Errorf("event %s lacks %s", detail, want)
		}
	}
	// Closed by its type, it can't be reopened by status.
	h.wantError("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 5, "status": "todo"}, http.StatusConflict, "transition_not_allowed")
}

func TestCancellingEndsTheOffer(t *testing.T) {
	h := newHarness(t)
	h.types()
	item := h.ask("ada")
	h.send("ada", item, "bob", "transfer")
	h.want("ada", "POST", "/v1/work-items/"+item+"/transition", map[string]any{"transition": "cancel"}, http.StatusOK)
	h.wantState("ada", item, "cancelled", "cancelled") // not back to draft by the withdrawal
	h.wantInbox("bob", 0)
}

func TestAnUnansweredOfferToSomeoneWhoLeftRevertsTheState(t *testing.T) {
	h := newHarness(t)
	h.types()
	item := h.ask("ada")
	h.send("ada", item, "bob", "transfer")
	h.org.set("inactive", acme+"/bob")
	h.sweep()
	h.wantState("ada", item, "draft", "todo")
}

func TestAnAssignmentNeedsAnAssignMapping(t *testing.T) {
	h := newHarness(t)
	h.types()
	ts := h.teammateServer()
	h.url = ts.URL
	// carol manages acc_lead, so her send is an assignment, which skips acceptance.
	item := h.ask("carol")
	h.wantError("carol", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "acc_lead"}, http.StatusConflict, "transition_not_allowed")
	assign := map[string]any{
		"send":     map[string]any{"from": []string{"draft"}, "to": "sent", "requires": []string{"what", "for_whom"}},
		"assign":   map[string]any{"from": []string{"draft"}, "to": "accepted", "requires": []string{"what", "for_whom"}},
		"accept":   map[string]any{"to": "accepted"},
		"decline":  map[string]any{"to": "declined", "note": "required"},
		"withdraw": map[string]any{"to": "draft"},
	}
	if got, code := h.pushType("t10-sales", with(presales, map[string]any{"version": 2, "offers": assign})); got != http.StatusOK {
		t.Fatalf("push = %d %s", got, code)
	}
	h.send("carol", item, "acc_lead", "transfer")
	h.wantState("carol", item, "accepted", "doing")

	// The agent teammate delivers with work_transition; an assistant can't.
	moves := call(t, ts.URL, "teammate:acc_lead", "work_workflow", map[string]any{"id": item})
	if moves.failed || !slices.Equal(moveIDs(moves.data), []string{"deliver"}) {
		t.Fatalf("work_workflow = %+v", moves)
	}
	if got := call(t, ts.URL, "agent-assistant", "work_transition", map[string]any{"id": item, "transition": "deliver"}); !got.failed {
		t.Errorf("an assistant moved it: %+v", got)
	}
	done := call(t, ts.URL, "teammate:acc_lead", "work_transition", map[string]any{
		"id": item, "transition": "deliver", "payload": map[string]any{"reply": "Draft deck in the folder"},
	})
	if done.failed || done.data["state"] != "delivered" {
		t.Fatalf("work_transition = %+v", done)
	}
}

func TestATypeWithStatesStaysOffWorkflows(t *testing.T) {
	h := newHarness(t)
	h.types()
	inspection := with(followup, map[string]any{
		"id": "property.inspection", "states": map[string]any{
			"open": map[string]any{"phase": "todo", "label": "Open"}, "closed": map[string]any{"phase": "done", "label": "Closed"}},
		"initial":     "open",
		"transitions": []any{map[string]any{"id": "close", "from": []string{"open"}, "to": "closed", "by": []string{"owner", "list_member"}}},
	})
	if got, code := h.pushType("t10-property", inspection); got != http.StatusOK {
		t.Fatalf("push = %d %s", got, code)
	}
	list := h.list("ada", "bob")
	h.want("ada", "PUT", "/v1/lists/"+list+"/workflow", map[string]any{"stages": []any{map[string]any{"name": "Do", "holder": "ada"}}, "auto_start": true}, http.StatusOK)
	item := h.typed("ada", map[string]any{"type": "property.inspection", "list_id": list})
	if out := h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK); out["stage"] != nil {
		t.Errorf("auto start put a typed item on the workflow: %v", out["stage"])
	}
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/stage", map[string]any{}, http.StatusConflict, "typed_item_not_on_workflow")
	// A list member may close it, as the type says.
	h.want("bob", "POST", "/v1/work-items/"+item+"/transition", map[string]any{"transition": "close"}, http.StatusOK)
	h.wantState("ada", item, "closed", "done")
}

func TestAStateALaterVersionRemovedCanOnlyBeCancelled(t *testing.T) {
	h := newHarness(t)
	h.types()
	item := h.ask("ada")
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", item, "bob", "transfer")+"/accept", nil, http.StatusOK)
	h.want("bob", "POST", "/v1/work-items/"+item+"/transition", map[string]any{"transition": "deliver"}, http.StatusOK)

	states := map[string]any{}
	for k, v := range presales["states"].(map[string]any) {
		if k != "delivered" {
			states[k] = v
		}
	}
	v2 := with(presales, map[string]any{"version": 2, "states": states, "transitions": []any{
		map[string]any{"id": "review", "from": []string{"accepted"}, "to": "reviewed", "by": "creator"},
		map[string]any{"id": "cancel", "from": []string{"draft", "sent", "accepted"}, "to": "cancelled", "by": "creator"},
	}})
	if got, code := h.pushType("t10-sales", v2); got != http.StatusOK {
		t.Fatalf("push = %d %s", got, code)
	}
	if got := moveIDs(h.wantState("ada", item, "delivered", "doing")); !slices.Equal(got, []string{"cancel"}) {
		t.Errorf("moves from a removed state = %v", got)
	}
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/transition", map[string]any{"transition": "review"}, http.StatusConflict, "transition_not_allowed")
	h.want("ada", "POST", "/v1/work-items/"+item+"/transition", map[string]any{"transition": "cancel"}, http.StatusOK)
}

// feed reads a module's events with a service token, from a cursor.
func (h *harness) feed(token, module, after string, limit int) (int, []map[string]any, string) {
	h.t.Helper()
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/internal/v1/events?module=%s&after=%s&limit=%d", h.url, module, after, limit), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Events []map[string]any `json:"events"`
		Next   string           `json:"next"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Events, out.Next
}

func (h *harness) feedAll(module string) ([]map[string]any, string) {
	h.t.Helper()
	all, cursor := []map[string]any{}, ""
	for {
		status, events, next := h.feed("t10-"+module, module, cursor, 2)
		if status != http.StatusOK {
			h.t.Fatalf("feed = %d", status)
		}
		all, cursor = append(all, events...), next
		if len(events) < 2 {
			return all, cursor
		}
	}
}

func TestAModuleReadsOnlyItsOwnEvents(t *testing.T) {
	h := newHarness(t)
	h.types()
	ask := h.ask("ada")
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", ask, "bob", "transfer")+"/accept", nil, http.StatusOK)
	h.want("bob", "POST", "/v1/work-items/"+ask+"/transition", map[string]any{"transition": "deliver", "payload": map[string]any{"reply": "Done"}}, http.StatusOK)
	follow := h.typed("ada", map[string]any{"type": "property.followup"})
	h.item("ada")

	for token, want := range map[string]int{"t10-property": http.StatusForbidden, acme + "/ada": http.StatusForbidden, "garbage": http.StatusUnauthorized} {
		if got, _, _ := h.feed(token, "sales", "", 50); got != want {
			t.Errorf("%s reading sales = %d, want %d", token, got, want)
		}
	}
	for _, module := range []string{"collab", "Sales", ""} {
		if got, _, _ := h.feed("t10-sales", module, "", 50); got != http.StatusBadRequest {
			t.Errorf("module %q = %d", module, got)
		}
	}
	if got, _, _ := h.feed("t10-sales", "sales", "nonsense", 50); got != http.StatusBadRequest {
		t.Errorf("a bad cursor = %d", got)
	}

	events, cursor := h.feedAll("sales")
	kinds := []string{}
	for _, e := range events {
		if e["item_id"] != ask || e["tenant"] != acme || e["type"] != "sales.presales" {
			t.Fatalf("sales feed holds %v", e)
		}
		kinds = append(kinds, e["kind"].(string))
	}
	if want := []string{"item.created", "offer.sent", "item.state_changed", "item.state_changed", "offer.accepted", "item.state_changed"}; !slices.Equal(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
	last := events[len(events)-1]
	if last["detail"].(map[string]any)["payload"].(map[string]any)["reply"] != "Done" || last["item"].(map[string]any)["state"] != "delivered" ||
		last["item"].(map[string]any)["fields"].(map[string]any)["what"] != "A deck" {
		t.Errorf("last event = %v", last)
	}
	// Caught up, the cursor stays put.
	if _, more, next := h.feed("t10-sales", "sales", cursor, 50); len(more) != 0 || next != cursor {
		t.Errorf("after the end: %v, %q", more, next)
	}
	property, _ := h.feedAll("property")
	if len(property) != 1 || property[0]["item_id"] != follow {
		t.Errorf("property feed = %v", property)
	}
}

func TestTheFeedShowsWhoAnItemIsOfferedTo(t *testing.T) {
	h := newHarness(t)
	h.types()
	ask := h.typed("ada", map[string]any{"type": "sales.presales", "due_at": "2026-12-01T09:00:00Z", "fields": map[string]any{"what": "A deck", "for_whom": "boss"}})
	first := h.send("ada", ask, "bob", "share")
	second := h.send("ada", ask, "dan", "transfer")

	events, _ := h.feedAll("sales")
	item := events[len(events)-1]["item"].(map[string]any)
	if due, _ := time.Parse(time.RFC3339, fmt.Sprint(item["due_at"])); !due.Equal(time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("due_at = %v", item["due_at"])
	}
	want := []any{
		map[string]any{"id": first, "to_user_id": "bob", "mode": "share"},
		map[string]any{"id": second, "to_user_id": "dan", "mode": "transfer"},
	}
	if !reflect.DeepEqual(item["pending_offers"], want) {
		t.Errorf("pending_offers = %v, want %v", item["pending_offers"], want)
	}

	// Answered, the offers leave, and the feed shows it on every event.
	h.want("bob", "POST", "/v1/offers/"+first+"/decline", nil, http.StatusOK)
	h.want("dan", "POST", "/v1/offers/"+second+"/decline", map[string]any{"note": "Not mine"}, http.StatusOK)
	events, _ = h.feedAll("sales")
	for _, e := range events {
		if got := e["item"].(map[string]any)["pending_offers"]; !reflect.DeepEqual(got, []any{}) {
			t.Errorf("%s: pending_offers = %v", e["kind"], got)
		}
	}
}

func TestTheFeedWaitsForASlowCommit(t *testing.T) {
	h := newHarness(t)
	h.types()
	first := h.ask("ada")
	_, cursor := h.feedAll("sales")

	// A transaction that started first, but commits after a later one.
	ctx := context.Background()
	slow, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Rollback(ctx) //nolint:errcheck
	if _, err := slow.Exec(ctx,
		`INSERT INTO collab.events (tenant_id, id, item_id, actor_user_id, kind, module) VALUES ($1, 'evt_slow', $2, 'ada', 'comment.added', 'sales')`,
		acme, first); err != nil {
		t.Fatal(err)
	}
	h.ask("bob")
	if _, events, next := h.feed("t10-sales", "sales", cursor, 50); len(events) != 0 || next != cursor {
		t.Fatalf("read past a running transaction: %v", events)
	}
	if err := slow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	_, events, _ := h.feed("t10-sales", "sales", cursor, 50)
	if len(events) != 2 || events[0]["id"] != "evt_slow" || events[1]["kind"] != "item.created" {
		t.Fatalf("after the commit: %v", events)
	}
}

func TestADeletedItemsEventsCarryNoItem(t *testing.T) {
	h := newHarness(t)
	h.types()
	ask := h.ask("ada")
	h.want("ada", "DELETE", "/v1/work-items/"+ask, nil, http.StatusNoContent)
	events, _ := h.feedAll("sales")
	if len(events) != 2 || events[1]["kind"] != "item.deleted" {
		t.Fatalf("events = %v", events)
	}
	for _, e := range events {
		if _, has := e["item"]; has {
			t.Errorf("a deleted item's event shows it: %v", e)
		}
	}
}

// tenantType puts or deletes a tenant's version of a type, and returns the status and error code.
func (h *harness) tenantType(method, service, tenant string, def map[string]any) (int, string) {
	h.t.Helper()
	raw, _ := json.Marshal(def)
	req, _ := http.NewRequest(method, h.url+"/internal/v1/types/"+def["id"].(string)+"/tenants/"+tenant, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+service)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	code, _ := out["error"].(string)
	return resp.StatusCode, code
}

func (h *harness) typeLabel(token, id string) string {
	h.t.Helper()
	_, out := h.request(token, "GET", "/v1/types", nil)
	for _, raw := range out["types"].([]any) {
		if ty := raw.(map[string]any); ty["id"] == id {
			return ty["label"].(string)
		}
	}
	return ""
}

// copied deep-copies a fixture, so a test can change nested parts.
func copied(def map[string]any) map[string]any {
	raw, _ := json.Marshal(def)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestAModuleTailorsATypeForOneTenant(t *testing.T) {
	h := newHarness(t)
	h.types()
	// acme's pack: its own words, a budget field, and a budget needed before a send.
	acmes := copied(presales)
	acmes["label"] = "售前方案"
	props := acmes["fields"].(map[string]any)["properties"].(map[string]any)
	props["when"].(map[string]any)["title"] = "什么时候要"
	props["budget"] = map[string]any{"type": "integer", "title": "预算"}
	acmes["states"].(map[string]any)["sent"].(map[string]any)["label"] = "已发出"
	acmes["offers"].(map[string]any)["send"].(map[string]any)["requires"] = []any{"what", "for_whom", "budget"}
	acmes["transitions"].([]any)[0].(map[string]any)["label"] = "交付"

	if got, code := h.tenantType("PUT", "t10-property", acme, acmes); got != http.StatusForbidden {
		t.Errorf("another module = %d %s", got, code)
	}
	if got, code := h.tenantType("PUT", "t10-sales", "not a tenant", acmes); got != http.StatusBadRequest {
		t.Errorf("a bad tenant = %d %s", got, code)
	}
	if got, code := h.tenantType("PUT", "t10-sales", acme, with(acmes, map[string]any{"version": 2})); got != http.StatusConflict || code != "type_default_missing" {
		t.Errorf("a version with no default = %d %s", got, code)
	}
	for name, change := range map[string]func(map[string]any){
		"a phase": func(d map[string]any) { d["states"].(map[string]any)["sent"].(map[string]any)["phase"] = "doing" },
		"a new state": func(d map[string]any) {
			d["states"].(map[string]any)["paused"] = map[string]any{"phase": "doing", "label": "Paused"}
		},
		"who moves":    func(d map[string]any) { d["transitions"].([]any)[0].(map[string]any)["by"] = "creator" },
		"where to":     func(d map[string]any) { d["offers"].(map[string]any)["accept"].(map[string]any)["to"] = "delivered" },
		"the profile":  func(d map[string]any) { d["visibility"] = "team"; d["departure"] = "reassign" },
		"a lost field": func(d map[string]any) { delete(d["fields"].(map[string]any)["properties"].(map[string]any), "what") },
		"a field shape": func(d map[string]any) {
			d["fields"].(map[string]any)["properties"].(map[string]any)["when"].(map[string]any)["maxLength"] = 10
		},
		"the subject": func(d map[string]any) { d["subject"] = []any{"sales:deal"} },
	} {
		bad := copied(acmes)
		change(bad)
		if got, code := h.tenantType("PUT", "t10-sales", acme, bad); got != http.StatusBadRequest || code != "invalid_type" {
			t.Errorf("changing %s = %d %s", name, got, code)
		}
	}

	if got, code := h.tenantType("PUT", "t10-sales", acme, acmes); got != http.StatusOK {
		t.Fatalf("tailor = %d %s", got, code)
	}
	if got := h.typeLabel(acme+"/ada", "sales.presales"); got != "售前方案" {
		t.Errorf("acme sees %q", got)
	}
	if got := h.typeLabel(globex+"/gus", "sales.presales"); got != "Presales" {
		t.Errorf("globex sees %q", got)
	}
	// acme's send needs a budget; its items take the new field.
	item := h.ask("ada")
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "bob"}, http.StatusUnprocessableEntity, "requirements_unmet")
	h.want("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 1, "fields": map[string]any{"budget": 5000}}, http.StatusOK)
	h.send("ada", item, "bob", "transfer")

	// A newer default puts the tenant back on it until the module tailors that one.
	if got, code := h.pushType("t10-sales", with(presales, map[string]any{"version": 2, "label": "Presales v2"})); got != http.StatusOK {
		t.Fatalf("push v2 = %d %s", got, code)
	}
	if got := h.typeLabel(acme+"/ada", "sales.presales"); got != "Presales v2" {
		t.Errorf("after a new default acme sees %q", got)
	}
	if got, code := h.tenantType("PUT", "t10-sales", acme, with(acmes, map[string]any{"version": 2})); got != http.StatusOK {
		t.Fatalf("tailor v2 = %d %s", got, code)
	}
	if got := h.typeLabel(acme+"/ada", "sales.presales"); got != "售前方案" {
		t.Errorf("after tailoring v2 acme sees %q", got)
	}
	for range 2 {
		if got, code := h.tenantType("DELETE", "t10-sales", acme, acmes); got != http.StatusNoContent {
			t.Fatalf("delete = %d %s", got, code)
		}
	}
	if got := h.typeLabel(acme+"/ada", "sales.presales"); got != "Presales v2" {
		t.Errorf("after delete acme sees %q", got)
	}
}
