package server

import (
	"context"
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

func TestAWorkflowStartsItemsSharesStagesAndSkips(t *testing.T) {
	h := newHarness(t)
	list, mcp := h.workflowTeam()
	for _, agent := range []string{"acc_lead", "acc_close", "acc_test"} {
		h.want("eve", "PUT", "/v1/admin/agents/"+agent+"/intake", map[string]any{"lists": []string{list}}, http.StatusOK)
	}
	path := "/v1/lists/" + list + "/workflow"
	route := []map[string]any{
		{"id": "stg_build", "name": "Build", "holders": []string{"acc_lead", "acc_close"}},
		{"id": "stg_test", "name": "Test", "holder": "acc_test", "on_changes": "stg_build", "skip_when": "Only docs changed"},
		{"id": "stg_approve", "name": "Approve", "holder": "carol"},
	}
	// Nothing passes over the first stage, and a holder is named once.
	first := slices.Clone(route)
	first[0] = map[string]any{"id": "stg_build", "name": "Build", "holder": "acc_lead", "skip_when": "never"}
	h.wantError("carol", "PUT", path, map[string]any{"stages": first}, http.StatusBadRequest, "invalid_request")
	twice := slices.Clone(route)
	twice[0] = map[string]any{"id": "stg_build", "name": "Build", "holders": []string{"acc_lead", "acc_lead"}}
	h.wantError("carol", "PUT", path, map[string]any{"stages": twice}, http.StatusBadRequest, "invalid_request")
	wf := h.want("carol", "PUT", path, map[string]any{"stages": route, "auto_start": true}, http.StatusOK)
	if wf["auto_start"] != true || wf["stages"].([]any)[1].(map[string]any)["holders"].([]any)[0] != "acc_test" {
		t.Fatalf("workflow = %v", wf)
	}

	// New items start at Build by themselves, each going to the builder with
	// fewer open items, and nobody else on the list is woken.
	h.clearOutbox()
	create := func(title string) string {
		t.Helper()
		return h.want("carol", "POST", "/v1/work-items", map[string]any{"title": title, "list_id": list}, http.StatusCreated)["id"].(string)
	}
	a, b := create("A"), create("B")
	h.wantStage("carol", a, "Build", "acc_lead", 0)
	h.wantStage("carol", b, "Build", "acc_close", 0)
	for _, row := range h.outbox() {
		if row.forUser == "acc_test" {
			t.Errorf("acc_test told of a new item: %+v", row)
		}
	}
	// Build shows both builders; who gets it is picked at the move.
	got := h.want("acc_test", "GET", "/v1/work-items/"+b, nil, http.StatusOK)["workflow"].(map[string]any)
	if skip := got["skip"].([]any); len(skip) != 1 || skip[0].(map[string]any)["stage"] != "stg_approve" {
		t.Errorf("skip from Build = %v", got["skip"])
	}

	// Back to Build, the item goes to the builder that built it, though the
	// other has fewer items now.
	if r := call(t, mcp, "teammate:acc_close", "work_advance", map[string]any{"id": b, "result": "pass", "note": "built"}); r.failed {
		t.Fatal(r.text)
	}
	c, e := create("C"), create("E")
	h.wantStage("carol", c, "Build", "acc_close", 0)
	h.wantStage("carol", e, "Build", "acc_lead", 0)
	create("F")
	h.want("carol", "POST", "/v1/work-items/"+a+"/stage", map[string]any{"stage": "stg_test"}, http.StatusOK)
	if r := call(t, mcp, "teammate:acc_test", "work_advance", map[string]any{"id": b, "result": "changes", "note": "fails"}); r.failed {
		t.Fatal(r.text)
	}
	h.wantStage("carol", b, "Build", "acc_close", 1)

	// A skip says why, and goes only forward over stages that may be skipped.
	h.wantError("acc_close", "POST", "/v1/work-items/"+b+"/advance", map[string]any{"result": "pass", "to": "stg_approve"}, http.StatusBadRequest, "invalid_request")
	h.wantError("acc_close", "POST", "/v1/work-items/"+b+"/advance", map[string]any{"result": "pass", "to": "stg_build", "note": "x"}, http.StatusConflict, "cant_skip")
	if r := call(t, mcp, "teammate:acc_close", "work_advance", map[string]any{"id": b, "result": "pass", "to": "stg_approve", "note": "Only the README changed"}); r.failed {
		t.Fatal(r.text)
	}
	h.wantStage("carol", b, "Approve", "carol", 1)
	events := h.want("carol", "GET", "/v1/work-items/"+b+"/events", nil, http.StatusOK)["events"].([]any)
	last := events[len(events)-1].(map[string]any)
	if skipped, _ := last["detail"].(map[string]any)["skipped"].([]any); len(skipped) != 1 || skipped[0] != "stg_test" {
		t.Errorf("last event on B = %v", last)
	}
	// Test's holder can't skip Approve: it has no skip_when.
	h.wantError("acc_test", "POST", "/v1/work-items/"+a+"/advance", map[string]any{"result": "pass", "to": "done", "note": "x"}, http.StatusConflict, "cant_skip")

	// An item sent at creation goes where it's sent, not to Build.
	sent := h.want("carol", "POST", "/v1/work-items", map[string]any{"title": "G", "list_id": list, "send": map[string]any{"to": "bob", "note": "yours"}}, http.StatusCreated)
	if sent["stage"] != nil {
		t.Errorf("sent item started: %v", sent["stage"])
	}
}

func TestListChangesKeepTheWorkflowOnItsMembers(t *testing.T) {
	h := newHarness(t)
	list, _ := h.workflowTeam()
	for _, agent := range []string{"acc_lead", "acc_close"} {
		h.want("eve", "PUT", "/v1/admin/agents/"+agent+"/intake", map[string]any{"lists": []string{list}}, http.StatusOK)
	}
	h.want("carol", "PUT", "/v1/lists/"+list+"/members/bob", map[string]any{"role": "member"}, http.StatusNoContent)
	path := "/v1/lists/" + list + "/workflow"
	h.want("carol", "PUT", path, map[string]any{"stages": []map[string]any{
		{"id": "stg_build", "name": "Build", "holders": []string{"acc_lead", "acc_close"}},
		{"id": "stg_approve", "name": "Approve", "holder": "carol"},
	}}, http.StatusOK)
	if lists := h.want("bob", "GET", "/v1/lists", nil, http.StatusOK)["lists"].([]any); lists[0].(map[string]any)["has_workflow"] != true {
		t.Errorf("lists = %v", lists)
	}

	// Any member starts an item nobody owns at the first stage, as they may
	// claim it; only a list owner puts it further on.
	unowned := func() string {
		return h.want("bob", "POST", "/v1/work-items", map[string]any{"title": "Task", "list_id": list, "unowned": true}, http.StatusCreated)["id"].(string)
	}
	a := unowned()
	h.wantError("bob", "POST", "/v1/work-items/"+a+"/stage", map[string]any{"stage": "stg_approve"}, http.StatusForbidden, "forbidden")
	h.want("bob", "POST", "/v1/work-items/"+a+"/stage", map[string]any{}, http.StatusOK)
	h.wantStage("bob", a, "Build", "acc_lead", 0)

	// An agent its admin no longer lets take work from the list isn't given
	// any, though it would be next.
	h.want("eve", "PUT", "/v1/admin/agents/acc_close/intake", map[string]any{"lists": []string{}}, http.StatusOK)
	b := unowned()
	h.want("bob", "POST", "/v1/work-items/"+b+"/stage", map[string]any{}, http.StatusOK)
	h.wantStage("bob", b, "Build", "acc_lead", 0)

	// A holder taken off the list leaves stages that have others.
	h.want("carol", "DELETE", "/v1/lists/"+list+"/members/acc_close", nil, http.StatusNoContent)
	wf := h.want("carol", "GET", "/v1/lists/"+list, nil, http.StatusOK)["workflow"].(map[string]any)
	if holders := strs(wf["stages"].([]any)[0].(map[string]any)["holders"]); !slices.Equal(holders, []string{"acc_lead"}) {
		t.Errorf("Build holders = %v", holders)
	}
	// A lead who stops owning the list gives way to an owner, who is told.
	h.want("carol", "PUT", "/v1/lists/"+list+"/members/bob", map[string]any{"role": "owner"}, http.StatusNoContent)
	h.want("carol", "PUT", "/v1/lists/"+list+"/members/carol", map[string]any{"role": "member"}, http.StatusNoContent)
	wf = h.want("carol", "GET", "/v1/lists/"+list, nil, http.StatusOK)["workflow"].(map[string]any)
	if wf["lead"] != "bob" {
		t.Errorf("lead = %v, want bob", wf["lead"])
	}
	var told bool
	if err := h.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM collab.notifications n JOIN collab.events e ON e.tenant_id = n.tenant_id AND e.id = n.event_id
		 WHERE n.user_id = 'bob' AND e.kind = 'list.workflow_changed' AND e.detail->>'lead' = 'bob')`).Scan(&told); err != nil || !told {
		t.Errorf("bob wasn't told he leads the workflow: %v", err)
	}
}

func TestAPersonHoldsStagesOnlyOnceTheyAgree(t *testing.T) {
	h := newHarness(t)
	list := h.want("carol", "POST", "/v1/lists", map[string]any{"name": "Reviews"}, http.StatusCreated)["id"].(string)
	h.want("carol", "PUT", "/v1/lists/"+list+"/members/bob", map[string]any{"role": "member"}, http.StatusNoContent)
	h.want("carol", "PUT", "/v1/lists/"+list+"/workflow", map[string]any{"stages": []map[string]any{
		{"id": "stg_write", "name": "Write", "holder": "carol"},
		{"id": "stg_review", "name": "Review", "holder": "bob"},
	}}, http.StatusOK)
	var asked bool
	if err := h.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM collab.notifications n JOIN collab.events e ON e.tenant_id = n.tenant_id AND e.id = n.event_id
		 WHERE n.user_id = 'bob' AND e.kind = 'list.holder_named')`).Scan(&asked); err != nil || !asked {
		t.Errorf("bob wasn't asked to hold Review: %v", err)
	}

	// A list owner can't put work on a peer without asking: until bob agrees,
	// Review passes him over and the item goes to the lead.
	item := h.want("carol", "POST", "/v1/work-items", map[string]any{"title": "Doc", "list_id": list}, http.StatusCreated)["id"].(string)
	h.want("carol", "POST", "/v1/work-items/"+item+"/stage", map[string]any{}, http.StatusOK)
	h.want("carol", "POST", "/v1/work-items/"+item+"/advance", map[string]any{"result": "pass"}, http.StatusOK)
	h.wantStage("carol", item, "", "carol", 0)

	h.want("bob", "PUT", "/v1/lists/"+list+"/holding", nil, http.StatusNoContent)
	members := h.want("bob", "GET", "/v1/lists/"+list, nil, http.StatusOK)["members"].([]any)
	if !slices.ContainsFunc(members, func(m any) bool { return m.(map[string]any)["user_id"] == "bob" && m.(map[string]any)["holds"] == true }) {
		t.Errorf("members = %v", members)
	}
	h.want("carol", "POST", "/v1/work-items/"+item+"/stage", map[string]any{}, http.StatusOK)
	h.want("carol", "POST", "/v1/work-items/"+item+"/advance", map[string]any{"result": "pass"}, http.StatusOK)
	h.wantStage("carol", item, "Review", "bob", 0)

	// Withdrawing leaves what he holds with him, and stops new items.
	h.want("bob", "DELETE", "/v1/lists/"+list+"/holding", nil, http.StatusNoContent)
	h.wantStage("carol", item, "Review", "bob", 0)
	other := h.want("carol", "POST", "/v1/work-items", map[string]any{"title": "Doc 2", "list_id": list}, http.StatusCreated)["id"].(string)
	h.want("carol", "POST", "/v1/work-items/"+other+"/stage", map[string]any{}, http.StatusOK)
	h.want("carol", "POST", "/v1/work-items/"+other+"/advance", map[string]any{"result": "pass"}, http.StatusOK)
	h.wantStage("carol", other, "", "carol", 0)

	// An owner holds without asking, and keeps holding once made a member.
	h.want("carol", "PUT", "/v1/lists/"+list+"/members/bob", map[string]any{"role": "owner"}, http.StatusNoContent)
	h.want("carol", "PUT", "/v1/lists/"+list+"/members/bob", map[string]any{"role": "member"}, http.StatusNoContent)
	h.want("carol", "POST", "/v1/work-items/"+other+"/stage", map[string]any{}, http.StatusOK)
	h.want("carol", "POST", "/v1/work-items/"+other+"/advance", map[string]any{"result": "pass"}, http.StatusOK)
	h.wantStage("carol", other, "Review", "bob", 0)
}

func TestATaskMadeElsewhereJoinsAListAndItsWorkflow(t *testing.T) {
	h := newHarness(t)
	list, _ := h.workflowTeam()
	h.want("eve", "PUT", "/v1/admin/agents/acc_lead/intake", map[string]any{"lists": []string{list}}, http.StatusOK)
	h.want("carol", "PUT", "/v1/lists/"+list+"/workflow", map[string]any{"auto_start": true, "stages": []map[string]any{
		{"id": "stg_build", "name": "Build", "holder": "acc_lead"},
	}}, http.StatusOK)
	item := h.want("carol", "POST", "/v1/work-items", map[string]any{"title": "From a conversation"}, http.StatusCreated)["id"].(string)
	// Only an owner moves it, and only onto a list they're on.
	h.wantError("bob", "PUT", "/v1/work-items/"+item+"/list", map[string]any{"list_id": list}, http.StatusNotFound, "not_found")
	other := h.want("bob", "POST", "/v1/lists", map[string]any{"name": "Bob's"}, http.StatusCreated)["id"].(string)
	h.wantError("carol", "PUT", "/v1/work-items/"+item+"/list", map[string]any{"list_id": other}, http.StatusNotFound, "not_found")

	moved := h.want("carol", "PUT", "/v1/work-items/"+item+"/list", map[string]any{"list_id": list}, http.StatusOK)
	if moved["list_id"] != list || moved["stage"] != "stg_build" {
		t.Fatalf("moved = %v", moved)
	}
	h.wantStage("carol", item, "Build", "acc_lead", 0)

	// Taking an item off its list takes it off the workflow; its owner keeps it.
	h.want("carol", "PUT", "/v1/lists/"+list+"/workflow", map[string]any{"stages": []map[string]any{
		{"id": "stg_build", "name": "Build", "holder": "carol"},
	}}, http.StatusOK)
	mine := h.want("carol", "POST", "/v1/work-items", map[string]any{"title": "Mine", "list_id": list}, http.StatusCreated)["id"].(string)
	h.want("carol", "POST", "/v1/work-items/"+mine+"/stage", map[string]any{}, http.StatusOK)
	h.wantStage("carol", mine, "Build", "carol", 0)
	off := h.want("carol", "PUT", "/v1/work-items/"+mine+"/list", map[string]any{"list_id": ""}, http.StatusOK)
	if off["list_id"] != nil || off["stage"] != nil || !slices.Equal(strs(off["owners"]), []string{"carol"}) {
		t.Errorf("off = %v", off)
	}
	events := h.want("carol", "GET", "/v1/work-items/"+mine+"/events", nil, http.StatusOK)["events"].([]any)
	kinds := []string{}
	for _, e := range events {
		kinds = append(kinds, e.(map[string]any)["kind"].(string))
	}
	if !slices.Contains(kinds, "workflow.left") || kinds[len(kinds)-1] != "item.list_changed" {
		t.Errorf("events = %v", kinds)
	}
}
