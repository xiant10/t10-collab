package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/xiant10/t10-modkit/people"

	"t10-collab/internal/offers"
	"t10-collab/internal/policy"
	"t10-collab/internal/telemetry"
	"t10-collab/internal/workitems"
	"t10-collab/verifier"
)

const (
	mcpRoute = "/mcp"
	// JSON-RPC error codes.
	rpcParseError     = -32700
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	// maxPeopleIDs caps one work_people lookup, which reads each ID on its own.
	maxPeopleIDs = 50
)

// mcpVersions are the protocol versions spoken here, newest first.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

var errBadArguments = errors.New("the arguments don't fit this tool")

// tool is one agent tool. It calls the same services as the routes, so policy
// applies to it the same way. There are no work_send or work_delete tools: an
// agent reads and drafts, and only a person sends, accepts or deletes.
type tool struct {
	name, description string
	action            string
	schema            map[string]any
	run               func(ctx context.Context, c policy.Caller, args json.RawMessage) (any, error)
}

// args decodes a tool's arguments before run. A misnamed field is refused, since
// ignoring it could make a partial update look like it worked.
func args[A any](run func(ctx context.Context, c policy.Caller, a A) (any, error)) func(context.Context, policy.Caller, json.RawMessage) (any, error) {
	return func(ctx context.Context, c policy.Caller, raw json.RawMessage) (any, error) {
		var a A
		if len(raw) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&a); err != nil {
				return nil, errBadArguments
			}
		}
		return run(ctx, c, a)
	}
}

func object(required []string, properties map[string]any) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required}
}

func text(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func choice(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

// paged adds the page arguments every list tool takes.
func paged(properties map[string]any) map[string]any {
	properties["cursor"] = text("next_cursor from the previous page")
	properties["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": maxLimit, "description": "Page size, 50 by default"}
	return properties
}

// pageSize applies the same bounds as limit does on a route.
func pageSize(n int) (int, error) {
	switch {
	case n == 0:
		return defaultLimit, nil
	case n < 1 || n > maxLimit:
		return 0, errBadArguments
	}
	return n, nil
}

type pageArgs struct {
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

func (s *Server) tools() []tool {
	const id = "Work item ID, such as wi_ab12"
	return []tool{
		{
			name:        "work_list",
			description: "List the signed-in user's work items, which are tasks people give each other. Each has an id, title, status, owners and version.",
			action:      policy.ActionRead,
			schema: object(nil, paged(map[string]any{
				"view":    choice("Which items. Leave out for all the user can see", workitems.ViewOwned, workitems.ViewSent, workitems.ViewCollaborating, workitems.ViewList, workitems.ViewReports),
				"status":  choice("Only items in this status", workitems.StatusTodo, workitems.StatusDoing, workitems.StatusDone, workitems.StatusCancelled),
				"list_id": text("The shared list to read, needed with view list"),
			})),
			run: args(func(ctx context.Context, c policy.Caller, a struct {
				View   string `json:"view"`
				Status string `json:"status"`
				ListID string `json:"list_id"`
				pageArgs
			}) (any, error) {
				n, err := pageSize(a.Limit)
				if err != nil {
					return nil, err
				}
				items, next, err := s.Items.List(ctx, c, workitems.Filter{View: a.View, Status: a.Status, ListID: a.ListID, Cursor: a.Cursor, Limit: n})
				return map[string]any{"items": items, "next_cursor": next}, err
			}),
		},
		{
			name:        "work_get",
			description: "Read one work item in full. Its version is needed to update it.",
			action:      policy.ActionRead,
			schema:      object([]string{"id"}, map[string]any{"id": text(id)}),
			run: args(func(ctx context.Context, c policy.Caller, a struct {
				ID string `json:"id"`
			}) (any, error) {
				return s.Items.View(ctx, c, a.ID)
			}),
		},
		{
			name:        "work_comments",
			description: "Read the comments on a work item, oldest first.",
			action:      policy.ActionRead,
			schema:      object([]string{"id"}, paged(map[string]any{"id": text(id)})),
			run: args(func(ctx context.Context, c policy.Caller, a struct {
				ID string `json:"id"`
				pageArgs
			}) (any, error) {
				n, err := pageSize(a.Limit)
				if err != nil {
					return nil, err
				}
				comments, next, err := s.Items.Comments(ctx, c, a.ID, a.Cursor, n)
				return map[string]any{"comments": comments, "next_cursor": next}, err
			}),
		},
		{
			name:        "work_history",
			description: "Read what has happened to a work item, oldest first.",
			action:      policy.ActionRead,
			schema:      object([]string{"id"}, paged(map[string]any{"id": text(id)})),
			run: args(func(ctx context.Context, c policy.Caller, a struct {
				ID string `json:"id"`
				pageArgs
			}) (any, error) {
				n, err := pageSize(a.Limit)
				if err != nil {
					return nil, err
				}
				if _, err := s.Items.View(ctx, c, a.ID); err != nil {
					return nil, err
				}
				events, next, err := s.Activity.History(ctx, c.TenantID, a.ID, a.Cursor, n)
				return map[string]any{"events": events, "next_cursor": next}, err
			}),
		},
		{
			name:        "work_offers",
			description: "List work offered to the user that waits for an answer, or offers the user sent. Only the user can accept or decline one, in the app.",
			action:      policy.ActionRead,
			schema:      object([]string{"box"}, paged(map[string]any{"box": choice("Offers waiting for the user, or offers the user sent", offers.BoxInbox, offers.BoxSent)})),
			run: args(func(ctx context.Context, c policy.Caller, a struct {
				Box string `json:"box"`
				pageArgs
			}) (any, error) {
				n, err := pageSize(a.Limit)
				if err != nil {
					return nil, err
				}
				found, next, err := s.Offers.List(ctx, c, a.Box, a.Cursor, n)
				return map[string]any{"offers": found, "next_cursor": next}, err
			}),
		},
		{
			name:        "work_lists",
			description: "List the shared work lists the user belongs to.",
			action:      policy.ActionRead,
			schema:      object(nil, paged(map[string]any{"archived": map[string]any{"type": "boolean", "description": "List archived ones instead"}})),
			run: args(func(ctx context.Context, c policy.Caller, a struct {
				Archived bool `json:"archived"`
				pageArgs
			}) (any, error) {
				n, err := pageSize(a.Limit)
				if err != nil {
					return nil, err
				}
				found, next, err := s.Lists.Mine(ctx, c, a.Archived, a.Cursor, n)
				return map[string]any{"lists": found, "next_cursor": next}, err
			}),
		},
		{
			name:        "work_people",
			description: "Find colleagues by name with q, or turn the user IDs on a work item into names with ids.",
			action:      policy.ActionRead,
			schema: object(nil, paged(map[string]any{
				"q":   text("Part of a name"),
				"ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": maxPeopleIDs, "description": "User IDs to look up. Leave q out when using this"},
			})),
			run: args(s.toolPeople),
		},
		{
			name:        "work_create",
			description: "Create a work item owned by the user. It stays with the user: nothing is sent to anyone.",
			action:      policy.ActionWrite,
			schema: object([]string{"title"}, map[string]any{
				"title":       text("A short title"),
				"description": text("The detail"),
				"priority":    choice("Normal by default", "low", "normal", "high"),
				"due_at":      text("When it's due, as an RFC 3339 time"),
				"list_id":     text("Put it on this shared list"),
				"unowned":     map[string]any{"type": "boolean", "description": "With list_id: leave it for any list member to claim"},
			}),
			run: args(func(ctx context.Context, c policy.Caller, a struct {
				Title       string     `json:"title"`
				Description string     `json:"description"`
				Priority    string     `json:"priority"`
				DueAt       *time.Time `json:"due_at"`
				ListID      string     `json:"list_id"`
				Unowned     bool       `json:"unowned"`
			}) (any, error) {
				return s.Items.Create(ctx, c, workitems.NewItem{
					Title: a.Title, Description: a.Description, Priority: a.Priority, DueAt: a.DueAt, ListID: a.ListID, Unowned: a.Unowned,
				}, nil)
			}),
		},
		{
			name:        "work_update",
			description: "Change a work item's fields or status. Give the version from work_get. Leave out what shouldn't change. A version_conflict means someone else changed it, so read it again.",
			action:      policy.ActionWrite,
			schema: object([]string{"id", "version"}, map[string]any{
				"id":          text(id),
				"version":     map[string]any{"type": "integer", "description": "The version last read"},
				"title":       text("The new title"),
				"description": text("The new description"),
				"priority":    choice("The new priority", "low", "normal", "high"),
				"status":      choice("The new status", workitems.StatusTodo, workitems.StatusDoing, workitems.StatusDone, workitems.StatusCancelled),
				"due_at":      map[string]any{"type": []string{"string", "null"}, "description": "The new due time, as an RFC 3339 time, or null to clear it"},
			}),
			run: args(func(ctx context.Context, c policy.Caller, a struct {
				ID string `json:"id"`
				workitems.Patch
			}) (any, error) {
				return s.Items.Update(ctx, c, a.ID, a.Patch)
			}),
		},
		{
			name:        "work_comment",
			description: "Add a comment to a work item. Everyone on the item is told, and it shows as the user's.",
			action:      policy.ActionWrite,
			schema:      object([]string{"id", "body"}, map[string]any{"id": text(id), "body": text("The comment")}),
			run: args(func(ctx context.Context, c policy.Caller, a struct {
				ID   string `json:"id"`
				Body string `json:"body"`
			}) (any, error) {
				return s.Items.AddComment(ctx, c, a.ID, a.Body)
			}),
		},
	}
}

func (s *Server) toolPeople(ctx context.Context, c policy.Caller, a struct {
	Q   string   `json:"q"`
	IDs []string `json:"ids"`
	pageArgs
}) (any, error) {
	if len(a.IDs) > maxPeopleIDs || (len(a.IDs) > 0 && a.Q != "") {
		return nil, errBadArguments
	}
	if len(a.IDs) > 0 {
		found := []person{}
		for _, id := range a.IDs {
			member, err := s.Org.Member(ctx, c.TenantID, id)
			if errors.Is(err, people.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			found = append(found, personOf(member))
		}
		return map[string]any{"people": found}, nil
	}
	n, err := pageSize(a.Limit)
	if err != nil {
		return nil, err
	}
	members, next, err := s.Org.Search(ctx, c.TenantID, a.Q, "", a.Cursor, n)
	if err != nil {
		return nil, err
	}
	found := make([]person, len(members))
	for i, m := range members {
		found[i] = personOf(m)
	}
	return map[string]any{"people": found, "next_cursor": next}, nil
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func rpcResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func rpcError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	writeJSON(w, status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

// handleMCP is the tools side of MCP over Streamable HTTP. It holds no session,
// so any replica can answer, and it needs only POST: a server that offers no
// stream answers GET and DELETE with 405, as the mux does.
//
// Listing answers without a token, because agent-pi lists once at startup with
// no user, and the list holds nothing about any tenant. Only tools/call needs a
// token, and only a delegated one.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	// Nothing here serves a browser, and the protocol requires refusing one a
	// page sent, so that a local server can't be reached by DNS rebinding.
	if r.Header.Get("Origin") != "" {
		writeError(w, http.StatusForbidden, "forbidden", "this route doesn't serve browsers")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var message rpcMessage
	if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
		rpcError(w, http.StatusBadRequest, nil, rpcParseError, "the body isn't a JSON-RPC message")
		return
	}
	if message.ID == nil {
		// Notifications need no answer. A call is too short to be worth cancelling.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch message.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(message.Params, &params) != nil {
			rpcError(w, http.StatusOK, message.ID, rpcInvalidParams, "params aren't valid")
			return
		}
		version := mcpVersions[0]
		if slices.Contains(mcpVersions, params.ProtocolVersion) {
			version = params.ProtocolVersion
		}
		rpcResult(w, message.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "t10-collab", "version": "1"},
		})
	case "ping":
		rpcResult(w, message.ID, map[string]any{})
	case "tools/list":
		tools := s.tools()
		listed := make([]map[string]any, len(tools))
		for i, t := range tools {
			listed[i] = map[string]any{"name": t.name, "description": t.description, "inputSchema": t.schema}
		}
		rpcResult(w, message.ID, map[string]any{"tools": listed})
	case "tools/call":
		s.mcpCall(w, r, message)
	default:
		rpcError(w, http.StatusOK, message.ID, rpcMethodNotFound, "unknown method")
	}
}

func (s *Server) mcpCall(w http.ResponseWriter, r *http.Request, message rpcMessage) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			Traceparent string `json:"traceparent"`
		} `json:"_meta"`
	}
	if json.Unmarshal(message.Params, &params) != nil {
		rpcError(w, http.StatusOK, message.ID, rpcInvalidParams, "params aren't valid")
		return
	}
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		unauthorized(w)
		return
	}
	who, err := s.Verifier.Verify(r.Context(), raw)
	if errors.Is(err, verifier.ErrGrantUnavailable) {
		// The model can tell the person this. The token may well be fine.
		rpcResult(w, message.ID, toolFailure("access_unconfirmed", "this agent's access couldn't be confirmed, or it has stopped"))
		return
	}
	if err != nil {
		unauthorized(w)
		return
	}
	if who.Agent == nil || who.UserID == "" || who.TenantID == "" {
		writeError(w, http.StatusForbidden, "forbidden", "this token can't use this route")
		return
	}
	tools := s.tools()
	i := slices.IndexFunc(tools, func(t tool) bool { return t.name == params.Name })
	if i < 0 {
		rpcError(w, http.StatusOK, message.ID, rpcInvalidParams, "unknown tool")
		return
	}
	t := tools[i]
	switch {
	case !unconstrained(who):
		rpcResult(w, message.ID, toolFailure("forbidden", "this agent's grant has limits this service can't apply"))
		return
	case !who.Allows(t.action):
		rpcResult(w, message.ID, toolFailure("forbidden", "this agent isn't allowed to do this: it needs "+t.action))
		return
	}
	ctx, end := telemetry.Continue(r.Context(), params.Meta.Traceparent, "tool "+t.name)
	defer end()
	caller := policy.Caller{TenantID: who.TenantID, UserID: who.UserID, GrantID: who.Agent.GrantID}
	out, err := t.run(ctx, caller, params.Arguments)
	slog.Info("tool call", "tool", t.name, "grant", who.Agent.GrantID, "ok", err == nil)
	if err != nil {
		status, code, reason := classify(err)
		if status == http.StatusInternalServerError {
			slog.Error("tool failed", "tool", t.name, "error", err)
		}
		rpcResult(w, message.ID, toolFailure(code, reason))
		return
	}
	result, err := toolContent(out)
	if err != nil {
		slog.Error("encode tool result", "tool", t.name, "error", err)
		result = toolFailure("internal_error", "something went wrong")
	}
	rpcResult(w, message.ID, result)
}

// unconstrained reports that no grant sets a limit. Constraints are opaque to
// the auth service and none are defined here yet, so honoring a grant that has
// one would mean ignoring it.
func unconstrained(who *verifier.Identity) bool {
	return !slices.ContainsFunc(who.Constraints(), func(raw json.RawMessage) bool {
		trimmed := string(bytes.TrimSpace(raw))
		return trimmed != "" && trimmed != "null" && trimmed != "{}"
	})
}

// toolContent marks a result as data. Titles, descriptions and comments come
// from other people, and could otherwise steer the reader's agent. agent-pi's
// prompt treats <module_record> as data, and json.Marshal writes < as <, so
// someone's text can't close the tag.
func toolContent(out any) (map[string]any, error) {
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return map[string]any{"content": []map[string]any{{
		"type": "text", "text": "<module_record module=\"collab\">\n" + string(encoded) + "\n</module_record>",
	}}}, nil
}

// toolFailure is a refusal the calling model can read and act on. Protocol
// errors are kept for protocol mistakes.
func toolFailure(code, text string) map[string]any {
	return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": code + ": " + text}}}
}
