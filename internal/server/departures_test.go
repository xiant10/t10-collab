package server

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"t10-collab/internal/sweep"
)

func (h *harness) sweep() {
	h.t.Helper()
	if err := sweep.New(h.pool, h.org).Once(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) suspend(user string) {
	h.org.mu.Lock()
	defer h.org.mu.Unlock()
	m := h.org.members[acme+"/"+user]
	m.UserStatus = "suspended"
	h.org.members[acme+"/"+user] = m
}

func (h *harness) managedBy(user, manager string) {
	h.org.mu.Lock()
	defer h.org.mu.Unlock()
	m := h.org.members[acme+"/"+user]
	m.ManagerUserID = manager
	h.org.members[acme+"/"+user] = m
}

// inbox returns the IDs of a person's pending offers, and who each is from.
func (h *harness) inbox(user string) (ids, from []string) {
	h.t.Helper()
	for _, raw := range h.want(user, "GET", "/v1/offers?box=inbox", nil, http.StatusOK)["offers"].([]any) {
		o := raw.(map[string]any)
		ids, from = append(ids, o["id"].(string)), append(from, o["from_user_id"].(string))
	}
	return ids, from
}

func (h *harness) wantInbox(user string, n int) string {
	h.t.Helper()
	ids, _ := h.inbox(user)
	if len(ids) != n {
		h.t.Fatalf("%s has %d offers, want %d", user, len(ids), n)
	}
	if n == 0 {
		return ""
	}
	return ids[0]
}

func (h *harness) queue(admin string) map[string]string {
	h.t.Helper()
	out := map[string]string{}
	for _, raw := range h.want(admin, "GET", "/v1/admin/departures", nil, http.StatusOK)["items"].([]any) {
		q := raw.(map[string]any)
		out[q["id"].(string)] = q["reason"].(string)
	}
	return out
}

func (h *harness) listQueue(admin string) map[string]string {
	h.t.Helper()
	out := map[string]string{}
	for _, raw := range h.want(admin, "GET", "/v1/admin/departures/lists", nil, http.StatusOK)["lists"].([]any) {
		q := raw.(map[string]any)
		out[q["id"].(string)] = q["reason"].(string)
	}
	return out
}

// expire makes every pending offer due, so a test needn't wait 14 days.
func (h *harness) expire() {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		"UPDATE collab.offers SET expires_at = now() - interval '1 second' WHERE state = 'pending'"); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) owners(item string) []string {
	h.t.Helper()
	found := []string{}
	rows, err := h.pool.Query(context.Background(), "SELECT user_id FROM collab.work_item_owners WHERE item_id = $1 ORDER BY user_id", item)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.t.Fatal(err)
		}
		found = append(found, id)
	}
	return found
}

func TestAnotherOwnerKeepsTheItem(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", item, "bob", "share")+"/accept", nil, http.StatusOK)
	h.org.set("inactive", acme+"/ada")
	h.sweep()
	if got := h.owners(item); !slices.Equal(got, []string{"bob"}) {
		t.Fatalf("owners = %v", got)
	}
	h.wantInbox("ada", 0)
	h.wantInbox("carol", 0)
}

func TestAListItemBecomesClaimable(t *testing.T) {
	h := newHarness(t)
	list := h.list("carol", "ada", "bob")
	item := h.listItem("ada", list, false)
	h.org.set("inactive", acme+"/ada")
	h.sweep()
	h.wantOwners("bob", item, []string{}, []string{})
	h.want("bob", "POST", "/v1/work-items/"+item+"/claim", nil, http.StatusOK)
}

func TestTheCreatorIsOfferedTheItemAndTheOwnerLeavesAfter(t *testing.T) {
	h := newHarness(t)
	item := h.item("bob")
	h.want("ada", "POST", "/v1/offers/"+h.send("bob", item, "ada", "transfer")+"/accept", nil, http.StatusOK)
	h.org.set("inactive", acme+"/ada")
	h.sweep()
	h.sweep() // A second pass must not offer it twice.
	ids, from := h.inbox("bob")
	if len(ids) != 1 || from[0] != "system:sweep" {
		t.Fatalf("offers %v from %v", ids, from)
	}
	if got := h.owners(item); !slices.Equal(got, []string{"ada"}) {
		t.Fatalf("an item off a list lost its last owner: %v", got)
	}
	h.want("bob", "POST", "/v1/offers/"+ids[0]+"/accept", nil, http.StatusOK)
	h.sweep()
	if got := h.owners(item); !slices.Equal(got, []string{"bob"}) {
		t.Fatalf("owners = %v", got)
	}
	var strays int
	if err := h.pool.QueryRow(context.Background(), "SELECT count(*) FROM collab.notifications WHERE user_id = 'system:sweep'").Scan(&strays); err != nil || strays != 0 {
		t.Errorf("the sweep got %d notifications: %v", strays, err)
	}
}

func TestADeclineGoesStraightToTheQueue(t *testing.T) {
	h := newHarness(t)
	h.setRole("eve", "admin")
	h.managedBy("ada", "carol")
	item := h.item("bob")
	h.want("ada", "POST", "/v1/offers/"+h.send("bob", item, "ada", "transfer")+"/accept", nil, http.StatusOK)
	h.org.set("inactive", acme+"/ada")
	h.setRole("eve", "admin")
	h.managedBy("ada", "carol")
	h.sweep()
	h.want("bob", "POST", "/v1/offers/"+h.wantInbox("bob", 1)+"/decline", nil, http.StatusOK)
	h.sweep()
	h.wantInbox("carol", 0) // The manager isn't asked after a real no.
	if got := h.queue("eve"); got[item] != "no_successor" {
		t.Fatalf("queue = %v", got)
	}
}

func TestSilenceMovesOnToTheManagerAndThenTheQueue(t *testing.T) {
	h := newHarness(t)
	h.setRole("eve", "admin")
	item := h.item("bob")
	h.want("ada", "POST", "/v1/offers/"+h.send("bob", item, "ada", "transfer")+"/accept", nil, http.StatusOK)
	h.org.set("inactive", acme+"/ada")
	h.managedBy("ada", "carol")
	h.sweep()
	h.wantInbox("bob", 1)
	h.expire()
	h.sweep()
	h.wantInbox("bob", 0)
	h.wantInbox("carol", 1)
	h.expire()
	h.sweep()
	h.wantInbox("bob", 0)
	h.wantInbox("carol", 0)
	if got := h.queue("eve"); got[item] != "no_successor" {
		t.Fatalf("queue = %v", got)
	}
}

func TestALiveOfferIsLeftInPlaceAndFallsThroughIfItFails(t *testing.T) {
	h := newHarness(t)
	item := h.item("bob")
	h.want("ada", "POST", "/v1/offers/"+h.send("bob", item, "ada", "transfer")+"/accept", nil, http.StatusOK)
	offer := h.send("ada", item, "dan", "transfer")
	h.org.set("inactive", acme+"/ada")
	h.sweep()
	// The one-offer limit isn't what stops this. The creator just isn't needed yet.
	h.wantInbox("bob", 0)
	found := h.want("dan", "GET", "/v1/offers?box=inbox", nil, http.StatusOK)["offers"].([]any)
	if found[0].(map[string]any)["expires_at"] == nil {
		t.Fatal("a held offer has no expiry")
	}
	h.want("dan", "POST", "/v1/offers/"+offer+"/decline", nil, http.StatusOK)
	h.sweep()
	h.wantInbox("bob", 1)
}

func TestAnOfferFromSomeoneWhoLeftCanStillBeAccepted(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	offer := h.send("ada", item, "dan", "transfer")
	h.org.set("inactive", acme+"/ada")
	h.sweep()
	h.want("dan", "POST", "/v1/offers/"+offer+"/accept", nil, http.StatusOK)
	if got := h.owners(item); !slices.Equal(got, []string{"dan"}) {
		t.Fatalf("owners = %v", got)
	}
}

func TestAnOfferToSomeoneWhoLeftIsVoidedAndTheSenderTold(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	offer := h.send("ada", item, "dan", "transfer")
	h.org.set("inactive", acme+"/dan")
	h.sweep()
	sent := h.want("ada", "GET", "/v1/offers?box=sent", nil, http.StatusOK)["offers"].([]any)
	if got := sent[0].(map[string]any); got["id"] != offer || got["state"] != "voided" {
		t.Fatalf("offer = %v", got)
	}
	kinds := []string{}
	for _, n := range h.want("ada", "GET", "/v1/notifications", nil, http.StatusOK)["notifications"].([]any) {
		kinds = append(kinds, n.(map[string]any)["event"].(map[string]any)["kind"].(string))
	}
	if !slices.Contains(kinds, "offer.voided") {
		t.Errorf("notifications = %v", kinds)
	}
}

func TestOnlyAnExplicitStatusCountsAsLeaving(t *testing.T) {
	h := newHarness(t)
	h.setRole("eve", "admin")
	item := h.item("ada")
	other := h.item("bob")
	h.want("ada", "POST", "/v1/offers/"+h.send("bob", other, "ada", "share")+"/accept", nil, http.StatusOK)

	h.org.set("inactive", acme+"/ada")
	h.org.down = true
	h.sweep()
	h.org.down = false
	if got := h.owners(other); len(got) != 2 {
		t.Fatalf("a failed read moved work: %v", got)
	}

	h.org.mu.Lock()
	delete(h.org.members, acme+"/ada")
	h.org.mu.Unlock()
	h.sweep()
	if got := h.owners(other); len(got) != 2 {
		t.Fatalf("a missing member counted as leaving: %v", got)
	}
	h.wantInbox("bob", 0)
	if got := h.queue("eve"); len(got) != 0 {
		t.Errorf("queue = %v, item %s", got, item)
	}
}

func TestClosedItemsStayUntilReopened(t *testing.T) {
	h := newHarness(t)
	item := h.item("bob")
	h.want("ada", "POST", "/v1/offers/"+h.send("bob", item, "ada", "transfer")+"/accept", nil, http.StatusOK)
	h.want("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 1, "status": "done"}, http.StatusOK)
	h.org.set("inactive", acme+"/ada")
	h.sweep()
	h.wantInbox("bob", 0)
	h.want("bob", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 2, "status": "todo"}, http.StatusOK)
	h.sweep()
	h.wantInbox("bob", 1)
}

func TestReinstatedOwnerComesOutOfTheQueue(t *testing.T) {
	h := newHarness(t)
	h.setRole("eve", "admin")
	item := h.item("ada")
	h.org.set("inactive", acme+"/ada")
	h.sweep()
	h.setRole("eve", "admin")
	if got := h.queue("eve"); got[item] != "no_successor" {
		t.Fatalf("queue = %v", got)
	}
	h.org.set("active", acme+"/ada")
	h.sweep()
	if got := h.queue("eve"); len(got) != 0 {
		t.Fatalf("queue = %v", got)
	}
	h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK)
	// Leaving a second time works like the first.
	h.org.set("inactive", acme+"/ada")
	h.sweep()
	if got := h.queue("eve"); got[item] != "no_successor" {
		t.Fatalf("second departure: queue = %v", got)
	}
}

func TestASuspendedOwnersWorkStaysAndCanBeReassigned(t *testing.T) {
	h := newHarness(t)
	h.setRole("eve", "admin")
	item := h.item("ada")
	h.suspend("ada")
	h.sweep()
	if got := h.owners(item); !slices.Equal(got, []string{"ada"}) {
		t.Fatalf("owners = %v", got)
	}
	if got := h.queue("eve"); got[item] != "owner_suspended" {
		t.Fatalf("queue = %v", got)
	}
	h.wantError("eve", "GET", "/v1/work-items/"+item, nil, http.StatusNotFound, "not_found")

	// A suspension can end, and the item leaves the queue with no admin.
	h.org.set("active", acme+"/ada")
	h.sweep()
	if got := h.queue("eve"); len(got) != 0 {
		t.Fatalf("queue = %v", got)
	}

	h.suspend("ada")
	h.sweep()
	h.want("eve", "POST", "/v1/admin/departures/"+item+"/reassign", map[string]string{"to": "bob"}, http.StatusOK)
	h.wantOwners("bob", item, []string{"bob"}, []string{"ada"})
	if got := h.queue("eve"); len(got) != 0 {
		t.Fatalf("queue = %v", got)
	}
	_, got := h.request("notify", "GET", "/internal/v1/attention?tenant="+acme+"&user=bob", nil)
	if got["new_assignments"] != float64(1) {
		t.Errorf("attention = %v", got)
	}
}

func TestReassignReachesOnlyTheQueue(t *testing.T) {
	h := newHarness(t)
	h.setRole("eve", "admin")
	ordinary := h.item("bob")
	item := h.item("ada")
	h.org.set("inactive", acme+"/ada")
	h.sweep()
	h.setRole("eve", "admin")
	body := map[string]string{"to": "dan"}
	path := "/v1/admin/departures/" + item + "/reassign"

	h.wantError("bob", "GET", "/v1/admin/departures", nil, http.StatusForbidden, "forbidden")
	h.wantError("bob", "POST", path, body, http.StatusForbidden, "forbidden")
	h.wantError("eve", "POST", "/v1/admin/departures/"+ordinary+"/reassign", body, http.StatusNotFound, "not_found")
	h.wantError("eve", "POST", path, map[string]string{"to": "ada"}, http.StatusUnprocessableEntity, "invalid_recipient")
	h.org.down = true
	h.wantError("eve", "GET", "/v1/admin/departures", nil, http.StatusServiceUnavailable, "org_unavailable")
	h.wantError("eve", "POST", path, body, http.StatusServiceUnavailable, "org_unavailable")
	h.org.down = false

	h.org.set("active", globex+"/gus")
	h.org.mu.Lock()
	gus := h.org.members[globex+"/gus"]
	gus.Role = "admin"
	h.org.members[globex+"/gus"] = gus
	h.org.mu.Unlock()
	if got, _ := h.request(globex+"/gus", "POST", path, map[string]string{"to": "gus"}); got != http.StatusNotFound {
		t.Errorf("reassigned across tenants: %d", got)
	}
	if _, out := h.request(globex+"/gus", "GET", "/v1/admin/departures", nil); len(out["items"].([]any)) != 0 {
		t.Errorf("saw another tenant's queue: %v", out)
	}

	got := h.want("eve", "POST", path, body, http.StatusOK)
	if _, leaked := got["description"]; leaked {
		t.Errorf("reassign showed the item: %v", got)
	}
	h.wantOwners("dan", item, []string{"dan"}, []string{})
	h.wantError("eve", "POST", path, body, http.StatusNotFound, "not_found")
}

func TestListsWithNobodyToManageThemAreQueued(t *testing.T) {
	h := newHarness(t)
	h.setRole("eve", "admin")
	noOwner := h.list("ada", "bob")
	noMembers := h.list("dan")
	h.listItem("dan", noMembers, true)
	finished := h.list("carol")
	done := h.listItem("carol", finished, true)
	h.want("carol", "PATCH", "/v1/work-items/"+done, map[string]any{"version": 1, "status": "done"}, http.StatusOK)
	healthy := h.list("eve", "ada")

	h.org.set("inactive", acme+"/ada", acme+"/dan", acme+"/carol")
	h.setRole("eve", "admin")
	h.sweep()
	got := h.listQueue("eve")
	if got[noOwner] != "no_active_owner" || got[noMembers] != "no_active_members" {
		t.Fatalf("queue = %v", got)
	}
	if _, ok := got[finished]; ok {
		t.Error("a list with nothing left to do was queued")
	}
	if _, ok := got[healthy]; ok {
		t.Error("a list with an owner was queued")
	}
	if m := h.members("eve", healthy); m["ada"] != "" {
		t.Errorf("a departed member stayed on: %v", m)
	}

	path := "/v1/admin/departures/lists/" + noOwner + "/owner"
	h.wantError("bob", "POST", path, map[string]string{"to": "bob"}, http.StatusForbidden, "forbidden")
	h.wantError("eve", "POST", "/v1/admin/departures/lists/"+healthy+"/owner", map[string]string{"to": "bob"}, http.StatusNotFound, "not_found")
	h.want("eve", "POST", path, map[string]string{"to": "bob"}, http.StatusNoContent)
	h.want("bob", "PATCH", "/v1/lists/"+noOwner, map[string]string{"name": "Ours now"}, http.StatusOK)
	if got := h.listQueue("eve"); len(got) != 1 {
		t.Fatalf("queue = %v", got)
	}
	h.sweep()
	if got := h.listQueue("eve"); len(got) != 1 {
		t.Fatalf("after a pass: %v", got)
	}
}

func TestASuspendedListOwnerQueuesTheListUntilTheyReturn(t *testing.T) {
	h := newHarness(t)
	h.setRole("eve", "admin")
	list := h.list("ada", "bob")
	h.suspend("ada")
	h.sweep()
	if got := h.listQueue("eve"); got[list] != "no_active_owner" {
		t.Fatalf("queue = %v", got)
	}
	if m := h.members("bob", list); m["ada"] == "" {
		t.Fatal("a suspended owner was removed")
	}
	h.org.set("active", acme+"/ada")
	h.sweep()
	if got := h.listQueue("eve"); len(got) != 0 {
		t.Fatalf("queue = %v", got)
	}
}

func TestASuccessorWhoLeavesIsSkipped(t *testing.T) {
	h := newHarness(t)
	item := h.item("bob")
	h.want("ada", "POST", "/v1/offers/"+h.send("bob", item, "ada", "transfer")+"/accept", nil, http.StatusOK)
	h.org.set("inactive", acme+"/ada")
	h.managedBy("ada", "carol")
	h.sweep()
	h.wantInbox("bob", 1)
	h.org.set("inactive", acme+"/bob")
	h.sweep()
	h.wantInbox("carol", 1)
}

func TestTheSweepAndAnAcceptCannotLoseTheItem(t *testing.T) {
	h := newHarness(t)
	for range 10 {
		h.org.set("active", acme+"/ada")
		item := h.item("ada")
		offer := h.send("ada", item, "dan", "transfer")
		h.org.set("inactive", acme+"/ada")
		var wg sync.WaitGroup
		wg.Go(h.sweep)
		wg.Go(func() { h.as("dan", "POST", "/v1/offers/"+offer+"/accept", nil) })
		wg.Wait()
		h.sweep()
		if got := h.owners(item); !slices.Equal(got, []string{"dan"}) {
			t.Fatalf("owners = %v", got)
		}
	}
}
