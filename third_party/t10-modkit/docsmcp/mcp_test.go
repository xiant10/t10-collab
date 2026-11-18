package docsmcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func fm(audience, kind, status, title string) string {
	return "---\ntitle: \"" + title + "\"\naudience: " + audience + "\nkind: " + kind + "\nstatus: " + status + "\nsummary: \"About " + title + ".\"\n---\n\n# " + title + "\n\n"
}

var docs = fstest.MapFS{
	"api.md":          {Data: []byte(fm("consumer", "reference", "current", "Widgets API") + "Intro.\n\n## Widgets\n\nCreate a widget with `POST /v1/widgets`.\n\n### Paging\n\nWidgets page by cursor.\n\n```\n# not a heading\n```\n\n## Errors\n\nAll errors are JSON.\n")},
	"guides/start.md": {Data: []byte(fm("consumer", "guide", "current", "Getting started") + "Make a widget first.\n")},
	"design.md":       {Data: []byte(fm("contributor", "design", "current", "Internals") + "Secret widget internals.\n")},
	"records/old.md":  {Data: []byte(fm("contributor", "record", "record", "Old plan") + "Widgets, long ago.\n")},
	"plain.md":        {Data: []byte("# No front matter\n\nWidgets.\n")},
	"next.md":         {Data: []byte(fm("consumer", "design", "proposed", "Gadgets") + "Gadgets will come.\n")},
	"openapi.yaml":    {Data: []byte("openapi: 3.1.0\npaths:\n  /v1/widgets:\n    post:\n      operationId: createWidget\n      summary: Create a widget\n      requestBody:\n        content:\n          application/json:\n            schema: {$ref: '#/components/schemas/Widget'}\n      responses:\n        201: {description: Created}\ncomponents:\n  schemas:\n    Widget:\n      type: object\n      properties:\n        name: {type: string}\n        parent: {$ref: '#/components/schemas/Widget'}\n")},
	"docs.go":         {Data: []byte("package docs")},
}

func handler(t *testing.T) *Handler {
	t.Helper()
	h, err := New(Options{Component: "t10-widgets", Summary: "Widgets for everyone.", Version: "abc123", Docs: docs,
		AgentTools: func() []Tool {
			return []Tool{{Name: "widget_list", Description: "List widgets", Action: "widget:read", InputSchema: map[string]any{"type": "object"}}}
		}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func rpc(t *testing.T, h http.Handler, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", Route, strings.NewReader(string(body))))
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d", method, w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func call(t *testing.T, h http.Handler, tool string, args map[string]any) map[string]any {
	t.Helper()
	out := rpc(t, h, "tools/call", map[string]any{"name": tool, "arguments": args})
	if out["error"] != nil {
		t.Fatalf("%s: %v", tool, out["error"])
	}
	return out["result"].(map[string]any)
}

func TestServesOnlyConsumerDocs(t *testing.T) {
	h := handler(t)
	var ids []string
	for _, d := range h.Docs() {
		ids = append(ids, d.ID)
	}
	if got := strings.Join(ids, ","); got != "guides/start,api,next" {
		t.Errorf("served %s", got)
	}
	res := call(t, h, "docs_read", map[string]any{"id": "design"})
	if res["isError"] != true {
		t.Error("a contributor doc was served")
	}
	if hits := call(t, h, "docs_search", map[string]any{"query": "secret"})["structuredContent"].(map[string]any)["results"].([]any); len(hits) != 0 {
		t.Errorf("search found a contributor doc: %v", hits)
	}
}

func TestOverview(t *testing.T) {
	s := call(t, handler(t), "docs_overview", nil)["structuredContent"].(map[string]any)
	if s["component"] != "t10-widgets" || s["version"] != "abc123" || s["agent_tools"] != true || s["api_operations"] != float64(1) {
		t.Errorf("overview %v", s)
	}
	if len(s["docs"].([]any)) != 3 {
		t.Errorf("docs %v", s["docs"])
	}
}

func TestReadSection(t *testing.T) {
	h := handler(t)
	res := call(t, h, "docs_read", map[string]any{"id": "api", "section": "widgets"})
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "## Widgets") || !strings.Contains(text, "### Paging") || strings.Contains(text, "## Errors") {
		t.Errorf("section:\n%s", text)
	}
	if res := call(t, h, "docs_read", map[string]any{"id": "api", "section": "Nope"}); res["isError"] != true {
		t.Error("an unknown section was read")
	}
	sections := call(t, h, "docs_read", map[string]any{"id": "api"})["structuredContent"].(map[string]any)["sections"].([]any)
	for _, s := range sections {
		if strings.Contains(s.(string), "not a heading") {
			t.Error("a comment in a code block was read as a heading")
		}
	}
}

func TestSearch(t *testing.T) {
	hits := call(t, handler(t), "docs_search", map[string]any{"query": "widget cursor"})["structuredContent"].(map[string]any)["results"].([]any)
	if len(hits) == 0 || hits[0].(map[string]any)["section"] != "paging" {
		t.Errorf("hits %v", hits)
	}
}

func TestAPI(t *testing.T) {
	h := handler(t)
	ops := call(t, h, "docs_api", nil)["structuredContent"].(map[string]any)["operations"].([]any)
	if len(ops) != 1 || ops[0].(map[string]any)["operation_id"] != "createWidget" {
		t.Fatalf("operations %v", ops)
	}
	for _, name := range []string{"createWidget", "post /v1/widgets"} {
		op := call(t, h, "docs_api", map[string]any{"operation": name})["structuredContent"].(map[string]any)
		schema := op["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
		if schema["type"] != "object" {
			t.Errorf("%s: schema not resolved: %v", name, schema)
		}
		if _, ok := op["responses"].(map[string]any)["201"]; !ok {
			t.Errorf("%s: responses %v", name, op["responses"])
		}
	}
}

func TestProtocol(t *testing.T) {
	h := handler(t)
	init := rpc(t, h, "initialize", map[string]any{"protocolVersion": "2025-06-18"})["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" || init["serverInfo"].(map[string]any)["name"] != "t10-widgets" {
		t.Errorf("initialize %v", init)
	}
	if out := rpc(t, h, "nope", nil); out["error"].(map[string]any)["code"] != float64(methodNotFound) {
		t.Errorf("unknown method %v", out)
	}
	res := rpc(t, h, "resources/list", nil)["result"].(map[string]any)["resources"].([]any)
	if len(res) != 3 || res[0].(map[string]any)["uri"] != "t10://t10-widgets/guides/start" {
		t.Errorf("resources %v", res)
	}
	read := rpc(t, h, "resources/read", map[string]any{"uri": "t10://t10-widgets/api"})["result"].(map[string]any)
	if !strings.HasPrefix(read["contents"].([]any)[0].(map[string]any)["text"].(string), "# Widgets API") {
		t.Errorf("read %v", read)
	}

	for _, tc := range []struct {
		method, body, origin string
		status               int
	}{
		{"GET", "", "", http.StatusMethodNotAllowed},
		{"POST", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "", http.StatusAccepted},
		{"POST", `{`, "", http.StatusBadRequest},
		{"POST", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "https://evil.example", http.StatusForbidden},
	} {
		r := httptest.NewRequest(tc.method, Route, strings.NewReader(tc.body))
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.body, w.Code, tc.status)
		}
	}
}

func TestBadFrontMatter(t *testing.T) {
	_, err := New(Options{Component: "x", Docs: fstest.MapFS{"a.md": {Data: []byte("---\ntitle: [\n---\n")}}})
	if err == nil {
		t.Error("broken front matter was accepted")
	}
}
