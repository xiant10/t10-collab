package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOntologyConformance runs the workspace's ontology suite, vendored in
// third_party/ontology-conformance, against collab on the test database: ada
// in scope, bob a stranger in her tenant, and her assistant's tokens. It needs
// Node 24 or later.
func TestOntologyConformance(t *testing.T) {
	h := newHarness(t)
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node isn't installed")
		}
		t.Skip("node isn't installed")
	}
	ts := h.mcpServer()
	item := func(user, title string) string {
		return h.want(user, "POST", "/v1/work-items", map[string]any{"title": title}, http.StatusCreated)["id"].(string)
	}
	visible := item("ada", "Visible")
	item("ada", "Visible")
	item("ada", "Visible")
	bobs := item("bob", "Bob's")
	h.want("bob", "PUT", "/v1/work-items/"+bobs+"/collaborators/ada", nil, http.StatusNoContent)
	done := item("ada", "Done")
	h.want("ada", "PATCH", "/v1/work-items/"+done, map[string]any{"version": 1, "status": "done"}, http.StatusOK)

	config := map[string]any{
		"tokens":      map[string]string{"in_scope": acme + "/ada", "out_of_scope": acme + "/bob", "agent": "agent-assistant", "agent_read_only": "agent-read"},
		"visible":     ref(visible),
		"search":      map[string]any{"type": "work_item", "body": map[string]any{"filter": map[string]any{"property": "title", "op": "eq", "value": "Visible"}}, "limit": 2, "expect": 3},
		"forbidden":   map[string]any{"action": "work_item.send", "object": ref(bobs), "params": map[string]any{"to": "auth:user:dan"}},
		"wrong_state": map[string]any{"action": "work_item.send", "object": ref(done), "params": map[string]any{"to": "auth:user:dan"}},
		"apply":       map[string]any{"action": "work_item.edit", "object": ref(visible), "params": map[string]any{"priority": "high"}},
		"idempotent":  map[string]any{"action": "work_item.create", "params": map[string]any{"title": "Once"}},
		// The task just made is the newest, and a send puts someone else on it.
		"undo": map[string]any{"action": "work_item.create", "params": map[string]any{"title": "Undo me"},
			"dependent": map[string]any{"action": "work_item.send", "object": "$newest:work_item", "params": map[string]any{"to": "auth:user:bob"}}},
	}
	path := filepath.Join(t.TempDir(), "collab.json")
	raw, _ := json.Marshal(config)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--disable-warning=ExperimentalWarning", "../../third_party/ontology-conformance/conformance.ts", ts.URL, path).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("the ontology suite failed: %v", err)
	}
}
