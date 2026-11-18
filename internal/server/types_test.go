package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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

	ask := h.typed("ada", map[string]any{"type": "sales.presales"})
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
	h.want("bob", "POST", "/v1/offers/"+offer+"/decline", nil, http.StatusOK)
	h.wantError("bob", "GET", "/v1/work-items/"+ask, nil, http.StatusNotFound, "not_found")

	// Accepted, bob owns it, and his manager carol still can't read it.
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", ask, "bob", "transfer")+"/accept", nil, http.StatusOK)
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
	cancelled := h.typed("ada", map[string]any{"type": "sales.presales"})
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", cancelled, "bob", "transfer")+"/accept", nil, http.StatusOK)
	// creator: offered to the creator only, never the manager.
	offered := h.typed("ada", map[string]any{"type": "sales.review"})
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", offered, "bob", "transfer")+"/accept", nil, http.StatusOK)
	// creator, with the creator gone too: cancelled.
	orphan := h.typed("dan", map[string]any{"type": "sales.review"})
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
	if got := h.want("ada", "GET", "/v1/work-items/"+offered, nil, http.StatusOK)["status"]; got != "todo" {
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
	item := h.typed("ada", map[string]any{"type": "sales.presales"})
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
			for _, name := range []string{"sales", "property", "presales", "followup"} {
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
	ts := h.mcpServer()
	created := call(t, ts.URL, "agent-write", "work_create", map[string]any{
		"title": "Deck", "type": "sales.presales", "subject_ref": "sales:deal:d_12", "fields": map[string]any{"what": "A deck"},
	})
	if created.failed || created.data["type"] != "sales.presales" {
		t.Fatalf("create = %+v", created)
	}
	if bad := call(t, ts.URL, "agent-write", "work_create", map[string]any{"title": "Deck", "type": "sales.presales", "fields": map[string]any{"x": 1}}); !bad.failed {
		t.Errorf("an undeclared field = %+v", bad)
	}
	updated := call(t, ts.URL, "agent-write", "work_update", map[string]any{
		"id": created.data["id"], "version": created.data["version"], "fields": map[string]any{"when": "Friday"},
	})
	if updated.failed || updated.data["fields"].(map[string]any)["when"] != "Friday" {
		t.Errorf("update = %+v", updated)
	}
	found := call(t, ts.URL, "agent-read", "work_list", map[string]any{"module": "sales", "subject": "sales:deal:d_12"})
	if found.failed || len(found.data["items"].([]any)) != 1 {
		t.Errorf("list = %+v", found)
	}
}
