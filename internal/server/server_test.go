package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"t10-collab/internal/activity"
	"t10-collab/internal/db"
	"t10-collab/internal/lists"
	"t10-collab/internal/offers"
	"t10-collab/internal/people"
	"t10-collab/internal/policy"
	"t10-collab/internal/workitems"
	"t10-collab/verifier"
)

const acme, globex = "ten_acme", "ten_globex"

// tokens is a Verifier whose token is the literal "tenant/user".
type tokens struct{}

func (tokens) Verify(_ context.Context, raw string) (*verifier.Identity, error) {
	switch raw {
	case "service":
		return &verifier.Identity{ServiceID: "other"}, nil
	case "notify":
		return &verifier.Identity{ServiceID: notifyService}, nil
	case "platform-admin":
		return &verifier.Identity{UserID: "usr_root"}, nil
	case "agent":
		return &verifier.Identity{UserID: "ada", TenantID: acme, Agent: &verifier.Agent{GrantID: "grt_1"}}, nil
	}
	tenant, user, ok := strings.Cut(raw, "/")
	if !ok {
		return nil, verifier.ErrInvalidToken
	}
	return &verifier.Identity{TenantID: tenant, UserID: user}, nil
}

// org is a fake auth service.
type org struct {
	mu       sync.Mutex
	down     bool
	members  map[string]people.Member
	reports  map[string][]string
	direct   map[string][]string
	units    map[string][]string
	calls    int
	searched string
}

func (o *org) CachedReports(_ context.Context, tenantID, userID string, transitive bool) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	if o.down {
		return nil, people.ErrUnavailable
	}
	if transitive {
		return o.reports[tenantID+"/"+userID], nil
	}
	return o.direct[tenantID+"/"+userID], nil
}

func (o *org) UnitMembers(_ context.Context, tenantID, unitID string) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.down {
		return nil, people.ErrUnavailable
	}
	members, ok := o.units[tenantID+"/"+unitID]
	if !ok {
		return nil, people.ErrNotFound
	}
	return members, nil
}

func (o *org) Member(_ context.Context, tenantID, userID string) (people.Member, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.down {
		return people.Member{}, people.ErrUnavailable
	}
	m, ok := o.members[tenantID+"/"+userID]
	if !ok {
		return people.Member{}, people.ErrNotFound
	}
	return m, nil
}

func (o *org) Reports(_ context.Context, tenantID, userID string) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.down {
		return nil, people.ErrUnavailable
	}
	return o.reports[tenantID+"/"+userID], nil
}

func (o *org) Members(_ context.Context, tenantID string) ([]people.Member, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.down {
		return nil, people.ErrUnavailable
	}
	found := []people.Member{}
	for key, m := range o.members {
		if strings.HasPrefix(key, tenantID+"/") {
			found = append(found, m)
		}
	}
	return found, nil
}

func (o *org) Search(_ context.Context, tenantID, _, _, _ string, _ int) ([]people.Member, string, error) {
	o.searched = tenantID
	return []people.Member{{UserID: "ada", DisplayName: "Ada", Role: "admin"}}, "", nil
}

func (o *org) Units(context.Context, string) ([]people.Unit, error) {
	return []people.Unit{{ID: "org_1", Name: "Sales", Kind: "team"}}, nil
}

func (o *org) set(status string, users ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, user := range users {
		_, id, _ := strings.Cut(user, "/")
		o.members[user] = people.Member{UserID: id, Status: status, UserStatus: "active"}
	}
}

type harness struct {
	t    *testing.T
	pool *pgxpool.Pool
	org  *org
	url  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS collab CASCADE"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	o := &org{
		members: map[string]people.Member{}, units: map[string][]string{},
		reports: map[string][]string{acme + "/carol": {"bob", "ada"}}, direct: map[string][]string{},
	}
	o.set("active", acme+"/ada", acme+"/bob", acme+"/carol", acme+"/dan", acme+"/eve", globex+"/gus")
	items := workitems.NewService(pool, o)
	s := New(Deps{
		Pool: pool, Verifier: tokens{}, Org: o, Items: items,
		Offers: offers.NewService(pool, o, items), Lists: lists.NewService(pool, o), Activity: activity.NewService(pool),
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &harness{t: t, pool: pool, org: o, url: ts.URL}
}

func (h *harness) request(token, method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.url+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// as calls the API as a user of acme.
func (h *harness) as(user, method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	return h.request(acme+"/"+user, method, path, body)
}

func (h *harness) want(user, method, path string, body any, status int) map[string]any {
	h.t.Helper()
	got, out := h.as(user, method, path, body)
	if got != status {
		h.t.Fatalf("%s %s %s = %d, want %d: %v", user, method, path, got, status, out)
	}
	return out
}

func (h *harness) wantError(user, method, path string, body any, status int, code string) {
	h.t.Helper()
	if out := h.want(user, method, path, body, status); out["error"] != code {
		h.t.Fatalf("%s %s %s: error %v, want %s", user, method, path, out["error"], code)
	}
}

func (h *harness) item(user string) string {
	h.t.Helper()
	return h.want(user, "POST", "/v1/work-items", map[string]any{"title": "Task"}, http.StatusCreated)["id"].(string)
}

func (h *harness) send(user, item, to, mode string) string {
	h.t.Helper()
	return h.want(user, "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": to, "mode": mode}, http.StatusCreated)["id"].(string)
}

func strs(v any) []string {
	out := []string{}
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

func (h *harness) wantOwners(reader, item string, owners []string, collaborators []string) {
	h.t.Helper()
	out := h.want(reader, "GET", "/v1/work-items/"+item, nil, http.StatusOK)
	if got := strs(out["owners"]); !slices.Equal(got, owners) {
		h.t.Errorf("owners = %v, want %v", got, owners)
	}
	if got := strs(out["collaborators"]); !slices.Equal(got, collaborators) {
		h.t.Errorf("collaborators = %v, want %v", got, collaborators)
	}
}

func TestEveryRouteDeclaresAnAction(t *testing.T) {
	s := New(Deps{})
	for _, rt := range s.routes() {
		if !slices.Contains([]string{policy.ActionRead, policy.ActionWrite, policy.ActionDelete, policy.ActionSend}, rt.action) {
			t.Errorf("%s %s declares %q", rt.method, rt.pattern, rt.action)
		}
		if rt.handler == nil {
			t.Errorf("%s %s has no handler", rt.method, rt.pattern)
		}
	}
}

func TestOnlyAPersonInATenantGetsIn(t *testing.T) {
	h := newHarness(t)
	for token, want := range map[string]int{
		"": http.StatusUnauthorized, "garbage": http.StatusUnauthorized,
		"service": http.StatusForbidden, "platform-admin": http.StatusForbidden, "agent": http.StatusForbidden,
		acme + "/ada": http.StatusOK,
	} {
		if got, _ := h.request(token, "GET", "/v1/work-items", nil); got != want {
			t.Errorf("token %q: %d, want %d", token, got, want)
		}
	}
}

func TestListFiltersBeforePagination(t *testing.T) {
	h := newHarness(t)
	mine := []string{}
	for i := range 30 {
		if i%3 == 0 {
			mine = append(mine, h.item("ada"))
		} else {
			h.item("bob")
		}
	}
	seen := []string{}
	cursor := ""
	for {
		out := h.want("ada", "GET", "/v1/work-items?limit=4&view=owned&cursor="+cursor, nil, http.StatusOK)
		page := out["items"].([]any)
		cursor = out["next_cursor"].(string)
		if cursor != "" && len(page) != 4 {
			t.Fatalf("a filtered page held %d items, so filtering ran after pagination", len(page))
		}
		for _, i := range page {
			seen = append(seen, i.(map[string]any)["id"].(string))
		}
		if cursor == "" {
			break
		}
	}
	slices.Sort(seen)
	slices.Sort(mine)
	if !slices.Equal(seen, mine) {
		t.Errorf("saw %d items, want %d", len(seen), len(mine))
	}
	h.wantError("ada", "GET", "/v1/work-items?cursor=nonsense!", nil, http.StatusBadRequest, "invalid_request")
	h.wantError("ada", "GET", "/v1/work-items?view=everything", nil, http.StatusBadRequest, "invalid_request")
}

func TestTenantsCannotCross(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	gus := globex + "/gus"
	if got, _ := h.request(gus, "GET", "/v1/work-items/"+item, nil); got != http.StatusNotFound {
		t.Errorf("read across tenants: %d", got)
	}
	if got, out := h.request(gus, "GET", "/v1/work-items", nil); got != http.StatusOK || len(out["items"].([]any)) != 0 {
		t.Errorf("list across tenants: %v", out)
	}
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/send", map[string]string{"to": "gus"}, http.StatusUnprocessableEntity, "invalid_recipient")

	// The database refuses it too, whatever the code above does.
	_, err := h.pool.Exec(context.Background(),
		"INSERT INTO collab.work_item_owners (tenant_id, item_id, user_id, added_by) VALUES ($1, $2, 'gus', 'gus')", globex, item)
	if err == nil {
		t.Error("the database linked an owner across tenants")
	}
}

func TestMissingAndForbiddenLookTheSame(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.wantError("bob", "GET", "/v1/work-items/"+item, nil, http.StatusNotFound, "not_found")
	h.wantError("bob", "GET", "/v1/work-items/wi_nope", nil, http.StatusNotFound, "not_found")
	h.wantError("bob", "GET", "/v1/work-items/"+item+"/events", nil, http.StatusNotFound, "not_found")
	h.wantError("bob", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 1, "title": "x"}, http.StatusNotFound, "not_found")
	h.wantError("bob", "POST", "/v1/work-items/"+item+"/comments", map[string]any{"body": "x"}, http.StatusNotFound, "not_found")
}

func TestOfferTransfer(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	offer := h.want("ada", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "bob", "note": "yours?"}, http.StatusCreated)
	if offer["state"] != "pending" || offer["mode"] != "transfer" {
		t.Fatalf("offer = %v", offer)
	}
	h.wantOwners("ada", item, []string{"ada"}, []string{})
	inbox := h.want("bob", "GET", "/v1/offers?box=inbox", nil, http.StatusOK)["offers"].([]any)
	if len(inbox) != 1 || inbox[0].(map[string]any)["item_title"] != "Task" {
		t.Fatalf("inbox = %v", inbox)
	}
	h.want("bob", "GET", "/v1/work-items/"+item, nil, http.StatusOK)

	h.want("bob", "POST", "/v1/offers/"+offer["id"].(string)+"/accept", nil, http.StatusOK)
	h.wantOwners("ada", item, []string{"bob"}, []string{"ada"})
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/send", map[string]string{"to": "dan"}, http.StatusForbidden, "forbidden")
	h.wantError("bob", "POST", "/v1/offers/"+offer["id"].(string)+"/accept", nil, http.StatusConflict, "offer_not_pending")
}

func TestDeclineEndsAccessAndTellsTheSender(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	offer := h.send("ada", item, "bob", "")
	h.want("bob", "POST", "/v1/offers/"+offer+"/decline", map[string]string{"note": "too busy"}, http.StatusOK)
	h.wantOwners("ada", item, []string{"ada"}, []string{})
	h.wantError("bob", "GET", "/v1/work-items/"+item, nil, http.StatusNotFound, "not_found")
	if inbox := h.want("bob", "GET", "/v1/offers?box=inbox", nil, http.StatusOK)["offers"].([]any); len(inbox) != 0 {
		t.Errorf("inbox = %v", inbox)
	}
	sent := h.want("ada", "GET", "/v1/offers?box=sent", nil, http.StatusOK)["offers"].([]any)[0].(map[string]any)
	if sent["state"] != "declined" || sent["response_note"] != "too busy" {
		t.Errorf("sent = %v", sent)
	}
	notes := h.want("ada", "GET", "/v1/notifications", nil, http.StatusOK)["notifications"].([]any)
	if len(notes) != 1 || notes[0].(map[string]any)["event"].(map[string]any)["kind"] != "offer.declined" {
		t.Errorf("notifications = %v", notes)
	}
}

func TestOnlyTheRecipientAnswersAndOnlyAnOwnerWithdraws(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	offer := h.send("ada", item, "bob", "")
	h.wantError("dan", "POST", "/v1/offers/"+offer+"/accept", nil, http.StatusNotFound, "not_found")
	h.wantError("ada", "POST", "/v1/offers/"+offer+"/accept", nil, http.StatusNotFound, "not_found")
	h.wantError("dan", "POST", "/v1/offers/"+offer+"/withdraw", nil, http.StatusNotFound, "not_found")
	h.wantError("bob", "POST", "/v1/offers/"+offer+"/withdraw", nil, http.StatusForbidden, "forbidden")
	h.want("ada", "POST", "/v1/offers/"+offer+"/withdraw", nil, http.StatusOK)
	h.wantError("bob", "POST", "/v1/offers/"+offer+"/accept", nil, http.StatusConflict, "offer_not_pending")
	h.wantError("bob", "GET", "/v1/work-items/"+item, nil, http.StatusNotFound, "not_found")
}

func TestShareKeepsBothOwners(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", item, "bob", "share")+"/accept", nil, http.StatusOK)
	h.wantOwners("ada", item, []string{"ada", "bob"}, []string{})
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/send", map[string]string{"to": "bob"}, http.StatusConflict, "already_owner")

	h.want("ada", "DELETE", "/v1/work-items/"+item+"/owners/me", nil, http.StatusNoContent)
	h.wantError("bob", "DELETE", "/v1/work-items/"+item+"/owners/me", nil, http.StatusConflict, "last_owner")
	h.wantOwners("bob", item, []string{"bob"}, []string{})
}

func TestPendingOfferLimits(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.send("ada", item, "bob", "share")
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "bob", "mode": "share"}, http.StatusConflict, "offer_exists")
	h.send("ada", item, "dan", "transfer")
	h.send("ada", item, "eve", "share")
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "carol", "mode": "transfer"}, http.StatusConflict, "offer_exists")
}

func TestAcceptingATransferEndsTheSendersOtherOffers(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	transfer := h.send("ada", item, "bob", "transfer")
	share := h.send("ada", item, "dan", "share")
	h.want("bob", "POST", "/v1/offers/"+transfer+"/accept", nil, http.StatusOK)
	h.wantError("dan", "POST", "/v1/offers/"+share+"/accept", nil, http.StatusConflict, "offer_not_pending")
}

func TestManagerAssignsWithNoAcceptance(t *testing.T) {
	h := newHarness(t)
	item := h.item("carol")
	stale := h.send("carol", item, "dan", "transfer")
	out := h.want("carol", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "bob", "note": "for you"}, http.StatusCreated)
	if out["state"] != "assigned" {
		t.Fatalf("send = %v", out)
	}
	h.wantOwners("bob", item, []string{"bob"}, []string{"carol"})
	// Assigning must end the offer, or it could take the item from Bob.
	h.wantError("dan", "POST", "/v1/offers/"+stale+"/accept", nil, http.StatusConflict, "offer_not_pending")
	if inbox := h.want("bob", "GET", "/v1/offers?box=inbox", nil, http.StatusOK)["offers"].([]any); len(inbox) != 0 {
		t.Errorf("an assignment reached the inbox: %v", inbox)
	}
	notes := h.want("bob", "GET", "/v1/notifications", nil, http.StatusOK)["notifications"].([]any)
	if len(notes) == 0 {
		t.Error("Bob was not told")
	}

	// A send up the org is an ordinary offer, even back to the manager.
	if got := h.send("bob", item, "carol", "transfer"); got == "" {
		t.Fatal("no offer")
	}
	sent := h.want("bob", "GET", "/v1/offers?box=sent", nil, http.StatusOK)["offers"].([]any)[0].(map[string]any)
	if sent["state"] != "pending" {
		t.Errorf("a send up the org = %v", sent)
	}
	h.wantOwners("bob", item, []string{"bob"}, []string{"carol"})
}

func TestShareAssignmentKeepsTheManagerAsOwner(t *testing.T) {
	h := newHarness(t)
	item := h.item("carol")
	h.send("carol", item, "bob", "share")
	h.wantOwners("bob", item, []string{"bob", "carol"}, []string{})
}

func TestCollaboratorPromotedOnAccept(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.want("ada", "PUT", "/v1/work-items/"+item+"/collaborators/bob", nil, http.StatusNoContent)
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", item, "bob", "share")+"/accept", nil, http.StatusOK)
	h.wantOwners("ada", item, []string{"ada", "bob"}, []string{})
}

func TestRemovedManagerCannotAssign(t *testing.T) {
	h := newHarness(t)
	item := h.item("carol")
	h.org.set("inactive", acme+"/carol")
	h.wantError("carol", "POST", "/v1/work-items/"+item+"/send", map[string]string{"to": "bob"}, http.StatusForbidden, "forbidden")
}

func TestSendFailsClosedWhenTheOrgIsDown(t *testing.T) {
	h := newHarness(t)
	item := h.item("carol")
	h.want("carol", "PUT", "/v1/work-items/"+item+"/collaborators/ada", nil, http.StatusNoContent)
	h.org.down = true
	h.wantError("carol", "POST", "/v1/work-items/"+item+"/send", map[string]string{"to": "bob"}, http.StatusServiceUnavailable, "org_unavailable")
	h.wantError("carol", "PUT", "/v1/work-items/"+item+"/collaborators/bob", nil, http.StatusServiceUnavailable, "org_unavailable")
	// Reads and edits keep working through the outage.
	h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK)
	h.want("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 1, "title": "New"}, http.StatusOK)
	h.wantOwners("carol", item, []string{"carol"}, []string{"ada"})
}

func TestSendRefusesBadRecipients(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.org.set("inactive", acme+"/dan")
	for _, to := range []string{"dan", "nobody", "ada"} {
		h.wantError("ada", "POST", "/v1/work-items/"+item+"/send", map[string]string{"to": to}, http.StatusUnprocessableEntity, "invalid_recipient")
	}
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/send", map[string]string{"to": "bob", "mode": "gift"}, http.StatusBadRequest, "invalid_request")
}

func TestCreateAndSendAtOnce(t *testing.T) {
	h := newHarness(t)
	out := h.want("ada", "POST", "/v1/work-items", map[string]any{"title": "Now", "send": map[string]string{"to": "bob"}}, http.StatusCreated)
	if len(h.want("bob", "GET", "/v1/offers?box=inbox", nil, http.StatusOK)["offers"].([]any)) != 1 {
		t.Error("no offer")
	}
	h.wantOwners("ada", out["id"].(string), []string{"ada"}, []string{})

	assigned := h.want("carol", "POST", "/v1/work-items", map[string]any{"title": "Now", "send": map[string]string{"to": "bob"}}, http.StatusCreated)
	if got := strs(assigned["owners"]); !slices.Equal(got, []string{"bob"}) {
		t.Errorf("the response shows owners %v", got)
	}

	// A send that fails must leave no item behind.
	h.wantError("ada", "POST", "/v1/work-items", map[string]any{"title": "Lost", "send": map[string]string{"to": "nobody"}}, http.StatusUnprocessableEntity, "invalid_recipient")
	if items := h.want("ada", "GET", "/v1/work-items", nil, http.StatusOK)["items"].([]any); len(items) != 1 {
		t.Errorf("items = %d", len(items))
	}
}

func TestClientRefMakesRetriesSafe(t *testing.T) {
	h := newHarness(t)
	body := map[string]any{"title": "Once", "client_ref": "r1"}
	first := h.want("ada", "POST", "/v1/work-items", body, http.StatusCreated)
	second := h.want("ada", "POST", "/v1/work-items", body, http.StatusCreated)
	if first["id"] != second["id"] {
		t.Error("a retry created a duplicate")
	}
	if other := h.want("bob", "POST", "/v1/work-items", body, http.StatusCreated); other["id"] == first["id"] {
		t.Error("client_ref is shared between creators")
	}

	item := h.item("carol")
	send := map[string]any{"to": "bob", "client_ref": "s1"}
	a := h.want("carol", "POST", "/v1/work-items/"+item+"/send", send, http.StatusCreated)
	// Carol no longer owns it, but the retry must still return the first result.
	b := h.want("carol", "POST", "/v1/work-items/"+item+"/send", send, http.StatusCreated)
	if a["id"] != b["id"] || b["state"] != "assigned" {
		t.Errorf("retry = %v", b)
	}
	h.wantError("carol", "POST", "/v1/work-items/"+h.item("carol")+"/send", send, http.StatusConflict, "client_ref_reused")
}

func TestVersionConflict(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.want("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 1, "title": "One"}, http.StatusOK)
	h.wantError("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 1, "title": "Two"}, http.StatusConflict, "version_conflict")
	h.wantError("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"title": "Two"}, http.StatusBadRequest, "invalid_request")
	out := h.want("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 2, "due_at": "2027-01-02T03:04:05Z"}, http.StatusOK)
	if out["due_at"] == nil {
		t.Error("due_at was not set")
	}
	out = h.want("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 3, "due_at": nil}, http.StatusOK)
	if out["due_at"] != nil || out["title"] != "One" {
		t.Errorf("clearing due_at: %v", out)
	}
}

func TestStatusRules(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.want("ada", "PUT", "/v1/work-items/"+item+"/collaborators/bob", nil, http.StatusNoContent)
	offer := h.send("ada", item, "dan", "share")

	h.want("bob", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 1, "status": "doing"}, http.StatusOK)
	h.wantError("bob", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 2, "status": "cancelled"}, http.StatusForbidden, "forbidden")
	h.wantError("dan", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 2, "title": "x"}, http.StatusForbidden, "forbidden")
	h.wantError("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 2, "status": "lost"}, http.StatusBadRequest, "invalid_request")

	// Closing withdraws, and a closed item can't be sent.
	h.want("bob", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 2, "status": "done"}, http.StatusOK)
	h.wantError("dan", "POST", "/v1/offers/"+offer+"/accept", nil, http.StatusConflict, "offer_not_pending")
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/send", map[string]string{"to": "eve"}, http.StatusConflict, "item_closed")

	// Anyone who can edit can reopen, except out of cancelled.
	h.want("bob", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 3, "status": "todo"}, http.StatusOK)
	h.want("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 4, "status": "cancelled"}, http.StatusOK)
	h.wantError("bob", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 5, "status": "todo"}, http.StatusForbidden, "forbidden")
}

func TestDelete(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", item, "bob", "transfer")+"/accept", nil, http.StatusOK)
	// Ada created it, but it's Bob's work now.
	h.wantError("ada", "DELETE", "/v1/work-items/"+item, nil, http.StatusForbidden, "forbidden")

	offer := h.send("bob", item, "dan", "share")
	h.want("bob", "DELETE", "/v1/work-items/"+item, nil, http.StatusNoContent)
	h.wantError("bob", "GET", "/v1/work-items/"+item, nil, http.StatusNotFound, "not_found")
	h.wantError("dan", "GET", "/v1/work-items/"+item, nil, http.StatusNotFound, "not_found")
	if status, _ := h.as("dan", "POST", "/v1/offers/"+offer+"/accept", nil); status == http.StatusOK {
		t.Error("accepted an offer on a deleted item")
	}
	// Sent history stays, without the title of an item nobody can see.
	sent := h.want("bob", "GET", "/v1/offers?box=sent", nil, http.StatusOK)["offers"].([]any)[0].(map[string]any)
	if sent["state"] != "withdrawn" || sent["item_title"] != "" {
		t.Errorf("sent = %v", sent)
	}
}

func TestCollaboratorsAndComments(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.want("ada", "PUT", "/v1/work-items/"+item+"/collaborators/bob", nil, http.StatusNoContent)
	h.want("ada", "PUT", "/v1/work-items/"+item+"/collaborators/bob", nil, http.StatusNoContent)
	h.wantError("bob", "PUT", "/v1/work-items/"+item+"/collaborators/dan", nil, http.StatusForbidden, "forbidden")
	h.wantError("ada", "PUT", "/v1/work-items/"+item+"/collaborators/ada", nil, http.StatusConflict, "already_owner")
	h.wantError("ada", "PUT", "/v1/work-items/"+item+"/collaborators/gus", nil, http.StatusUnprocessableEntity, "invalid_recipient")
	if views := h.want("bob", "GET", "/v1/work-items?view=collaborating", nil, http.StatusOK)["items"].([]any); len(views) != 1 {
		t.Errorf("collaborating = %v", views)
	}

	h.want("bob", "POST", "/v1/work-items/"+item+"/comments", map[string]string{"body": "hello"}, http.StatusCreated)
	h.wantError("bob", "POST", "/v1/work-items/"+item+"/comments", map[string]string{"body": strings.Repeat("x", 10001)}, http.StatusBadRequest, "invalid_request")
	comments := h.want("ada", "GET", "/v1/work-items/"+item+"/comments", nil, http.StatusOK)["comments"].([]any)
	if len(comments) != 1 || comments[0].(map[string]any)["body"] != "hello" {
		t.Errorf("comments = %v", comments)
	}

	h.want("ada", "DELETE", "/v1/work-items/"+item+"/collaborators/bob", nil, http.StatusNoContent)
	h.wantError("bob", "GET", "/v1/work-items/"+item, nil, http.StatusNotFound, "not_found")
}

func TestHistoryRecordsEveryChange(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.want("ada", "PATCH", "/v1/work-items/"+item, map[string]any{"version": 1, "title": "Secret plan"}, http.StatusOK)
	h.want("bob", "POST", "/v1/offers/"+h.send("ada", item, "bob", "")+"/accept", nil, http.StatusOK)
	kinds := []string{}
	cursor := ""
	for {
		out := h.want("ada", "GET", "/v1/work-items/"+item+"/events?limit=2&cursor="+cursor, nil, http.StatusOK)
		for _, e := range out["events"].([]any) {
			kinds = append(kinds, e.(map[string]any)["kind"].(string))
		}
		if cursor = out["next_cursor"].(string); cursor == "" {
			break
		}
	}
	if want := []string{"item.created", "item.updated", "offer.sent", "offer.accepted"}; !slices.Equal(kinds, want) {
		t.Errorf("history = %v", kinds)
	}
	if _, err := h.pool.Exec(context.Background(), "UPDATE collab.events SET kind = 'x'"); err == nil {
		t.Error("events can be rewritten")
	}
	if _, err := h.pool.Exec(context.Background(), "DELETE FROM collab.events"); err == nil {
		t.Error("events can be deleted")
	}
}

func TestSendsAreRateLimited(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	ctx := context.Background()
	_, err := h.pool.Exec(ctx,
		`INSERT INTO collab.offers (tenant_id, id, item_id, from_user_id, to_user_id, mode, state)
		 SELECT $1, 'ofr_' || n, $2, 'ada', 'old', 'share', 'withdrawn' FROM generate_series(1, 100) n`, acme, item)
	if err != nil {
		t.Fatal(err)
	}
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/send", map[string]string{"to": "bob"}, http.StatusTooManyRequests, "rate_limited")
}

func TestNotificationsAreNotLostToASlowCommit(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.want("ada", "PUT", "/v1/work-items/"+item+"/collaborators/bob", nil, http.StatusNoContent)
	ctx := context.Background()
	start := h.want("bob", "GET", "/v1/notifications", nil, http.StatusOK)["cursor"].(string)

	// A transaction takes its place, then stays open while a later one commits.
	slow, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Rollback(ctx)
	_, err = slow.Exec(ctx,
		`WITH e AS (INSERT INTO collab.events (tenant_id, id, item_id, actor_user_id, kind) VALUES ($1, 'evt_slow', $2, 'ada', 'slow') RETURNING id)
		 INSERT INTO collab.notifications (tenant_id, id, user_id, event_id) SELECT $1, 'ntf_slow', 'bob', id FROM e`, acme, item)
	if err != nil {
		t.Fatal(err)
	}
	h.want("ada", "POST", "/v1/work-items/"+item+"/comments", map[string]string{"body": "fast"}, http.StatusCreated)

	first := h.want("bob", "GET", "/v1/notifications?after="+start, nil, http.StatusOK)
	if len(first["notifications"].([]any)) != 0 {
		t.Fatalf("read past a running transaction: %v", first)
	}
	cursor := first["cursor"].(string)
	if err := slow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	second := h.want("bob", "GET", "/v1/notifications?after="+cursor, nil, http.StatusOK)
	kinds := []string{}
	for _, n := range second["notifications"].([]any) {
		kinds = append(kinds, n.(map[string]any)["event"].(map[string]any)["kind"].(string))
	}
	if !slices.Equal(kinds, []string{"slow", "comment.added"}) {
		t.Fatalf("after the commit: %v", kinds)
	}
	third := h.want("bob", "GET", "/v1/notifications?after="+second["cursor"].(string), nil, http.StatusOK)
	if len(third["notifications"].([]any)) != 0 {
		t.Errorf("repeated: %v", third)
	}

	id := second["notifications"].([]any)[0].(map[string]any)["id"].(string)
	h.want("ada", "POST", "/v1/notifications/read", map[string]any{"ids": []string{id}}, http.StatusNoContent)
	h.want("bob", "POST", "/v1/notifications/read", map[string]any{"ids": []string{id}}, http.StatusNoContent)
	var readAt bool
	if err := h.pool.QueryRow(ctx, "SELECT read_at IS NOT NULL FROM collab.notifications WHERE id = $1", id).Scan(&readAt); err != nil || !readAt {
		t.Errorf("read_at set = %v, %v", readAt, err)
	}
	h.wantError("bob", "GET", "/v1/notifications?after=nonsense", nil, http.StatusBadRequest, "invalid_request")
}

func TestAcceptAndWithdrawCannotBothWin(t *testing.T) {
	h := newHarness(t)
	for range 15 {
		item := h.item("ada")
		offer := h.send("ada", item, "bob", "transfer")
		var wg sync.WaitGroup
		var accepted, withdrawn int
		wg.Go(func() { accepted, _ = h.as("bob", "POST", "/v1/offers/"+offer+"/accept", nil) })
		wg.Go(func() { withdrawn, _ = h.as("ada", "POST", "/v1/offers/"+offer+"/withdraw", nil) })
		wg.Wait()
		if (accepted == http.StatusOK) == (withdrawn == http.StatusOK) {
			t.Fatalf("accept %d, withdraw %d", accepted, withdrawn)
		}
		out := h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK)
		want := []string{"ada"}
		if accepted == http.StatusOK {
			want = []string{"bob"}
		}
		if got := strs(out["owners"]); !slices.Equal(got, want) {
			t.Fatalf("owners = %v, want %v", got, want)
		}
	}
}

func TestTwoOwnersLeavingAtOnceLeaveOne(t *testing.T) {
	h := newHarness(t)
	for range 15 {
		item := h.item("ada")
		h.want("bob", "POST", "/v1/offers/"+h.send("ada", item, "bob", "share")+"/accept", nil, http.StatusOK)
		var wg sync.WaitGroup
		statuses := make([]int, 2)
		for i, user := range []string{"ada", "bob"} {
			wg.Go(func() { statuses[i], _ = h.as(user, "DELETE", "/v1/work-items/"+item+"/owners/me", nil) })
		}
		wg.Wait()
		slices.Sort(statuses)
		if statuses[0] != http.StatusNoContent || statuses[1] != http.StatusConflict {
			t.Fatalf("statuses = %v", statuses)
		}
	}
}

func TestPeopleShowsNoRoleAndStaysInTheTokensTenant(t *testing.T) {
	h := newHarness(t)
	out := h.want("ada", "GET", "/v1/people?q=ad&tenant="+globex, nil, http.StatusOK)
	person := out["people"].([]any)[0].(map[string]any)
	if person["display_name"] != "Ada" || person["role"] != nil {
		t.Errorf("person = %v", person)
	}
	if h.org.searched != acme {
		t.Errorf("searched %s", h.org.searched)
	}
	h.want("ada", "GET", "/v1/units", nil, http.StatusOK)
}

func TestNoErrorLeaksDetail(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	failure(rec, r, errors.New("password=hunter2"))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	failure(rec, r, fmt.Errorf("wrapped: %w", people.ErrUnavailable))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("%d", rec.Code)
	}
}

func TestManifest(t *testing.T) {
	h := newHarness(t)
	got := h.want("ada", "GET", "/v1/module", nil, http.StatusOK)
	if got["id"] != "collab" || got["api_version"] != float64(1) || got["enabled"] != true {
		t.Errorf("manifest = %v", got)
	}
}

func TestDeletingAnItemNeedsItsOwnAction(t *testing.T) {
	for _, rt := range New(Deps{}).routes() {
		if rt.method == "DELETE" && rt.pattern == "/v1/work-items/{id}" && rt.action != policy.ActionDelete {
			t.Errorf("action = %q", rt.action)
		}
	}
}

func TestNotificationsNameTheirRecord(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.send("ada", item, "bob", "transfer")
	got := h.want("bob", "GET", "/v1/notifications", nil, http.StatusOK)["notifications"].([]any)
	if len(got) == 0 || got[0].(map[string]any)["target"] != "collab:work_item:"+item {
		t.Errorf("notifications = %v", got)
	}
}

func TestAttention(t *testing.T) {
	h := newHarness(t)
	path := "/internal/v1/attention?tenant=" + acme + "&user=bob"
	if status, _ := h.request("service", "GET", path, nil); status != http.StatusForbidden {
		t.Errorf("another service: %d", status)
	}
	if status, _ := h.request(acme+"/bob", "GET", path, nil); status != http.StatusForbidden {
		t.Errorf("a person: %d", status)
	}
	if status, _ := h.request("notify", "GET", "/internal/v1/attention?tenant=a&user=b%3Fx", nil); status != http.StatusBadRequest {
		t.Errorf("bad ID: %d", status)
	}
	h.send("ada", h.item("ada"), "bob", "transfer")
	declined := h.send("ada", h.item("ada"), "carol", "transfer")
	h.want("carol", "POST", "/v1/offers/"+declined+"/decline", map[string]any{}, http.StatusOK)
	h.send("carol", h.item("carol"), "bob", "transfer")

	status, got := h.request("notify", "GET", path, nil)
	if status != http.StatusOK || got["offers_to_answer"] != float64(1) || got["new_assignments"] != float64(1) {
		t.Errorf("bob: %d %v", status, got)
	}
	_, got = h.request("notify", "GET", "/internal/v1/attention?tenant="+acme+"&user=ada", nil)
	if got["declined"] != float64(1) {
		t.Errorf("ada: %v", got)
	}
	_, got = h.request("notify", "GET", "/internal/v1/attention?tenant="+globex+"&user=bob", nil)
	if got["offers_to_answer"] != float64(0) {
		t.Errorf("another tenant: %v", got)
	}
}
