package docsmcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"sort"
	"strings"
)

// Route is where every image serves its docs.
const Route = "/docs/mcp"

// Options says what to serve.
type Options struct {
	// Component is the image's name, such as t10-collab.
	Component string
	// Summary is one sentence on what it offers someone building on t10.
	Summary string
	// Version is the commit the image was built from. Empty reads as "unknown".
	Version string
	// Docs is the repo's docs/ folder. Its openapi.yaml, if there is one, backs docs_api.
	Docs fs.FS
	// AgentTools, if set, backs docs_agent_tools: the tools the component serves agents.
	AgentTools func() []Tool
}

// Tool is one agent tool, as docs_agent_tools lists it.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Action      string         `json:"action,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Handler answers MCP over Streamable HTTP at Route.
type Handler struct {
	opts Options
	docs map[string]*Doc
	ids  []string
	spec *spec
}

// New reads the docs once. A doc with broken front matter is an error, so a
// test that calls New keeps the image's docs servable.
func New(o Options) (*Handler, error) {
	if o.Component == "" || o.Docs == nil {
		return nil, errors.New("docsmcp: Component and Docs are required")
	}
	if o.Version == "" {
		o.Version = "unknown"
	}
	docs, err := load(o.Docs)
	if err != nil {
		return nil, err
	}
	h := &Handler{opts: o, docs: docs}
	for id := range docs {
		h.ids = append(h.ids, id)
	}
	sort.Slice(h.ids, func(i, j int) bool {
		return order(docs[h.ids[i]]) < order(docs[h.ids[j]]) || order(docs[h.ids[i]]) == order(docs[h.ids[j]]) && h.ids[i] < h.ids[j]
	})
	if raw, err := fs.ReadFile(o.Docs, "openapi.yaml"); err == nil {
		if h.spec, err = loadSpec(raw); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return h, nil
}

// order puts guides first, then references, then designs.
func order(d *Doc) int {
	return map[string]int{"guide": 0, "reference": 1, "design": 2}[d.Kind]
}

// Docs lists what's served, in overview order.
func (h *Handler) Docs() []Doc {
	out := make([]Doc, len(h.ids))
	for i, id := range h.ids {
		out[i] = *h.docs[id]
	}
	return out
}

// Versions this server speaks, newest first.
var versions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const (
	parseError     = -32700
	invalidRequest = -32600
	methodNotFound = -32601
	invalidParams  = -32602
	notFound       = -32002
	maxBody        = 64 << 10
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// ServeHTTP takes one JSON-RPC message per POST, with no session or stream. It
// needs no token and ignores one. It refuses a request a browser page sent, as
// the protocol asks, so a local server can't be reached by DNS rebinding.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "this route takes POST", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Origin") != "" {
		http.Error(w, "this route doesn't serve browsers", http.StatusForbidden)
		return
	}
	var m message
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&m); err != nil {
		writeError(w, http.StatusBadRequest, nil, parseError, "the body isn't a JSON-RPC message")
		return
	}
	if m.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if m.JSONRPC != "2.0" || m.Method == "" {
		writeError(w, http.StatusOK, m.ID, invalidRequest, "not a JSON-RPC 2.0 request")
		return
	}
	result, code, msg := h.answer(m)
	if code != 0 {
		writeError(w, http.StatusOK, m.ID, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
}

func (h *Handler) answer(m message) (any, int, string) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(m.Params) > 0 && json.Unmarshal(m.Params, &p) != nil {
			return nil, invalidParams, "params aren't valid"
		}
		version := versions[0]
		if slices.Contains(versions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}, "resources": map[string]any{}},
			"serverInfo":      map[string]any{"name": h.opts.Component, "title": h.opts.Component + " docs", "version": h.opts.Version},
			"instructions":    fmt.Sprintf("The docs of %s for people building on t10. Start with docs_overview, then docs_search or docs_read.", h.opts.Component),
		}, 0, ""
	case "ping":
		return map[string]any{}, 0, ""
	case "tools/list":
		return map[string]any{"tools": h.tools()}, 0, ""
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.Name == "" {
			return nil, invalidParams, "params need a tool name"
		}
		return h.call(p.Name, p.Arguments)
	case "resources/list":
		return map[string]any{"resources": h.resources()}, 0, ""
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.URI == "" {
			return nil, invalidParams, "params need a uri"
		}
		return h.read(p.URI)
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, 0, ""
	default:
		return nil, methodNotFound, "unknown method"
	}
}

func object(required []string, properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

var readOnly = map[string]any{"readOnlyHint": true, "openWorldHint": false}

func (h *Handler) tools() []map[string]any {
	c := h.opts.Component
	tools := []map[string]any{
		{"name": "docs_overview", "description": "What " + c + " offers someone building on t10, its version, and every doc it serves with a one-line summary. Start here.",
			"inputSchema": object([]string{}, map[string]any{})},
		{"name": "docs_search", "description": "Search " + c + "'s docs. Answers matching sections, best first, each with the id and section to pass to docs_read.",
			"inputSchema": object([]string{"query"}, map[string]any{
				"query": str("Words to look for. Every word must appear in a doc for it to match."),
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "At most this many results. Default 10."},
			})},
		{"name": "docs_read", "description": "Read one of " + c + "'s docs, whole or one section of it.",
			"inputSchema": object([]string{"id"}, map[string]any{
				"id":      str("The doc's id, as docs_overview or docs_search gives it, such as \"api\"."),
				"section": str("Optional. A heading, or its anchor such as \"work-items\". The section comes with its subsections."),
			})},
	}
	if h.spec != nil {
		tools = append(tools, map[string]any{"name": "docs_api", "description": "The HTTP routes " + c + " serves, from its OpenAPI spec. With no operation, every route; with one, its parameters, body and responses, with schemas resolved.",
			"inputSchema": object([]string{}, map[string]any{
				"operation": str("Optional. An operationId, or a method and path such as \"GET /v1/work-items\"."),
			})})
	}
	if h.opts.AgentTools != nil {
		tools = append(tools, map[string]any{"name": "docs_agent_tools", "description": "The MCP tools " + c + " serves agents running in t10: names, descriptions, inputs and the action each needs on a delegated token.",
			"inputSchema": object([]string{}, map[string]any{})})
	}
	for _, t := range tools {
		t["annotations"] = readOnly
	}
	return tools
}

func (h *Handler) call(name string, raw json.RawMessage) (any, int, string) {
	var args map[string]any
	if len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &args) != nil {
		return nil, invalidParams, "arguments must be an object"
	}
	text := func(k string) (string, error) {
		v, ok := args[k]
		if !ok || v == nil {
			return "", nil
		}
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("%s must be a string", k)
		}
		return s, nil
	}
	switch name {
	case "docs_overview":
		return h.overview(), 0, ""
	case "docs_search":
		q, err := text("query")
		if err != nil {
			return failure(err.Error()), 0, ""
		}
		limit := 10
		if v, ok := args["limit"].(float64); ok {
			limit = min(max(int(v), 1), 50)
		}
		if len(terms(q)) == 0 {
			return failure("query needs at least one word"), 0, ""
		}
		hits := search(h.docs, q, limit)
		if hits == nil {
			hits = []Hit{}
		}
		var b strings.Builder
		if len(hits) == 0 {
			b.WriteString("No section matches. Try fewer or other words, or docs_overview for the list of docs.")
		}
		for _, hit := range hits {
			fmt.Fprintf(&b, "- %s, section %q: %s\n  %s\n", hit.ID, hit.Section, hit.Title, hit.Snippet)
		}
		return result(b.String(), map[string]any{"results": hits}), 0, ""
	case "docs_read":
		id, err := text("id")
		if err != nil {
			return failure(err.Error()), 0, ""
		}
		sec, err := text("section")
		if err != nil {
			return failure(err.Error()), 0, ""
		}
		d, ok := h.docs[strings.TrimSuffix(strings.TrimPrefix(id, "/"), ".md")]
		if !ok {
			return failure(fmt.Sprintf("no doc %q. docs_overview lists them.", id)), 0, ""
		}
		meta := map[string]any{"id": d.ID, "title": d.Title, "kind": d.Kind, "status": d.Status, "summary": d.Summary, "sections": d.headings()}
		body := d.body
		if sec != "" {
			s, ok := d.find(sec)
			if !ok {
				return failure(fmt.Sprintf("%s has no section %q. Its sections: %s", d.ID, sec, strings.Join(d.headings(), "; "))), 0, ""
			}
			body = d.body[s.start:s.end]
			meta["section"] = s.slug
		}
		return result(fmt.Sprintf("<!-- %s/%s · %s · %s -->\n\n%s", h.opts.Component, d.ID, d.Kind, d.Status, body), meta), 0, ""
	case "docs_api":
		if h.spec == nil {
			return nil, invalidParams, "unknown tool"
		}
		op, err := text("operation")
		if err != nil {
			return failure(err.Error()), 0, ""
		}
		if op == "" {
			var b strings.Builder
			for _, o := range h.spec.ops {
				fmt.Fprintf(&b, "%s %s  %s  %s\n", o.Method, o.Path, o.OperationID, o.Summary)
			}
			return result(b.String(), map[string]any{"operations": h.spec.ops}), 0, ""
		}
		found, ok := h.spec.operation(op)
		if !ok {
			return failure(fmt.Sprintf("no operation %q. Call docs_api with no operation for the list.", op)), 0, ""
		}
		raw, _ := json.MarshalIndent(found, "", "  ")
		return result(string(raw), found), 0, ""
	case "docs_agent_tools":
		if h.opts.AgentTools == nil {
			return nil, invalidParams, "unknown tool"
		}
		tools := h.opts.AgentTools()
		raw, _ := json.MarshalIndent(tools, "", "  ")
		return result(string(raw), map[string]any{"tools": tools}), 0, ""
	default:
		return nil, invalidParams, "unknown tool"
	}
}

func (h *Handler) overview() map[string]any {
	docs := h.Docs()
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%s\n\nVersion %s. Read a doc with docs_read and its id.\n", h.opts.Component, h.opts.Summary, h.opts.Version)
	heads := map[string]string{"guide": "Guides", "reference": "Reference", "design": "Design"}
	last := ""
	for _, d := range docs {
		if d.Kind != last {
			fmt.Fprintf(&b, "\n## %s\n\n", heads[d.Kind])
			last = d.Kind
		}
		note := ""
		if d.Status == "proposed" {
			note = " (proposed, not built)"
		}
		fmt.Fprintf(&b, "- %s: %s%s. %s\n", d.ID, d.Title, note, d.Summary)
	}
	ops := 0
	if h.spec != nil {
		ops = len(h.spec.ops)
		fmt.Fprintf(&b, "\ndocs_api lists the %d HTTP routes in its OpenAPI spec.\n", ops)
	}
	if h.opts.AgentTools != nil {
		fmt.Fprintf(&b, "docs_agent_tools lists the tools it serves agents.\n")
	}
	return result(b.String(), map[string]any{
		"component": h.opts.Component, "summary": h.opts.Summary, "version": h.opts.Version,
		"docs": docs, "api_operations": ops, "agent_tools": h.opts.AgentTools != nil,
	})
}

func (h *Handler) uri(id string) string { return "t10://" + h.opts.Component + "/" + id }

func (h *Handler) resources() []map[string]any {
	out := []map[string]any{}
	for _, id := range h.ids {
		d := h.docs[id]
		out = append(out, map[string]any{"uri": h.uri(id), "name": id, "title": d.Title, "description": d.Summary, "mimeType": "text/markdown"})
	}
	return out
}

func (h *Handler) read(uri string) (any, int, string) {
	id, ok := strings.CutPrefix(uri, "t10://"+h.opts.Component+"/")
	d, found := h.docs[id]
	if !ok || !found {
		return nil, notFound, "unknown resource"
	}
	return map[string]any{"contents": []map[string]any{{"uri": uri, "mimeType": "text/markdown", "text": d.body}}}, 0, ""
}

func result(text string, structured map[string]any) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "structuredContent": structured}
}

func failure(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": true}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	writeJSON(w, status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}
