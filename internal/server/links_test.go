package server

import (
	"net/http"
	"testing"
)

func TestLinksOnAnItem(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	link := h.want("ada", "POST", "/v1/work-items/"+item+"/links",
		map[string]any{"url": "https://github.com/acme/app/pull/7", "title": "Fix export", "kind": "pull_request"}, http.StatusCreated)
	if link["kind"] != "pull_request" || link["added_by"] != "ada" {
		t.Fatalf("link = %v", link)
	}
	got := h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK)
	if links := got["links"].([]any); len(links) != 1 || links[0].(map[string]any)["url"] != "https://github.com/acme/app/pull/7" {
		t.Fatalf("item links = %v", got["links"])
	}
	for name, bad := range map[string]map[string]any{
		"http":        {"url": "http://example.com", "title": "x"},
		"javascript":  {"url": "javascript:alert(1)", "title": "x"},
		"credentials": {"url": "https://user:pw@example.com", "title": "x"},
		"long title":  {"url": "https://example.com", "title": string(make([]byte, 201))},
		"bad kind":    {"url": "https://example.com", "title": "x", "kind": "secret"},
	} {
		if status, _ := h.as("ada", "POST", "/v1/work-items/"+item+"/links", bad); status != http.StatusBadRequest {
			t.Errorf("%s: %d", name, status)
		}
	}
	// With no title or kind, the URL gives them.
	guessed := h.want("ada", "POST", "/v1/work-items/"+item+"/links", map[string]any{"url": "https://gitlab.com/acme/app/-/merge_requests/12/"}, http.StatusCreated)
	if guessed["kind"] != "pull_request" || guessed["title"] != "gitlab.com/acme/app/-/merge_requests/12" {
		t.Errorf("guessed = %v", guessed)
	}
	h.want("ada", "DELETE", "/v1/work-items/"+item+"/links/"+guessed["id"].(string), nil, http.StatusNoContent)
	// Someone who can't edit it can't link it either.
	if status, _ := h.as("bob", "POST", "/v1/work-items/"+item+"/links", map[string]any{"url": "https://example.com", "title": "x"}); status != http.StatusNotFound {
		t.Errorf("bob linked it: %d", status)
	}
	for range 19 {
		h.want("ada", "POST", "/v1/work-items/"+item+"/links", map[string]any{"url": "https://example.com", "title": "x"}, http.StatusCreated)
	}
	h.wantError("ada", "POST", "/v1/work-items/"+item+"/links", map[string]any{"url": "https://example.com", "title": "x"}, http.StatusConflict, "too_many_links")
	h.want("ada", "DELETE", "/v1/work-items/"+item+"/links/"+link["id"].(string), nil, http.StatusNoContent)
	h.want("ada", "DELETE", "/v1/work-items/"+item+"/links/"+link["id"].(string), nil, http.StatusNotFound)
	history := h.want("ada", "GET", "/v1/work-items/"+item+"/events", nil, http.StatusOK)
	kinds := map[string]int{}
	for _, e := range history["events"].([]any) {
		kinds[e.(map[string]any)["kind"].(string)]++
	}
	if kinds["link.added"] != 21 || kinds["link.removed"] != 2 {
		t.Errorf("history = %v", kinds)
	}
}

// An agent adds a link with work_link, and no agent is woken by it.
func TestAgentLinksItsWork(t *testing.T) {
	h := newHarness(t)
	ts := h.mcpServer()
	created := call(t, ts.URL, "agent-write", "work_create", map[string]any{"title": "Fix export"})
	id := created.data["id"].(string)
	linked := call(t, ts.URL, "agent-write", "work_link", map[string]any{"id": id, "url": "https://github.com/acme/app/pull/7", "title": "Fix export", "kind": "pull_request"})
	if linked.failed || linked.data["url"] != "https://github.com/acme/app/pull/7" {
		t.Fatalf("work_link = %+v", linked)
	}
	if got := call(t, ts.URL, "agent-write", "work_get", map[string]any{"id": id}); len(got.data["links"].([]any)) != 1 {
		t.Errorf("work_get links = %+v", got.data["links"])
	}
	if got := call(t, ts.URL, "agent-read", "work_link", map[string]any{"id": id, "url": "https://example.com", "title": "x"}); !got.failed && got.text != "" {
		t.Errorf("a read-only run linked: %+v", got)
	}
	if rows := h.outbox(); len(rows) != 0 {
		t.Errorf("outbox = %v", rows)
	}
}
