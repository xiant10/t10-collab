package server

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/xiant10/t10-modkit/people"
)

// workflowTeam is carol's list with three agent teammates on it, each taking
// work from the list: acc_lead builds, acc_close reviews and acc_test tests.
func (h *harness) workflowTeam() (string, string) {
	h.t.Helper()
	ts := h.teammateServer()
	h.org.mu.Lock()
	h.org.members[acme+"/acc_test"] = people.Member{UserID: "acc_test", DisplayName: "Test desk", Status: "active", UserStatus: "active", Kind: people.KindAgent, ManagerUserID: "carol"}
	h.org.mu.Unlock()
	h.url = ts.URL
	list := h.want("carol", "POST", "/v1/lists", map[string]any{"name": "Payments"}, http.StatusCreated)["id"].(string)
	for _, agent := range []string{"acc_lead", "acc_close", "acc_test"} {
		h.want("carol", "PUT", "/v1/lists/"+list+"/members/"+agent, map[string]any{"role": "member"}, http.StatusNoContent)
	}
	return list, ts.URL
}

func stages(holders ...string) []map[string]any {
	names := []string{"Build", "Review", "Test", "Approve"}
	var out []map[string]any
	for i, holder := range holders {
		out = append(out, map[string]any{"name": names[i], "holder": holder})
	}
	return out
}

func (h *harness) wantStage(reader, item, stage string, owner string, rounds int) map[string]any {
	h.t.Helper()
	out := h.want(reader, "GET", "/v1/work-items/"+item, nil, http.StatusOK)
	wf, _ := out["workflow"].(map[string]any)
	var got string
	if st, ok := wf["stage"].(map[string]any); ok {
		got = st["name"].(string)
	}
	if got != stage {
		h.t.Errorf("stage = %q, want %q", got, stage)
	}
	if owners := strs(out["owners"]); !slices.Equal(owners, []string{owner}) {
		h.t.Errorf("owners = %v, want %s", owners, owner)
	}
	if n, _ := out["rounds"].(float64); int(n) != rounds {
		h.t.Errorf("rounds = %v, want %d", out["rounds"], rounds)
	}
	return out
}

func TestAWorkflowRunsItsLoopsAndEndsWithAPerson(t *testing.T) {
	h := newHarness(t)
	list, mcp := h.workflowTeam()
	path := "/v1/lists/" + list + "/workflow"

	// Every holder must be on the list and take work from it.
	h.wantError("carol", "PUT", path, map[string]any{"stages": stages("acc_lead", "bob")}, http.StatusUnprocessableEntity, "not_on_list")
	h.wantError("carol", "PUT", path, map[string]any{"stages": stages("acc_lead")}, http.StatusUnprocessableEntity, "holder_not_accepting")
	for _, agent := range []string{"acc_lead", "acc_close", "acc_test"} {
		h.want("eve", "PUT", "/v1/admin/agents/"+agent+"/intake", map[string]any{"lists": []string{list}}, http.StatusOK)
	}
	// Changes go back only, to a stage before.
	bad := stages("acc_lead", "acc_close")
	bad[0]["id"], bad[0]["on_changes"] = "stg_build", "stg_review"
	bad[1]["id"] = "stg_review"
	h.wantError("carol", "PUT", path, map[string]any{"stages": bad}, http.StatusBadRequest, "invalid_request")
	// Only a list owner shapes the route.
	h.wantError("acc_lead", "PUT", path, map[string]any{"stages": stages("acc_lead")}, http.StatusForbidden, "forbidden")

	route := stages("acc_lead", "acc_close", "acc_test", "carol")
	for i, id := range []string{"stg_build", "stg_review", "stg_test", "stg_approve"} {
		route[i]["id"] = id
	}
	route[1]["on_changes"], route[2]["on_changes"] = "stg_build", "stg_build"
	route[2]["guide"] = "Run the end-to-end tests"
	wf := h.want("carol", "PUT", path, map[string]any{"stages": route, "rounds": 2}, http.StatusOK)
	if wf["lead"] != "carol" || wf["version"].(float64) != 1 {
		t.Fatalf("workflow = %v", wf)
	}

	item := h.want("carol", "POST", "/v1/work-items", map[string]any{"title": "Second approver", "list_id": list}, http.StatusCreated)["id"].(string)
	h.clearOutbox()
	h.want("carol", "POST", "/v1/work-items/"+item+"/stage", map[string]any{"note": "Please build this"}, http.StatusOK)
	h.wantStage("carol", item, "Build", "acc_lead", 0)

	advance := func(agent, result, note string) toolReply {
		t.Helper()
		r := call(t, mcp, "teammate:"+agent, "work_advance", map[string]any{"id": item, "result": result, "note": note})
		if r.failed {
			t.Fatalf("%s %s: %s", agent, result, r.text)
		}
		return r
	}
	// Two rounds back from Test to Build, then a third goes to carol, the lead.
	for round := 1; round <= 3; round++ {
		advance("acc_lead", "pass", "built")
		advance("acc_close", "pass", "reviewed")
		h.wantStage("carol", item, "Test", "acc_test", round-1)
		advance("acc_test", "changes", "refunds over the limit still pass")
		if round < 3 {
			h.wantStage("carol", item, "Build", "acc_lead", round)
		}
	}
	h.wantStage("carol", item, "", "carol", 0)
	// A workflow move is a person's route: no hops pile up, and no send was
	// redirected to a manager. Each holder was woken once per stage, and
	// nobody that handed the item on was woken by the next one's move.
	woken := map[string]int{}
	for _, row := range h.outbox() {
		if row.hops != 0 {
			t.Errorf("hops %d on %+v", row.hops, row)
		}
		if row.kind == "work.assigned" {
			woken[row.forUser]++
		}
	}
	if want := map[string]int{"acc_lead": 3, "acc_close": 3, "acc_test": 3}; !maps.Equal(woken, want) {
		t.Errorf("woken = %v, want %v", woken, want)
	}

	// The agent at a stage reads where each result takes the item.
	h.want("carol", "POST", "/v1/work-items/"+item+"/stage", map[string]any{"stage": "stg_test"}, http.StatusOK)
	got := call(t, mcp, "teammate:acc_test", "work_get", map[string]any{"id": item})
	place := got.data["workflow"].(map[string]any)
	if place["stage"].(map[string]any)["guide"] != "Run the end-to-end tests" || place["pass"].(map[string]any)["to"] != "carol" ||
		place["changes"].(map[string]any)["to"] != "acc_lead" {
		t.Errorf("work_get workflow = %v", place)
	}
	// changes needs a note; only the holder finishes a stage.
	if r := call(t, mcp, "teammate:acc_test", "work_advance", map[string]any{"id": item, "result": "changes", "note": " "}); !r.failed {
		t.Error("changes with no note moved the item")
	}
	if r := call(t, mcp, "teammate:acc_lead", "work_advance", map[string]any{"id": item, "result": "pass", "note": "x"}); !r.failed {
		t.Error("a former holder finished the stage")
	}
	advance("acc_test", "pass", "e2e green")
	h.wantStage("carol", item, "Approve", "carol", 0)
	h.wantError("carol", "POST", "/v1/work-items/"+item+"/advance", map[string]any{"result": "changes", "note": "x"}, http.StatusConflict, "no_way_back")
	done := h.want("carol", "POST", "/v1/work-items/"+item+"/advance", map[string]any{"result": "pass"}, http.StatusOK)
	if done["status"] != "done" || done["stage"] != nil {
		t.Errorf("after the last stage: status %v, stage %v", done["status"], done["stage"])
	}
}

func TestChangingAWorkflowKeepsItemsInFlight(t *testing.T) {
	h := newHarness(t)
	list, mcp := h.workflowTeam()
	for _, agent := range []string{"acc_lead", "acc_close"} {
		h.want("eve", "PUT", "/v1/admin/agents/"+agent+"/intake", map[string]any{"lists": []string{list}}, http.StatusOK)
	}
	path := "/v1/lists/" + list + "/workflow"
	wf := h.want("carol", "PUT", path, map[string]any{"stages": stages("acc_lead", "acc_close")}, http.StatusOK)
	saved := wf["stages"].([]any)
	build, review := saved[0].(map[string]any), saved[1].(map[string]any)
	if build["id"] == "" || wf["rounds"].(float64) != 3 {
		t.Fatalf("workflow = %v", wf)
	}
	a := h.want("carol", "POST", "/v1/work-items", map[string]any{"title": "A", "list_id": list}, http.StatusCreated)["id"].(string)
	b := h.want("carol", "POST", "/v1/work-items", map[string]any{"title": "B", "list_id": list}, http.StatusCreated)["id"].(string)
	h.want("carol", "POST", "/v1/work-items/"+a+"/stage", map[string]any{}, http.StatusOK)
	h.want("carol", "POST", "/v1/work-items/"+b+"/stage", map[string]any{"stage": review["id"]}, http.StatusOK)

	// Rename Build and drop Review: A stays at Build, B leaves the workflow
	// and stays with its holder.
	build["name"] = "Code"
	h.want("carol", "PUT", path, map[string]any{"stages": []any{build}}, http.StatusOK)
	h.wantStage("carol", a, "Code", "acc_lead", 0)
	h.wantStage("carol", b, "", "acc_close", 0)
	events := h.want("carol", "GET", "/v1/work-items/"+b+"/events", nil, http.StatusOK)["events"].([]any)
	if last := events[len(events)-1].(map[string]any); last["kind"] != "workflow.left" {
		t.Errorf("last event on B = %v", last)
	}

	// A holder taken off the list doesn't get the item; the lead does.
	h.want("carol", "PUT", path, map[string]any{"stages": []any{build, review}}, http.StatusOK)
	h.want("carol", "POST", "/v1/work-items/"+b+"/stage", map[string]any{}, http.StatusOK)
	h.want("carol", "DELETE", "/v1/lists/"+list+"/members/acc_close", nil, http.StatusNoContent)
	advanced := call(t, mcp, "teammate:acc_lead", "work_advance", map[string]any{"id": b, "result": "pass", "note": "built"})
	if advanced.failed {
		t.Fatal(advanced.text)
	}
	h.wantStage("carol", b, "", "carol", 0)

	// A send outside the workflow takes the item off it once it's taken.
	h.want("carol", "POST", "/v1/work-items/"+a+"/stage", map[string]any{}, http.StatusOK)
	sent := call(t, mcp, "teammate:acc_lead", "work_send", map[string]any{"id": a, "to": "carol", "note": "Stuck: need a decision"})
	if sent.failed {
		t.Fatal(sent.text)
	}
	h.wantStage("carol", a, "Code", "acc_lead", 0)
	h.want("carol", "POST", "/v1/offers/"+sent.data["id"].(string)+"/accept", nil, http.StatusOK)
	h.wantStage("carol", a, "", "carol", 0)

	// Removing the workflow leaves items with their holders.
	h.want("carol", "POST", "/v1/work-items/"+a+"/stage", map[string]any{}, http.StatusOK)
	h.want("carol", "DELETE", path, nil, http.StatusNoContent)
	h.wantStage("carol", a, "", "acc_lead", 0)
	if r := call(t, mcp, "teammate:acc_lead", "work_advance", map[string]any{"id": a, "result": "pass", "note": "done"}); !r.failed || !strings.Contains(r.text, "isn't at a stage") {
		t.Errorf("advance off the workflow: %+v", r)
	}
}
