package ontology

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/xiant10/t10-modkit/docsmcp"
)

// Tool is one generated MCP tool. The module's MCP server lists Tool, checks
// that the agent's grant holds Tool.Action, builds the caller for the person
// the grant acts for, narrowed by it, and calls Run with the agent's name.
type Tool[C any] struct {
	docsmcp.Tool
	// Run answers an MCP tools/call result: content marked as data, or
	// isError with "code: message".
	Run func(ctx context.Context, c C, agent string, args json.RawMessage) map[string]any
}

// agentGrantKey holds what the agent's grant allows, from WithAgentGrant.
type agentGrantKey struct{}

// WithAgentGrant tells the generated tools which grant actions the calling
// agent holds. The MCP server that runs a tool passes its verified token's
// check, such as the verifier's Identity.Allows. Without it, the tools take
// the agent to hold no <prefix>:apply, so they draft every propose action.
func WithAgentGrant(ctx context.Context, allows func(action string) bool) context.Context {
	return context.WithValue(ctx, agentGrantKey{}, allows)
}

func agentAllows(ctx context.Context, action string) bool {
	allows, _ := ctx.Value(agentGrantKey{}).(func(string) bool)
	return allows != nil && allows(action)
}

// agentApplies reports whether the calling agent may apply an action through
// <prefix>_act: an undoable one under <prefix>:act, and a propose one under
// <prefix>:act and <prefix>:apply, which the person chose to grant.
func (r *Registry[C]) agentApplies(ctx context.Context, tier Tier) bool {
	p := r.opts.Prefix
	switch tier {
	case Undoable:
		return agentAllows(ctx, p+":act")
	case Propose:
		return agentAllows(ctx, p+":act") && agentAllows(ctx, p+":apply")
	}
	return false
}

// Tools are the generated tools: describe, search, get and propose always, and
// act exactly when the core declares an undoable or a propose action. Their
// names, descriptions and schemas depend on no tenant.
func (r *Registry[C]) Tools() []Tool[C] {
	p := r.opts.Prefix
	read, propose, act := p+":read", p+":propose", p+":act"
	refText := "A reference, " + r.opts.Module + ":<type>:<id>, from an earlier result."
	tools := []Tool[C]{
		{
			Tool: docsmcp.Tool{Name: p + "_describe", Action: read, InputSchema: schema(nil, map[string]any{}),
				Description: "Describe what this module holds for the user's company: its object types and what each is for, their properties (sensitive ones marked, and always masked for you), links, and the actions you may propose or take, with this company's own values such as stages and kinds. Call it before searching a type or proposing an action for the first time."},
			Run: r.toolDescribe,
		},
		{
			Tool: docsmcp.Tool{Name: p + "_search", Action: read,
				Description: "Search one object type, with the user's rights: only what they may see comes back, and sensitive values are masked. filter is one condition {property, op, value} or {and: [conditions]}; op is eq, in, lt, gt or contains (for text). You can't filter or sort on a sensitive property. Page with next_cursor.",
				InputSchema: schema([]string{"type"}, map[string]any{
					"type":   text("An object type from " + p + "_describe."),
					"filter": map[string]any{"type": "object", "description": "One condition, or {and: [conditions]}."},
					"sort": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
						"property": text("A property of the type."), "desc": map[string]any{"type": "boolean"}}},
					"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": MaxLimit},
					"cursor": text("next_cursor from the page before."),
				})},
			Run: r.toolSearch,
		},
		{
			Tool: docsmcp.Tool{Name: p + "_get", Action: read,
				Description: "Read one object with the user's rights, with the actions on it and whether each is allowed now. Sensitive values are masked. sources names a derived property, such as a score, to get the records its value came from, which you can cite.",
				InputSchema: schema([]string{"ref"}, map[string]any{"ref": text(refText), "sources": text("A derived property whose sources you want.")})},
			Run: r.toolGet,
		},
		{
			Tool: docsmcp.Tool{Name: p + "_propose", Action: propose,
				Description: "Draft an action for the user to confirm. Nothing changes until they do, so never say it's done: say it's drafted and waiting for them. action is one from " + p + "_describe; object is the reference it acts on, left out for one that creates; params as the action declares them. Money, consent and access actions can't be drafted.",
				InputSchema: schema([]string{"action"}, map[string]any{
					"action": text("An action from " + p + "_describe."),
					"object": text(refText),
					"params": map[string]any{"type": "object", "description": "The action's parameters."},
				})},
			Run: r.toolPropose,
		},
	}
	core := r.Core()
	if slices.ContainsFunc(sortedKeys(core.ActionTypes), func(k string) bool { return core.ActionTypes[k].Agent != Never }) {
		tools = append(tools, Tool[C]{
			Tool: docsmcp.Tool{Name: p + "_act", Action: act,
				Description: "Take an action for the user, under their rights, with no draft. Only actions " + p + "_describe marks agent_applies: undoable ones, and others only when the user lets you act without asking. Give the object's revision from your last read, and a request_id of your own: retrying with the same request_id never applies it twice. Say what you did; say they can undo it only when the answer has an undo ID.",
				InputSchema: schema([]string{"action", "request_id"}, map[string]any{
					"action":     text("An action from " + p + "_describe marked agent_applies."),
					"object":     text(refText),
					"revision":   map[string]any{"type": "integer", "minimum": 1, "description": "The object's revision from your last read."},
					"params":     map[string]any{"type": "object", "description": "The action's parameters."},
					"request_id": map[string]any{"type": "string", "minLength": 8, "maxLength": 100, "description": "Your own ID for this request, the same on a retry."},
				})},
			Run: r.toolAct,
		})
	}
	return tools
}

func schema(required []string, properties map[string]any) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func text(description string) map[string]any {
	return map[string]any{"type": "string", "maxLength": 300, "description": description}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// toolResult marks a result as data, because people wrote the text in it.
// json.Marshal escapes <, so text can't close the tag.
func (r *Registry[C]) toolResult(out any) map[string]any {
	encoded, err := json.Marshal(out)
	if err != nil {
		return toolFailure(errUnavailable)
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": "<module_record module=\"" + r.opts.Module + "\">\n" + string(encoded) + "\n</module_record>"}},
		"structuredContent": map[string]any{"module": r.opts.Module},
	}
}

func toolFailure(err error) map[string]any {
	e := asError(err)
	text := e.Code
	if e.Message != "" {
		text += ": " + e.Message
	}
	if len(e.Errors) > 0 {
		encoded, _ := json.Marshal(e.Errors)
		text += " " + string(encoded)
	}
	return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": text}}}
}

func decodeArgs(raw json.RawMessage, into any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(into) != nil {
		return Fail(http.StatusBadRequest, "invalid_arguments", "the arguments don't fit this tool")
	}
	return nil
}

// required refuses arguments that leave out what the tool's schema requires, as invalid_arguments,
// the same as a module's own tools.
func required(fields map[string]string) error {
	for _, name := range sortedKeys(fields) {
		if fields[name] == "" {
			return Fail(http.StatusBadRequest, "invalid_arguments", name+" is required")
		}
	}
	return nil
}

func asAgent(ctx context.Context) context.Context { return context.WithValue(ctx, agentKey{}, true) }

func (r *Registry[C]) toolDescribe(ctx context.Context, c C, _ string, args json.RawMessage) map[string]any {
	if err := decodeArgs(args, &struct{}{}); err != nil {
		return toolFailure(err)
	}
	d, err := r.Describe(asAgent(ctx), c)
	if err != nil {
		return toolFailure(err)
	}
	// Only what an agent may propose or act on is described, each saying
	// whether this agent may apply it now.
	for name, a := range d.ActionTypes {
		if a.Agent == Never {
			delete(d.ActionTypes, name)
			delete(d.Tenant.ActionTypes, name)
			continue
		}
		applies := r.agentApplies(ctx, a.Agent)
		a.AgentApplies = &applies
		d.ActionTypes[name] = a
	}
	d.Restricted = []string{}
	return r.toolResult(d)
}

func (r *Registry[C]) toolSearch(ctx context.Context, c C, _ string, args json.RawMessage) map[string]any {
	var a struct {
		Type   string          `json:"type"`
		Filter json.RawMessage `json:"filter"`
		Sort   json.RawMessage `json:"sort"`
		Limit  *int            `json:"limit"`
		Cursor string          `json:"cursor"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return toolFailure(err)
	}
	if err := required(map[string]string{"type": a.Type}); err != nil {
		return toolFailure(err)
	}
	body := map[string]any{"cursor": a.Cursor}
	if len(a.Filter) > 0 {
		body["filter"] = a.Filter
	}
	if len(a.Sort) > 0 {
		body["sort"] = a.Sort
	}
	if a.Limit != nil {
		body["limit"] = *a.Limit
	}
	raw, _ := json.Marshal(body)
	out, err := r.Search(asAgent(ctx), c, a.Type, raw)
	if err != nil {
		return toolFailure(err)
	}
	return r.toolResult(out)
}

func (r *Registry[C]) toolGet(ctx context.Context, c C, _ string, args json.RawMessage) map[string]any {
	var a struct {
		Ref     string `json:"ref"`
		Sources string `json:"sources"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return toolFailure(err)
	}
	if err := required(map[string]string{"ref": a.Ref}); err != nil {
		return toolFailure(err)
	}
	module, objectType, id, ok := SplitRef(a.Ref)
	if !ok || module != r.opts.Module {
		return toolFailure(ErrNotFound)
	}
	out, err := r.GetObject(asAgent(ctx), c, objectType, id, a.Sources)
	if err != nil {
		return toolFailure(err)
	}
	return r.toolResult(out)
}

func (r *Registry[C]) toolPropose(ctx context.Context, c C, agent string, args json.RawMessage) map[string]any {
	var a struct {
		Action string          `json:"action"`
		Object string          `json:"object"`
		Params json.RawMessage `json:"params"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return toolFailure(err)
	}
	if err := required(map[string]string{"action": a.Action}); err != nil {
		return toolFailure(err)
	}
	p, errs, err := r.Propose(asAgent(ctx), c, agent, a.Action, ActionRequest{Object: a.Object, Params: a.Params})
	if err != nil {
		return toolFailure(err)
	}
	if len(errs) > 0 {
		return toolFailure(refusal(errs))
	}
	return r.toolResult(map[string]any{"proposal": p.ID, "ref": r.ref("proposal", p.ID), "expires_at": p.ExpiresAt})
}

func (r *Registry[C]) toolAct(ctx context.Context, c C, agent string, args json.RawMessage) map[string]any {
	var a struct {
		Action    string          `json:"action"`
		Object    string          `json:"object"`
		Revision  int64           `json:"revision"`
		Params    json.RawMessage `json:"params"`
		RequestID string          `json:"request_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return toolFailure(err)
	}
	if err := required(map[string]string{"action": a.Action, "request_id": a.RequestID}); err != nil {
		return toolFailure(err)
	}
	action, ok := r.actions[a.Action]
	if !ok {
		return toolFailure(Fail(http.StatusNotFound, "unknown_action", "the descriptor declares no action "+a.Action))
	}
	if action.info.Agent == Never || (action.info.Agent == Propose && !r.agentApplies(ctx, Propose)) {
		return toolFailure(ErrNotAllowedForAgent)
	}
	if len(a.RequestID) < 8 || len(a.RequestID) > 100 {
		return toolFailure(Fail(http.StatusBadRequest, "invalid_arguments", "request_id is 8 to 100 characters"))
	}
	out, err := r.Apply(asAgent(ctx), c, a.Action, ActionRequest{Object: a.Object, Revision: a.Revision, Params: a.Params},
		Input{Via: "agent", Agent: agent, IdempotencyKey: a.RequestID, AutoApply: action.info.Agent == Propose})
	if err != nil {
		return toolFailure(err)
	}
	return r.toolResult(out)
}
