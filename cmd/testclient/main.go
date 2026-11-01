// Command testclient drives the API against a local stack, because no client
// exists yet. It signs in with the auth service's dev password grant as the sample
// accounts, so run `authctl samples-add` first. See the README.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
)

const (
	acme   = "ten_sample-acme"
	globex = "ten_sample-globex"
)

type client struct {
	base  string
	token string
}

func (c client) call(method, url string, body any) (int, map[string]any) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			fatal("encode: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+url, reader)
	if err != nil {
		fatal("%s %s: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fatal("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (c client) want(status int, method, url string, body any) map[string]any {
	got, out := c.call(method, url, body)
	if got != status {
		fatal("%s %s = %d, want %d: %v", method, url, got, status, out)
	}
	return out
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	os.Exit(1)
}

func step(format string, args ...any) { fmt.Printf("ok  "+format+"\n", args...) }

func owners(item map[string]any) []string {
	out := []string{}
	for _, id := range item["owners"].([]any) {
		out = append(out, id.(string))
	}
	return out
}

func main() {
	authURL := flag.String("auth", "http://localhost:8081", "auth service URL")
	collabURL := flag.String("collab", "http://localhost:8082", "t10-collab URL")
	password := flag.String("password", "t10-sample-password", "the sample accounts' password")
	flag.Parse()

	auth := client{base: *authURL}
	login := func(name, tenant string) (client, string) {
		out := auth.want(http.StatusOK, "POST", "/v1/token", map[string]string{
			"grant_type": "password", "username": name, "password": *password, "tenant": tenant,
		})
		token := out["access_token"].(string)
		me := client{base: *authURL, token: token}.want(http.StatusOK, "GET", "/v1/me", nil)
		return client{base: *collabURL, token: token}, me["user_id"].(string)
	}
	manager, managerID := login("sample-acme-admin", acme)
	member, memberID := login("sample-member", acme)
	peer, peerID := login("sample-multi", acme)
	outsider, _ := login("sample-globex-admin", globex)
	step("signed in with real tokens")

	placement := "/v1/tenants/" + acme + "/members/" + memberID + "/placement"
	admin := client{base: *authURL, token: manager.token}
	admin.want(http.StatusOK, "PUT", placement, map[string]string{"manager_user_id": managerID})
	defer admin.want(http.StatusOK, "PUT", placement, map[string]string{})
	step("sample-member now reports to sample-acme-admin")

	found := member.want(http.StatusOK, "GET", "/v1/people?q=sample-multi", nil)["people"].([]any)
	if len(found) != 1 || found[0].(map[string]any)["user_id"] != peerID {
		fatal("people search = %v", found)
	}
	step("found a teammate by display name")

	item := member.want(http.StatusCreated, "POST", "/v1/work-items", map[string]any{"title": "Review the Q3 plan"})
	itemURL := "/v1/work-items/" + item["id"].(string)
	offer := member.want(http.StatusCreated, "POST", itemURL+"/send", map[string]string{"to": peerID, "note": "can you take this?"})
	if offer["state"] != "pending" {
		fatal("a peer send = %v", offer)
	}
	peer.want(http.StatusOK, "POST", "/v1/offers/"+offer["id"].(string)+"/decline", map[string]string{"note": "busy"})
	peer.want(http.StatusNotFound, "GET", itemURL, nil)
	step("a peer declined, and lost access to the item")

	offer = member.want(http.StatusCreated, "POST", itemURL+"/send", map[string]string{"to": peerID})
	peer.want(http.StatusOK, "POST", "/v1/offers/"+offer["id"].(string)+"/accept", nil)
	item = peer.want(http.StatusOK, "GET", itemURL, nil)
	if !slices.Equal(owners(item), []string{peerID}) {
		fatal("owners = %v", owners(item))
	}
	step("a peer accepted, and ownership moved")

	assigned := manager.want(http.StatusCreated, "POST", "/v1/work-items", map[string]any{"title": "Close the books"})
	assignedURL := "/v1/work-items/" + assigned["id"].(string)
	sent := manager.want(http.StatusCreated, "POST", assignedURL+"/send", map[string]string{"to": memberID})
	if sent["state"] != "assigned" {
		fatal("a manager's send = %v", sent)
	}
	if got := owners(member.want(http.StatusOK, "GET", assignedURL, nil)); !slices.Equal(got, []string{memberID}) {
		fatal("owners = %v", got)
	}
	step("a manager assigned work, with no acceptance")

	up := member.want(http.StatusCreated, "POST", assignedURL+"/send", map[string]string{"to": managerID})
	if up["state"] != "pending" {
		fatal("a send up the org = %v", up)
	}
	member.want(http.StatusOK, "POST", "/v1/offers/"+up["id"].(string)+"/withdraw", nil)
	step("a send up the org stayed an offer")

	notes := member.want(http.StatusOK, "GET", "/v1/notifications", nil)["notifications"].([]any)
	kinds := []string{}
	for _, n := range notes {
		kinds = append(kinds, n.(map[string]any)["event"].(map[string]any)["kind"].(string))
	}
	if !strings.Contains(strings.Join(kinds, " "), "offer.declined") || !slices.Contains(kinds, "offer.assigned") {
		fatal("notifications = %v", kinds)
	}
	step("notifications arrived: %s", strings.Join(kinds, ", "))

	unit := admin.want(http.StatusCreated, "POST", "/v1/tenants/"+acme+"/units", map[string]string{"name": "Testclient team", "kind": "team"})["id"].(string)
	peerPlacement := "/v1/tenants/" + acme + "/members/" + peerID + "/placement"
	admin.want(http.StatusOK, "PUT", peerPlacement, map[string]string{"org_unit_id": unit})
	defer func() {
		admin.want(http.StatusOK, "PUT", peerPlacement, map[string]string{})
		admin.want(http.StatusNoContent, "DELETE", "/v1/tenants/"+acme+"/units/"+unit, nil)
	}()

	list := member.want(http.StatusCreated, "POST", "/v1/lists", map[string]string{"name": "Testclient backlog"})
	listURL := "/v1/lists/" + list["id"].(string)
	member.want(http.StatusOK, "PATCH", listURL, map[string]string{"org_unit_id": unit})
	peer.want(http.StatusOK, "GET", listURL, nil)
	step("linking a real org unit added its member to the list")

	backlog := member.want(http.StatusCreated, "POST", "/v1/work-items", map[string]any{
		"title": "Pick me up", "list_id": list["id"], "unowned": true,
	})
	claimed := peer.want(http.StatusOK, "POST", "/v1/work-items/"+backlog["id"].(string)+"/claim", nil)
	if !slices.Equal(owners(claimed), []string{peerID}) {
		fatal("owners = %v", owners(claimed))
	}
	step("a list member claimed an unowned item")

	manager.want(http.StatusOK, "PUT", "/v1/admin/settings", map[string]string{"manager_visibility": "direct_reports"})
	defer manager.want(http.StatusOK, "PUT", "/v1/admin/settings", map[string]string{"manager_visibility": "none"})
	member.want(http.StatusForbidden, "PUT", "/v1/admin/settings", map[string]string{"manager_visibility": "none"})
	own := member.want(http.StatusCreated, "POST", "/v1/work-items", map[string]any{"title": "My own work"})
	reports := manager.want(http.StatusOK, "GET", "/v1/work-items?view=reports", nil)["items"].([]any)
	if !slices.ContainsFunc(reports, func(i any) bool { return i.(map[string]any)["id"] == own["id"] }) {
		fatal("view=reports = %v", reports)
	}
	manager.want(http.StatusOK, "GET", "/v1/work-items/"+own["id"].(string), nil)
	manager.want(http.StatusNotFound, "PATCH", "/v1/work-items/"+own["id"].(string), map[string]any{"version": 1, "title": "x"})
	step("a tenant admin turned on manager visibility, which stayed read-only")

	outsider.want(http.StatusNotFound, "GET", itemURL, nil)
	step("another tenant can't see the item")

	if status, _ := (client{base: *collabURL, token: "not-a-token"}).call("GET", "/v1/work-items", nil); status != http.StatusUnauthorized {
		fatal("a bad token got %d", status)
	}
	step("a bad token was refused")
	fmt.Println("all good")
}
