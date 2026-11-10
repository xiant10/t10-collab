package server

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"t10-collab/internal/sweep"
)

func (h *harness) list(user string, members ...string) string {
	h.t.Helper()
	id := h.want(user, "POST", "/v1/lists", map[string]string{"name": "Team"}, http.StatusCreated)["id"].(string)
	for _, m := range members {
		h.want(user, "PUT", "/v1/lists/"+id+"/members/"+m, nil, http.StatusNoContent)
	}
	return id
}

func (h *harness) listItem(user, list string, unowned bool) string {
	h.t.Helper()
	body := map[string]any{"title": "Backlog", "list_id": list, "unowned": unowned}
	return h.want(user, "POST", "/v1/work-items", body, http.StatusCreated)["id"].(string)
}

func (h *harness) members(reader, list string) map[string]string {
	h.t.Helper()
	out := map[string]string{}
	for _, raw := range h.want(reader, "GET", "/v1/lists/"+list, nil, http.StatusOK)["members"].([]any) {
		m := raw.(map[string]any)
		out[m["user_id"].(string)] = m["source"].(string)
	}
	return out
}

func (h *harness) setRole(user, role string) {
	h.t.Helper()
	h.org.mu.Lock()
	defer h.org.mu.Unlock()
	m := h.org.members[acme+"/"+user]
	m.Role = role
	h.org.members[acme+"/"+user] = m
}

func TestListMembersSeeAndEditEveryItem(t *testing.T) {
	h := newHarness(t)
	list := h.list("ada", "bob")
	item := h.listItem("ada", list, false)

	h.want("bob", "GET", "/v1/work-items/"+item, nil, http.StatusOK)
	h.want("bob", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 1, "status": "doing"}, http.StatusOK)
	h.want("bob", "POST", "/v1/work-items/"+item+"/comments", map[string]string{"body": "on it"}, http.StatusCreated)
	h.wantError("bob", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 2, "status": "cancelled"}, http.StatusForbidden, "forbidden")
	h.wantError("dan", "GET", "/v1/work-items/"+item, nil, http.StatusNotFound, "not_found")

	found := h.want("bob", "GET", "/v1/work-items?view=list&list="+list, nil, http.StatusOK)["items"].([]any)
	if len(found) != 1 {
		t.Errorf("view=list = %v", found)
	}
	if got := h.want("dan", "GET", "/v1/work-items?view=list&list="+list, nil, http.StatusOK)["items"].([]any); len(got) != 0 {
		t.Errorf("an outsider saw %v", got)
	}
	h.wantError("bob", "GET", "/v1/work-items?view=list", nil, http.StatusBadRequest, "invalid_request")

	// Removal moves no items, and ends access that came only from the list.
	h.want("ada", "DELETE", "/v1/lists/"+list+"/members/bob", nil, http.StatusNoContent)
	h.wantError("bob", "GET", "/v1/work-items/"+item, nil, http.StatusNotFound, "not_found")
}

func TestOnlyMembersUseAList(t *testing.T) {
	h := newHarness(t)
	list := h.list("ada", "bob")
	body := map[string]any{"title": "x", "list_id": list}
	h.wantError("dan", "POST", "/v1/work-items", body, http.StatusNotFound, "not_found")
	h.wantError("dan", "GET", "/v1/lists/"+list, nil, http.StatusNotFound, "not_found")
	h.wantError("dan", "PATCH", "/v1/lists/"+list, map[string]string{"name": "x"}, http.StatusNotFound, "not_found")
	h.wantError("bob", "PATCH", "/v1/lists/"+list, map[string]string{"name": "x"}, http.StatusForbidden, "forbidden")
	h.wantError("bob", "PUT", "/v1/lists/"+list+"/members/dan", nil, http.StatusForbidden, "forbidden")
	h.wantError("ada", "PUT", "/v1/lists/"+list+"/members/gus", nil, http.StatusUnprocessableEntity, "invalid_recipient")
	h.wantError("ada", "POST", "/v1/work-items", map[string]any{"title": "x", "unowned": true}, http.StatusBadRequest, "invalid_request")

	if got, _ := h.request(globex+"/gus", "GET", "/v1/lists/"+list, nil); got != http.StatusNotFound {
		t.Errorf("read a list across tenants: %d", got)
	}
	_, err := h.pool.Exec(context.Background(),
		"INSERT INTO collab.work_items (tenant_id, id, title, creator_id, list_id) VALUES ($1, 'wi_x', 'x', 'gus', $2)", globex, list)
	if err == nil {
		t.Error("the database put an item on another tenant's list")
	}
}

func TestAListKeepsAnOwner(t *testing.T) {
	h := newHarness(t)
	list := h.list("ada", "bob")
	h.wantError("ada", "DELETE", "/v1/lists/"+list+"/members/ada", nil, http.StatusConflict, "last_list_owner")
	h.wantError("ada", "PUT", "/v1/lists/"+list+"/members/ada", map[string]string{"role": "member"}, http.StatusConflict, "last_list_owner")
	h.want("ada", "PUT", "/v1/lists/"+list+"/members/bob", map[string]string{"role": "owner"}, http.StatusNoContent)
	h.want("ada", "DELETE", "/v1/lists/"+list+"/members/ada", nil, http.StatusNoContent)
	h.wantError("ada", "GET", "/v1/lists/"+list, nil, http.StatusNotFound, "not_found")
}

func TestArchivedListTakesNoNewItems(t *testing.T) {
	h := newHarness(t)
	list := h.list("ada")
	item := h.listItem("ada", list, false)
	h.want("ada", "PATCH", "/v1/lists/"+list, map[string]any{"archived": true}, http.StatusOK)
	h.wantError("ada", "POST", "/v1/work-items", map[string]any{"title": "x", "list_id": list}, http.StatusConflict, "list_archived")
	h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK)
	if got := h.want("ada", "GET", "/v1/lists", nil, http.StatusOK)["lists"].([]any); len(got) != 0 {
		t.Errorf("active lists = %v", got)
	}
	if got := h.want("ada", "GET", "/v1/lists?archived=true", nil, http.StatusOK)["lists"].([]any); len(got) != 1 {
		t.Errorf("archived lists = %v", got)
	}
}

func TestClaim(t *testing.T) {
	h := newHarness(t)
	list := h.list("ada", "bob", "dan")
	item := h.listItem("ada", list, true)
	h.wantOwners("bob", item, []string{}, []string{})

	h.wantError("eve", "POST", "/v1/work-items/"+item+"/claim", nil, http.StatusNotFound, "not_found")
	stale := h.send("dan", item, "eve", "transfer")
	h.want("bob", "POST", "/v1/work-items/"+item+"/claim", nil, http.StatusOK)
	h.wantOwners("bob", item, []string{"bob"}, []string{})
	h.wantError("dan", "POST", "/v1/work-items/"+item+"/claim", nil, http.StatusConflict, "already_owned")
	// A claim must end the offer, or it could take the item from Bob.
	h.wantError("eve", "POST", "/v1/offers/"+stale+"/accept", nil, http.StatusConflict, "offer_not_pending")
	h.wantError("dan", "POST", "/v1/work-items/"+item+"/send", map[string]string{"to": "eve"}, http.StatusForbidden, "forbidden")

	// A list's last owner can leave, and the item is claimable again.
	h.want("bob", "DELETE", "/v1/work-items/"+item+"/owners/me", nil, http.StatusNoContent)
	h.want("dan", "POST", "/v1/work-items/"+item+"/claim", nil, http.StatusOK)

	closed := h.listItem("ada", list, true)
	h.want("ada", "PATCH", "/v1/work-items/"+closed, map[string]any{"version": 1, "status": "done"}, http.StatusOK)
	h.wantError("bob", "POST", "/v1/work-items/"+closed+"/claim", nil, http.StatusConflict, "item_closed")

	notOnList := h.item("ada")
	h.want("ada", "PUT", "/v1/work-items/"+notOnList+"/collaborators/bob", nil, http.StatusNoContent)
	h.wantError("bob", "POST", "/v1/work-items/"+notOnList+"/claim", nil, http.StatusForbidden, "forbidden")
}

func TestTwoPeopleClaimingCannotBothWin(t *testing.T) {
	h := newHarness(t)
	list := h.list("ada", "bob")
	for range 15 {
		item := h.listItem("ada", list, true)
		var wg sync.WaitGroup
		statuses := make([]int, 2)
		for i, user := range []string{"ada", "bob"} {
			wg.Go(func() { statuses[i], _ = h.as(user, "POST", "/v1/work-items/"+item+"/claim", nil) })
		}
		wg.Wait()
		slices.Sort(statuses)
		if statuses[0] != http.StatusOK || statuses[1] != http.StatusConflict {
			t.Fatalf("statuses = %v", statuses)
		}
	}
}

func TestAnAcceptedOfferEndsOtherMembersOffersOnAnUnownedItem(t *testing.T) {
	h := newHarness(t)
	list := h.list("ada", "bob")
	item := h.listItem("ada", list, true)
	first := h.send("ada", item, "dan", "share")
	second := h.send("bob", item, "eve", "share")
	h.want("dan", "POST", "/v1/offers/"+first+"/accept", nil, http.StatusOK)
	h.wantError("eve", "POST", "/v1/offers/"+second+"/accept", nil, http.StatusConflict, "offer_not_pending")
}

func TestDeleteOfAnUnownedItem(t *testing.T) {
	h := newHarness(t)
	list := h.list("ada", "bob")
	item := h.listItem("ada", list, true)
	h.wantError("bob", "DELETE", "/v1/work-items/"+item, nil, http.StatusForbidden, "forbidden")
	h.want("ada", "DELETE", "/v1/work-items/"+item, nil, http.StatusNoContent)
}

func TestManagerVisibility(t *testing.T) {
	h := newHarness(t)
	h.org.direct[acme+"/carol"] = []string{"bob"}
	h.org.reports[acme+"/carol"] = []string{"bob", "ada"}
	direct := h.item("bob")
	deep := h.item("ada")
	collaborating := h.item("dan")
	h.want("dan", "PUT", "/v1/work-items/"+collaborating+"/collaborators/bob", nil, http.StatusNoContent)
	declined := h.item("dan")
	h.want("bob", "POST", "/v1/offers/"+h.send("dan", declined, "bob", "share")+"/decline", nil, http.StatusOK)

	ids := func() []string {
		out := []string{}
		for _, i := range h.want("carol", "GET", "/v1/work-items?view=reports", nil, http.StatusOK)["items"].([]any) {
			out = append(out, i.(map[string]any)["id"].(string))
		}
		slices.Sort(out)
		return out
	}
	sorted := func(values ...string) []string {
		slices.Sort(values)
		return values
	}

	// Off by default, and the ordinary paths make no org read.
	h.want("carol", "GET", "/v1/work-items", nil, http.StatusOK)
	h.want("carol", "GET", "/v1/work-items/"+direct+"x", nil, http.StatusNotFound)
	if len(ids()) != 0 {
		t.Error("visibility is on by default")
	}
	if h.org.calls != 0 {
		t.Errorf("made %d org reads with visibility off", h.org.calls)
	}
	h.wantError("carol", "GET", "/v1/work-items/"+direct, nil, http.StatusNotFound, "not_found")
	if got := h.want("bob", "GET", "/v1/settings", nil, http.StatusOK)["manager_visibility"]; got != "none" {
		t.Errorf("default = %v", got)
	}

	// Only a tenant admin changes it, and it's confirmed with auth.
	body := map[string]string{"manager_visibility": "direct_reports"}
	h.wantError("carol", "PUT", "/v1/admin/settings", body, http.StatusForbidden, "forbidden")
	h.setRole("carol", "admin")
	h.wantError("carol", "PUT", "/v1/admin/settings", map[string]string{"manager_visibility": "everyone"}, http.StatusBadRequest, "invalid_request")
	h.org.down = true
	h.wantError("carol", "PUT", "/v1/admin/settings", body, http.StatusServiceUnavailable, "org_unavailable")
	h.org.down = false
	h.want("carol", "PUT", "/v1/admin/settings", body, http.StatusOK)
	var kind string
	if err := h.pool.QueryRow(context.Background(),
		"SELECT kind FROM collab.events WHERE item_id IS NULL AND actor_user_id = 'carol'").Scan(&kind); err != nil || kind != "settings.changed" {
		t.Errorf("event = %q, %v", kind, err)
	}

	if got := ids(); !slices.Equal(got, []string{direct}) {
		t.Errorf("direct_reports = %v", got)
	}
	h.want("carol", "GET", "/v1/work-items/"+direct, nil, http.StatusOK)
	h.want("carol", "GET", "/v1/work-items/"+direct+"/events", nil, http.StatusOK)
	h.want("carol", "GET", "/v1/work-items/"+direct+"/comments", nil, http.StatusOK)
	h.wantError("carol", "GET", "/v1/work-items/"+deep, nil, http.StatusNotFound, "not_found")

	// Read only.
	h.wantError("carol", "PATCH", "/v1/work-items/"+direct, map[string]any{"version": 1, "title": "x"}, http.StatusNotFound, "not_found")
	h.wantError("carol", "POST", "/v1/work-items/"+direct+"/comments", map[string]string{"body": "x"}, http.StatusNotFound, "not_found")
	h.wantError("carol", "POST", "/v1/work-items/"+direct+"/send", map[string]string{"to": "dan"}, http.StatusNotFound, "not_found")

	h.want("carol", "PUT", "/v1/admin/settings", map[string]string{"manager_visibility": "all_reports"}, http.StatusOK)
	if got := ids(); !slices.Equal(got, sorted(direct, deep)) {
		t.Errorf("all_reports = %v", got)
	}
	// Items a report only collaborates on, or declined, stay out.
	h.wantError("carol", "GET", "/v1/work-items/"+collaborating, nil, http.StatusNotFound, "not_found")
	h.wantError("carol", "GET", "/v1/work-items/"+declined, nil, http.StatusNotFound, "not_found")

	// Fails closed, and leaves everything else working.
	own := h.item("carol")
	h.org.down = true
	h.wantError("carol", "GET", "/v1/work-items/"+direct, nil, http.StatusNotFound, "not_found")
	h.wantError("carol", "GET", "/v1/work-items?view=reports", nil, http.StatusServiceUnavailable, "org_unavailable")
	h.want("carol", "GET", "/v1/work-items/"+own, nil, http.StatusOK)
	h.want("carol", "GET", "/v1/work-items?view=owned", nil, http.StatusOK)
}

func TestLinkingFillsTheList(t *testing.T) {
	h := newHarness(t)
	h.org.units[acme+"/org_a"] = []string{"bob", "dan"}
	list := h.list("ada", "eve")

	h.wantError("bob", "GET", "/v1/lists/"+list, nil, http.StatusNotFound, "not_found")
	h.wantError("ada", "PATCH", "/v1/lists/"+list, map[string]string{"org_unit_id": "org_nope"}, http.StatusUnprocessableEntity, "invalid_unit")
	h.org.down = true
	h.wantError("ada", "PATCH", "/v1/lists/"+list, map[string]string{"org_unit_id": "org_a"}, http.StatusServiceUnavailable, "org_unavailable")
	h.org.down = false
	h.want("ada", "PATCH", "/v1/lists/"+list, map[string]string{"org_unit_id": "org_a"}, http.StatusOK)
	got := h.members("ada", list)
	if got["bob"] != "org" || got["dan"] != "org" || got["ada"] != "manual" || got["eve"] != "manual" {
		t.Fatalf("members = %v", got)
	}

	h.wantError("ada", "DELETE", "/v1/lists/"+list+"/members/bob", nil, http.StatusConflict, "managed_by_unit")
	h.want("ada", "PUT", "/v1/lists/"+list+"/members/bob", nil, http.StatusNoContent)
	if got := h.members("ada", list); got["bob"] != "manual" {
		t.Errorf("a PUT left %v", got)
	}

	// Saving the same link again changes nothing.
	h.want("ada", "PATCH", "/v1/lists/"+list, map[string]string{"org_unit_id": "org_a"}, http.StatusOK)
	if got := h.members("ada", list); got["dan"] != "org" {
		t.Errorf("relinking left %v", got)
	}

	// Clearing it keeps everyone.
	h.want("ada", "PATCH", "/v1/lists/"+list, map[string]any{"org_unit_id": nil}, http.StatusOK)
	if got := h.members("ada", list); got["dan"] != "manual" {
		t.Errorf("clearing left %v", got)
	}
}

func TestSweepKeepsLinkedListsCurrent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := sweep.New(h.pool, h.org)
	h.org.units[acme+"/org_a"] = []string{"bob", "dan"}
	list := h.list("ada", "eve")
	h.want("ada", "PATCH", "/v1/lists/"+list, map[string]string{"org_unit_id": "org_a"}, http.StatusOK)

	h.org.units[acme+"/org_a"] = []string{"dan", "carol"}
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	got := h.members("ada", list)
	if _, ok := got["bob"]; ok || got["carol"] != "org" || got["eve"] != "manual" {
		t.Fatalf("after a change: %v", got)
	}

	// A failed read never removes anyone.
	h.org.down = true
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	h.org.down = false
	if got := h.members("ada", list); got["dan"] != "org" {
		t.Fatalf("after a failed read: %v", got)
	}

	// An empty unit really is empty. Hand-added members stay.
	h.org.units[acme+"/org_a"] = []string{}
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.members("ada", list); len(got) != 2 || got["eve"] != "manual" {
		t.Fatalf("after emptying: %v", got)
	}

	// A deleted unit clears the link, tells the owners, and keeps its members.
	h.org.units[acme+"/org_a"] = []string{"dan"}
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	delete(h.org.units, acme+"/org_a")
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	out := h.want("ada", "GET", "/v1/lists/"+list, nil, http.StatusOK)
	if out["org_unit_id"] != nil {
		t.Errorf("link = %v", out["org_unit_id"])
	}
	if got := h.members("ada", list); got["dan"] != "manual" {
		t.Errorf("after deletion: %v", got)
	}
	kinds := []string{}
	for _, n := range h.want("ada", "GET", "/v1/notifications", nil, http.StatusOK)["notifications"].([]any) {
		kinds = append(kinds, n.(map[string]any)["event"].(map[string]any)["kind"].(string))
	}
	if !slices.Contains(kinds, "list.unit_deleted") {
		t.Errorf("notifications = %v", kinds)
	}
}

func TestOnlyOneReplicaSweeps(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	other, err := h.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if _, err := other.Exec(ctx, "SELECT pg_advisory_lock(7410_0003)"); err != nil {
		t.Fatal(err)
	}
	defer other.Exec(ctx, "SELECT pg_advisory_unlock(7410_0003)")

	h.org.units[acme+"/org_a"] = []string{"bob"}
	list := h.list("ada")
	h.want("ada", "PATCH", "/v1/lists/"+list, map[string]string{"org_unit_id": "org_a"}, http.StatusOK)
	h.org.units[acme+"/org_a"] = []string{}
	if err := sweep.New(h.pool, h.org).Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.members("ada", list); got["bob"] != "org" {
		t.Errorf("swept while another replica held the lock: %v", got)
	}
}

func TestAnItemSaysWhatTheCallerCanDo(t *testing.T) {
	h := newHarness(t)
	list := h.list("ada", "bob")
	item := h.listItem("ada", list, true)
	can := func(user string) map[string]any {
		t.Helper()
		return h.want(user, "GET", "/v1/work-items/"+item, nil, http.StatusOK)["can"].(map[string]any)
	}
	want := func(user string, expected map[string]bool) {
		t.Helper()
		got := can(user)
		for k, v := range expected {
			if got[k] != v {
				t.Errorf("%s can %s = %v, want %v", user, k, got[k], v)
			}
		}
	}

	// A list member who didn't create it can work on it and take it, not delete it.
	want("bob", map[string]bool{"edit": true, "claim": true, "send": true, "delete": false, "cancel": false, "manage_people": false})
	want("ada", map[string]bool{"edit": true, "claim": true, "delete": true, "cancel": true, "manage_people": true})

	h.want("bob", "POST", "/v1/work-items/"+item+"/claim", nil, http.StatusOK)
	want("bob", map[string]bool{"claim": false, "delete": true, "cancel": true, "send": true})
	want("ada", map[string]bool{"claim": false, "delete": false, "send": false, "edit": true})

	// Nobody claims a closed item.
	closed := h.listItem("ada", list, true)
	h.want("ada", "PATCH", "/v1/work-items/"+closed, map[string]any{"version": 1, "status": "done"}, http.StatusOK)
	if h.want("bob", "GET", "/v1/work-items/"+closed, nil, http.StatusOK)["can"].(map[string]any)["claim"] != false {
		t.Error("a closed item can be claimed")
	}
}

func TestClaimableItemsAndListCounts(t *testing.T) {
	h := newHarness(t)
	list := h.list("ada", "bob")
	open := h.listItem("ada", list, true)
	taken := h.listItem("ada", list, true)
	h.want("ada", "POST", "/v1/work-items/"+taken+"/claim", nil, http.StatusOK)
	done := h.listItem("ada", list, true)
	h.want("ada", "PATCH", "/v1/work-items/"+done, map[string]any{"version": 1, "status": "done"}, http.StatusOK)
	other := h.list("dan")
	h.listItem("dan", other, true)

	ids := func(user, query string) []string {
		t.Helper()
		out := []string{}
		for _, i := range h.want(user, "GET", "/v1/work-items?"+query, nil, http.StatusOK)["items"].([]any) {
			out = append(out, i.(map[string]any)["id"].(string))
		}
		return out
	}
	if got := ids("bob", "view=claimable"); !slices.Equal(got, []string{open}) {
		t.Errorf("claimable = %v, want %v", got, []string{open})
	}

	var counts map[string]any
	for _, l := range h.want("bob", "GET", "/v1/lists", nil, http.StatusOK)["lists"].([]any) {
		if l.(map[string]any)["id"] == list {
			counts = l.(map[string]any)["counts"].(map[string]any)
		}
	}
	if counts["open"] != 2.0 || counts["unclaimed"] != 1.0 || counts["members"] != 2.0 {
		t.Errorf("counts = %v", counts)
	}

	// An archived list offers nothing to claim.
	h.want("ada", "PATCH", "/v1/lists/"+list, map[string]any{"archived": true}, http.StatusOK)
	if got := ids("bob", "view=claimable"); len(got) != 0 {
		t.Errorf("claimable on an archived list = %v", got)
	}
}

func TestSearchAndDueFiltersRunBeforePagination(t *testing.T) {
	h := newHarness(t)
	create := func(title, due string) string {
		body := map[string]any{"title": title}
		if due != "" {
			body["due_at"] = due
		}
		return h.want("ada", "POST", "/v1/work-items", body, http.StatusCreated)["id"].(string)
	}
	report := create("Quarterly Report", "2030-01-10T00:00:00Z")
	create("Lunch", "2030-01-11T00:00:00Z")
	percent := create("Raise to 100%", "")
	for range 3 {
		create("Filler", "")
	}
	count := func(query string) int {
		t.Helper()
		return len(h.want("ada", "GET", "/v1/work-items?view=owned&limit=2&"+query, nil, http.StatusOK)["items"].([]any))
	}
	if got := h.want("ada", "GET", "/v1/work-items?view=owned&limit=1&q=report", nil, http.StatusOK)["items"].([]any); len(got) != 1 || got[0].(map[string]any)["id"] != report {
		t.Errorf("q=report = %v", got)
	}
	if got := h.want("ada", "GET", "/v1/work-items?view=owned&q=%25", nil, http.StatusOK)["items"].([]any); len(got) != 1 || got[0].(map[string]any)["id"] != percent {
		t.Errorf("q=%% matched %d items, want only the one with a percent sign", len(got))
	}
	if got := count("due_before=2030-01-11T00:00:00Z"); got != 1 {
		t.Errorf("due_before = %d", got)
	}
	if got := count("due_from=2030-01-11T00:00:00Z&due_before=2030-01-12T00:00:00Z"); got != 1 {
		t.Errorf("one day = %d", got)
	}
	h.want("ada", "PATCH", "/v1/work-items/"+report, map[string]any{"version": 1, "status": "done"}, http.StatusOK)
	if got := len(h.want("ada", "GET", "/v1/work-items?view=owned&status=open", nil, http.StatusOK)["items"].([]any)); got != 5 {
		t.Errorf("status=open = %d", got)
	}
	h.wantError("ada", "GET", "/v1/work-items?due_before=tomorrow", nil, http.StatusBadRequest, "invalid_request")
}

func TestAMemberCanLeaveAList(t *testing.T) {
	h := newHarness(t)
	list := h.list("ada", "bob", "dan")
	h.wantError("bob", "DELETE", "/v1/lists/"+list+"/members/dan", nil, http.StatusForbidden, "forbidden")
	h.want("bob", "DELETE", "/v1/lists/"+list+"/members/bob", nil, http.StatusNoContent)
	h.wantError("bob", "GET", "/v1/lists/"+list, nil, http.StatusNotFound, "not_found")
	// The last owner still can't leave.
	h.wantError("ada", "DELETE", "/v1/lists/"+list+"/members/ada", nil, http.StatusConflict, "last_list_owner")
}
