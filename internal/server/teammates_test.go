package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/xiant10/t10-modkit/people"

	"t10-collab/internal/events"
	"t10-collab/internal/policy"
	"t10-collab/verifier"
)

// teammates is a Verifier that adds agent teammates' tokens, "teammate:<account>",
// to the agent tokens in mcp_test.go. Their runs hold every work action.
type teammates struct{ agents }

func (v teammates) Verify(ctx context.Context, raw string) (*verifier.Identity, error) {
	if account, ok := strings.CutPrefix(raw, "teammate:"); ok {
		who := grantOf(policy.ActionRead, policy.ActionWrite, policy.ActionSend)
		who.UserID = account
		return who, nil
	}
	return v.agents.Verify(ctx, raw)
}

// teammateServer adds two agent accounts to acme: acc_lead, managed by carol,
// and acc_close, managed by dan. eve is a tenant admin.
func (h *harness) teammateServer() *httptest.Server {
	h.t.Helper()
	h.org.mu.Lock()
	h.org.members[acme+"/acc_lead"] = people.Member{UserID: "acc_lead", Status: "active", UserStatus: "active", Kind: people.KindAgent, ManagerUserID: "carol"}
	h.org.members[acme+"/acc_close"] = people.Member{UserID: "acc_close", Status: "active", UserStatus: "active", Kind: people.KindAgent, ManagerUserID: "dan"}
	h.org.members[acme+"/eve"] = people.Member{UserID: "eve", Status: "active", UserStatus: "active", Role: "admin"}
	h.org.reports[acme+"/carol"] = append(h.org.reports[acme+"/carol"], "acc_lead")
	h.org.reports[acme+"/dan"] = []string{"acc_close"}
	h.org.mu.Unlock()
	s := h.server
	s.Verifier = teammates{}
	ts := httptest.NewServer(s.Handler())
	h.t.Cleanup(ts.Close)
	return ts
}

type outboxRow struct {
	kind, forUser, actor, cause string
	hops                        int
}

func (h *harness) outbox() []outboxRow {
	h.t.Helper()
	rows, err := h.pool.Query(context.Background(),
		"SELECT kind, for_user_id, actor_user_id, COALESCE(cause_grant_id, ''), hops FROM collab.outbox ORDER BY seq")
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.kind, &r.forUser, &r.actor, &r.cause, &r.hops); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (h *harness) clearOutbox() {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), "DELETE FROM collab.outbox"); err != nil {
		h.t.Fatal(err)
	}
}

func toolNames(t *testing.T, url, token string) []string {
	t.Helper()
	_, out := post(t, url, token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	names := []string{}
	for _, tool := range out["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

func TestOnlyATeammateSendsThroughTools(t *testing.T) {
	h := newHarness(t)
	ts := h.teammateServer()
	// A harness's catalog, listed with no token, has every tool; a person's
	// assistant's list doesn't.
	for token, want := range map[string]bool{"": true, "agent-write": false, "teammate:acc_lead": true} {
		if got := slices.Contains(toolNames(t, ts.URL, token), "work_send"); got != want {
			t.Errorf("token %q lists work_send: %v", token, got)
		}
	}
	item := h.item("ada")
	// An assistant acting for ada still only drafts.
	if r := call(t, ts.URL, "agent-write", "work_send", map[string]any{"id": item, "to": "bob", "note": "yours"}); !r.failed || !strings.Contains(r.text, "drafts") {
		t.Errorf("an assistant sent: %+v", r)
	}
}

func TestWhoMayOfferWorkToATeammate(t *testing.T) {
	h := newHarness(t)
	ts := h.teammateServer()
	item := h.item("ada")
	// Nobody may offer it work until an admin says who.
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "acc_lead"}, http.StatusForbidden, "not_accepting")
	// Its manager's send is an assignment, which needs no intake.
	boss := h.item("carol")
	out := h.want("carol", "POST", "/v1/work-items/"+boss+"/send", map[string]any{"to": "acc_lead"}, http.StatusCreated)
	if out["state"] != "assigned" {
		t.Errorf("manager's send: %v", out)
	}
	// Only a module admin sets the intake, and only for an agent account.
	h.wantError("ada", "PUT", "/v1/admin/agents/acc_lead/intake", map[string]any{"everyone": true}, http.StatusForbidden, "forbidden")
	h.wantError("eve", "PUT", "/v1/admin/agents/bob/intake", map[string]any{"everyone": true}, http.StatusNotFound, "not_found")
	h.wantError("eve", "PUT", "/v1/admin/agents/acc_lead/intake", map[string]any{"lists": []string{"lst_nope"}}, http.StatusBadRequest, "invalid_request")

	// A unit takes work from members of the units beneath it too.
	h.org.mu.Lock()
	m := h.org.members[acme+"/ada"]
	m.OrgUnitID = "org_1"
	h.org.members[acme+"/ada"] = m
	h.org.mu.Unlock()
	h.want("eve", "PUT", "/v1/admin/agents/acc_lead/intake", map[string]any{"units": []string{"org_1"}}, http.StatusOK)
	if got := h.want("eve", "GET", "/v1/admin/agents/acc_lead/intake", nil, http.StatusOK); !slices.Equal(strs(got["units"]), []string{"org_1"}) {
		t.Errorf("intake: %v", got)
	}
	h.clearOutbox()
	offer := h.want("ada", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "acc_lead", "note": "a lead"}, http.StatusCreated)
	if offer["state"] != "pending" {
		t.Fatalf("offer: %v", offer)
	}
	h.wantError("bob", "POST", "/v1/work-items/"+h.item("bob")+"/send", map[string]any{"to": "acc_lead"}, http.StatusForbidden, "not_accepting")

	// The offer becomes an event for the teammate, and nobody else.
	if got := h.outbox(); len(got) != 1 || got[0] != (outboxRow{kind: "work.offered", forUser: "acc_lead", actor: "ada"}) {
		t.Fatalf("outbox: %+v", got)
	}

	// The teammate accepts with its own tool, and its run's grant is the cause.
	h.clearOutbox()
	if r := call(t, ts.URL, "teammate:acc_lead", "work_accept", map[string]any{"offer_id": offer["id"]}); r.failed {
		t.Fatalf("accept: %+v", r)
	}
	h.wantOwners("ada", item, []string{"acc_lead"}, []string{"ada"})
	if got := h.outbox(); len(got) != 0 {
		t.Errorf("a person's notice became an event: %+v", got)
	}

	// A comment reaches it as an event; its own doesn't.
	h.want("ada", "POST", "/v1/work-items/"+item+"/comments", map[string]any{"body": "Which region?"}, http.StatusCreated)
	if r := call(t, ts.URL, "teammate:acc_lead", "work_comment", map[string]any{"id": item, "body": "EMEA, I think"}); r.failed {
		t.Fatalf("teammate comment: %+v", r)
	}
	if got := h.outbox(); len(got) != 1 || got[0].kind != "work.commented" || got[0].forUser != "acc_lead" || got[0].actor != "ada" {
		t.Errorf("outbox after comments: %+v", got)
	}
}

func TestMentionsTellOnlyThoseWhoCanRead(t *testing.T) {
	h := newHarness(t)
	h.teammateServer()
	item := h.item("ada")
	h.want("ada", "PUT", "/v1/work-items/"+item+"/collaborators/bob", nil, http.StatusNoContent)
	h.want("ada", "POST", "/v1/work-items/"+item+"/comments", map[string]any{"body": "@bob @dan", "mentions": []string{"bob", "dan"}}, http.StatusCreated)
	var dan int
	if err := h.pool.QueryRow(context.Background(), "SELECT count(*) FROM collab.notifications WHERE user_id = 'dan'").Scan(&dan); err != nil {
		t.Fatal(err)
	}
	if dan != 0 {
		t.Error("a mention reached someone who can't read the item")
	}
	if code, _ := h.as("dan", "GET", "/v1/work-items/"+item, nil); code != http.StatusNotFound {
		t.Errorf("a mention gave dan access: %d", code)
	}
}

func TestAMentionByIDReadsAsTheirName(t *testing.T) {
	h := newHarness(t)
	ts := h.teammateServer()
	h.org.mu.Lock()
	h.org.members[acme+"/usr_mei"] = people.Member{UserID: "usr_mei", DisplayName: "Mei Lin", Status: "active", UserStatus: "active"}
	h.org.members[acme+"/usr_gone"] = people.Member{UserID: "usr_gone", DisplayName: "Gone", Status: "inactive", UserStatus: "active"}
	h.org.mu.Unlock()
	h.want("eve", "PUT", "/v1/admin/agents/acc_lead/intake", map[string]any{"everyone": true}, http.StatusOK)
	item := h.item("ada")
	h.want("ada", "PUT", "/v1/work-items/"+item+"/collaborators/usr_mei", nil, http.StatusNoContent)
	offer := h.want("ada", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "acc_lead"}, http.StatusCreated)
	call(t, ts.URL, "teammate:acc_lead", "work_accept", map[string]any{"offer_id": offer["id"]})

	// The teammate knows Mei only by her ID, and leaves her out of mentions.
	r := call(t, ts.URL, "teammate:acc_lead", "work_comment", map[string]any{"id": item, "body": "@usr_mei which tier? cc @usr_gone @usr_nobody"})
	if r.failed {
		t.Fatalf("comment: %+v", r)
	}
	_, page := h.as("ada", "GET", "/v1/work-items/"+item+"/comments", nil)
	comments := page["comments"].([]any)
	if body := comments[len(comments)-1].(map[string]any)["body"]; body != "@Mei Lin which tier? cc @usr_gone @usr_nobody" {
		t.Errorf("body = %q", body)
	}
	var mentioned string
	if err := h.pool.QueryRow(context.Background(), "SELECT coalesce(detail->>'mentions', '') FROM collab.events WHERE item_id = $1 AND kind = 'comment.added' ORDER BY at DESC LIMIT 1", item).Scan(&mentioned); err != nil {
		t.Fatal(err)
	}
	if mentioned != `["usr_mei"]` {
		t.Errorf("mentions = %s", mentioned)
	}
}

func TestTeammatesPassingAnItemStopAtTheirManager(t *testing.T) {
	h := newHarness(t)
	ts := h.teammateServer()
	for _, account := range []string{"acc_lead", "acc_close"} {
		h.want("eve", "PUT", "/v1/admin/agents/"+account+"/intake", map[string]any{"everyone": true}, http.StatusOK)
	}
	item := h.item("ada")
	offer := h.want("ada", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "acc_lead"}, http.StatusCreated)
	call(t, ts.URL, "teammate:acc_lead", "work_accept", map[string]any{"offer_id": offer["id"]})
	pass := func(from, to string) map[string]any {
		t.Helper()
		r := call(t, ts.URL, "teammate:"+from, "work_send", map[string]any{"id": item, "to": to, "note": "yours"})
		if r.failed {
			t.Fatalf("%s to %s: %+v", from, to, r)
		}
		return r.data
	}
	h.clearOutbox()
	next := pass("acc_lead", "acc_close")
	call(t, ts.URL, "teammate:acc_close", "work_accept", map[string]any{"offer_id": next["id"]})
	next = pass("acc_close", "acc_lead")
	call(t, ts.URL, "teammate:acc_lead", "work_accept", map[string]any{"offer_id": next["id"]})
	offered := slices.DeleteFunc(h.outbox(), func(r outboxRow) bool { return r.kind != "work.offered" })
	if len(offered) != 2 || offered[0].hops != 1 || offered[1].hops != 2 || offered[0].cause != "grt_1" {
		t.Fatalf("hops: %+v", offered)
	}
	// The third pass goes to acc_lead's manager instead.
	third := pass("acc_lead", "acc_close")
	if third["to_user_id"] != "carol" || third["state"] != "pending" {
		t.Fatalf("third pass: %v", third)
	}
	events := h.want("carol", "GET", "/v1/work-items/"+item+"/events", nil, http.StatusOK)["events"].([]any)
	last := events[len(events)-1].(map[string]any)["detail"].(map[string]any)
	if last["redirected_from"] != "acc_close" || last["reason"] != "agent_loop" {
		t.Errorf("redirect: %v", last)
	}
	// A person acting resets the count: carol hands it back.
	h.want("carol", "POST", "/v1/offers/"+third["id"].(string)+"/decline", map[string]any{"note": "keep going"}, http.StatusOK)
	if r := call(t, ts.URL, "teammate:acc_lead", "work_send", map[string]any{"id": item, "to": "acc_close", "note": "after carol"}); r.failed {
		t.Errorf("after a person acted: %+v", r)
	}
}

func TestTeammatesAnsweringEachOthersCommentsCountHops(t *testing.T) {
	h := newHarness(t)
	ts := h.teammateServer()
	item := h.item("ada")
	h.want("ada", "PUT", "/v1/work-items/"+item+"/collaborators/acc_lead", nil, http.StatusNoContent)
	h.want("ada", "PUT", "/v1/work-items/"+item+"/collaborators/acc_close", nil, http.StatusNoContent)
	h.want("ada", "POST", "/v1/work-items/"+item+"/comments", map[string]any{"body": "Thoughts?"}, http.StatusCreated)
	h.clearOutbox()
	for i, from := range []string{"acc_lead", "acc_close", "acc_lead"} {
		if r := call(t, ts.URL, "teammate:"+from, "work_comment", map[string]any{"id": item, "body": "Over to you"}); r.failed {
			t.Fatalf("comment %d: %+v", i, r)
		}
	}
	var hops []int
	for _, r := range h.outbox() {
		if r.kind == "work.commented" && r.actor != "ada" {
			hops = append(hops, r.hops)
		}
	}
	if !slices.Equal(hops, []int{1, 2, 3}) {
		t.Fatalf("comment hops: %v", hops)
	}
	// A person's comment resets it.
	h.want("ada", "POST", "/v1/work-items/"+item+"/comments", map[string]any{"body": "Thanks"}, http.StatusCreated)
	h.clearOutbox()
	call(t, ts.URL, "teammate:acc_close", "work_comment", map[string]any{"id": item, "body": "Sure"})
	if got := h.outbox(); len(got) != 1 || got[0].hops != 1 {
		t.Errorf("after a person commented: %+v", got)
	}
}

func TestATeammateClaimsFromItsList(t *testing.T) {
	h := newHarness(t)
	ts := h.teammateServer()
	list := h.want("ada", "POST", "/v1/lists", map[string]any{"name": "Leads"}, http.StatusCreated)["id"].(string)
	h.want("ada", "PUT", "/v1/lists/"+list+"/members/acc_lead", map[string]any{"role": "member"}, http.StatusNoContent)
	h.clearOutbox()
	item := h.want("ada", "POST", "/v1/work-items", map[string]any{"title": "New lead", "list_id": list, "unowned": true}, http.StatusCreated)["id"].(string)
	if got := h.outbox(); len(got) != 1 || got[0].kind != "list.item_added" || got[0].forUser != "acc_lead" {
		t.Fatalf("outbox: %+v", got)
	}
	if r := call(t, ts.URL, "teammate:acc_lead", "work_claim", map[string]any{"id": item}); r.failed {
		t.Fatalf("claim: %+v", r)
	}
	h.wantOwners("ada", item, []string{"acc_lead"}, []string{})
}

func TestOutboxPostsAtLeastOnce(t *testing.T) {
	h := newHarness(t)
	h.teammateServer()
	h.want("eve", "PUT", "/v1/admin/agents/acc_lead/intake", map[string]any{"everyone": true}, http.StatusOK)
	h.clearOutbox()
	h.send("ada", h.item("ada"), "acc_lead", "transfer")

	var mu sync.Mutex
	var got []events.Event
	fail := true
	automations := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/v1/events" || r.Header.Get("Authorization") != "Bearer svc" {
			t.Errorf("post: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var body struct{ Events []events.Event }
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body.Events...)
	}))
	defer automations.Close()
	poster := events.New(h.pool, automations.URL, func(context.Context) (string, error) { return "svc", nil })
	ctx := context.Background()
	if err := poster.Once(ctx); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if err := h.pool.QueryRow(ctx, "SELECT attempts FROM collab.outbox").Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("after a failure: attempts %d, %v", attempts, err)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if _, err := h.pool.Exec(ctx, "UPDATE collab.outbox SET next_at = now()"); err != nil {
		t.Fatal(err)
	}
	if err := poster.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if err := poster.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "work.offered" || got[0].For != "acc_lead" || got[0].Tenant != acme ||
		got[0].Source != "t10-collab" || !strings.HasPrefix(got[0].Ref, "collab:work_item:wi_") {
		t.Fatalf("posted: %+v", got)
	}
}
