// Package client uses any module's ontology over the HTTP contract
// (contracts/ontology in the workspace), from a service or a server-drawn page.
// It finds ontologies only from the modules it is configured with, names no
// module itself, and calls with the person's own token for this request, never a
// service's own or a delegated one. web/ontology.ts is the same client for React
// apps, held to the same recorded answers.
package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/xiant10/t10-modkit/ontology"
)

// Module is one configured module: its ID and the base URL its routes are under.
type Module struct {
	ID, URL string
}

// Error is a module's refusal, with its status and code unchanged. A refusal
// before sending, such as person_token_required, has status 0.
type Error struct {
	Status  int
	Code    string
	Message string
	Errors  []ontology.FieldError
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return e.Code
	}
	return fmt.Sprintf("%d %s", e.Status, e.Code)
}

// ErrPersonTokenRequired is the refusal of a service's own or a delegated token.
var ErrPersonTokenRequired = &Error{Code: "person_token_required", Message: "the client calls only with a person's own token"}

// Client calls the modules it is configured with.
type Client struct {
	Modules []Module
	// Versions are the manifest api_versions it supports; 1 by default.
	Versions []int
	HTTP     *http.Client

	mu     sync.Mutex
	loaded map[string]Module
}

// Loaded is what Load found.
type Loaded struct {
	// Descriptors are the ontologies, by module ID.
	Descriptors map[string]ontology.Descriptor
	// Unavailable are the modules that didn't answer.
	Unavailable []string
	// AutoApply are the loaded modules whose manifest lists auto_apply: a person
	// may let their assistant apply its actions, and it serves Done.
	AutoApply []string
}

// PersonToken reports whether a bearer token is a person's own, by its claims,
// read without verifying them (the module verifies): a token with an act or a
// grant claim is delegated, and one with no session is a service's own.
func PersonToken(token string) bool {
	parts := strings.Split(strings.TrimPrefix(token, "Bearer "), ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(raw, &claims) != nil {
		return false
	}
	_, act := claims["act"]
	_, grant := claims["grant"]
	var sub, sid string
	_ = json.Unmarshal(claims["sub"], &sub)
	_ = json.Unmarshal(claims["sid"], &sid)
	return !act && !grant && sub != "" && sid != ""
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) do(ctx context.Context, token string, m Module, method, path string, body any, headers map[string]string, out any) error {
	if !PersonToken(token) {
		return ErrPersonTokenRequired
	}
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(m.URL, "/")+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimPrefix(token, "Bearer "))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		var e struct {
			Error   string                `json:"error"`
			Message string                `json:"message"`
			Errors  []ontology.FieldError `json:"errors"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = "unavailable"
		}
		return &Error{Status: res.StatusCode, Code: e.Error, Message: e.Message, Errors: e.Errors}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Load reads each configured module's manifest and fetches the descriptor of
// each that lists ontology with a supported api_version. A module that doesn't
// answer is reported, not fatal.
func (c *Client) Load(ctx context.Context, token string) (Loaded, error) {
	if !PersonToken(token) {
		return Loaded{}, ErrPersonTokenRequired
	}
	versions := c.Versions
	if len(versions) == 0 {
		versions = []int{1}
	}
	out := Loaded{Descriptors: map[string]ontology.Descriptor{}, Unavailable: []string{}, AutoApply: []string{}}
	loaded := map[string]Module{}
	for _, m := range c.Modules {
		var manifest struct {
			APIVersion int      `json:"api_version"`
			Features   []string `json:"features"`
		}
		if err := c.do(ctx, token, m, "GET", "/v1/module", nil, nil, &manifest); err != nil {
			out.Unavailable = append(out.Unavailable, m.ID)
			continue
		}
		if !slices.Contains(manifest.Features, "ontology") || !slices.Contains(versions, manifest.APIVersion) {
			continue
		}
		var d ontology.Descriptor
		if err := c.do(ctx, token, m, "GET", "/v1/ontology", nil, nil, &d); err != nil {
			out.Unavailable = append(out.Unavailable, m.ID)
			continue
		}
		out.Descriptors[m.ID] = d
		loaded[m.ID] = m
		if slices.Contains(manifest.Features, "auto_apply") {
			out.AutoApply = append(out.AutoApply, m.ID)
		}
	}
	c.mu.Lock()
	c.loaded = loaded
	c.mu.Unlock()
	return out, nil
}

func (c *Client) module(id string) (Module, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.loaded[id]
	if !ok {
		return Module{}, &Error{Status: http.StatusNotFound, Code: "unknown_module", Message: "no loaded module " + id + " has an ontology"}
	}
	return m, nil
}

func (c *Client) byRef(ref string) (Module, string, string, error) {
	module, objectType, id, ok := ontology.SplitRef(ref)
	if !ok {
		return Module{}, "", "", &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: "not a reference"}
	}
	m, err := c.module(module)
	return m, objectType, id, err
}

// Describe is a module's descriptor for the person's tenant.
func (c *Client) Describe(ctx context.Context, token, module string) (ontology.Descriptor, error) {
	var out ontology.Descriptor
	m, err := c.module(module)
	if err != nil {
		return out, err
	}
	return out, c.do(ctx, token, m, "GET", "/v1/ontology", nil, nil, &out)
}

// Search is one page of a module's objects of a type. query is a search body.
func (c *Client) Search(ctx context.Context, token, module, objectType string, query any) (ontology.SearchAnswer, error) {
	var out ontology.SearchAnswer
	m, err := c.module(module)
	if err != nil {
		return out, err
	}
	if query == nil {
		query = map[string]any{}
	}
	return out, c.do(ctx, token, m, "POST", "/v1/objects/"+url.PathEscape(objectType)+"/search", query, nil, &out)
}

// Get is one object by reference, with a derived property's sources when
// sources names one.
func (c *Client) Get(ctx context.Context, token, ref, sources string) (ontology.ObjectAnswer, error) {
	var out ontology.ObjectAnswer
	m, objectType, id, err := c.byRef(ref)
	if err != nil {
		return out, err
	}
	path := "/v1/objects/" + url.PathEscape(objectType) + "/" + url.PathEscape(id)
	if sources != "" {
		path += "?sources=" + url.QueryEscape(sources)
	}
	return out, c.do(ctx, token, m, "GET", path, nil, nil, &out)
}

// Links is one page of an object's link.
func (c *Client) Links(ctx context.Context, token, ref, link string, limit int, cursor string) (ontology.SearchAnswer, error) {
	var out ontology.SearchAnswer
	m, objectType, id, err := c.byRef(ref)
	if err != nil {
		return out, err
	}
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	path := "/v1/objects/" + url.PathEscape(objectType) + "/" + url.PathEscape(id) + "/links/" + url.PathEscape(link)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return out, c.do(ctx, token, m, "GET", path, nil, nil, &out)
}

// Resolved is what Resolve found.
type Resolved struct {
	Objects []ontology.Resolved `json:"objects"`
	// Unresolved are the modules that are absent, have no ontology or didn't answer.
	Unresolved []string `json:"unresolved"`
}

// Resolve groups references by module, sends each group once to its module,
// and merges the answers in the order given. References the person can't see
// are left out.
func (c *Client) Resolve(ctx context.Context, token string, refs []string) (Resolved, error) {
	if !PersonToken(token) {
		return Resolved{}, ErrPersonTokenRequired
	}
	groups := map[string][]string{}
	var order []string
	for _, ref := range refs {
		module, _, _, ok := ontology.SplitRef(ref)
		if !ok {
			continue
		}
		if _, seen := groups[module]; !seen {
			order = append(order, module)
		}
		groups[module] = append(groups[module], ref)
	}
	found := map[string]ontology.Resolved{}
	out := Resolved{Objects: []ontology.Resolved{}, Unresolved: []string{}}
	for _, module := range order {
		m, err := c.module(module)
		if err != nil {
			out.Unresolved = append(out.Unresolved, module)
			continue
		}
		for start := 0; start < len(groups[module]); start += ontology.MaxLimit {
			batch := groups[module][start:min(start+ontology.MaxLimit, len(groups[module]))]
			var answer struct {
				Objects []ontology.Resolved `json:"objects"`
			}
			if err := c.do(ctx, token, m, "POST", "/v1/objects/resolve", map[string]any{"refs": batch}, nil, &answer); err != nil {
				out.Unresolved = append(out.Unresolved, module)
				break
			}
			for _, o := range answer.Objects {
				found[o.Ref] = o
			}
		}
	}
	for _, ref := range refs {
		if o, ok := found[ref]; ok {
			out.Objects = append(out.Objects, o)
			delete(found, ref)
		}
	}
	return out, nil
}

// Validation is what validate answers.
type Validation struct {
	Valid  bool                  `json:"valid"`
	Errors []ontology.FieldError `json:"errors"`
}

// Validate checks an action without changing anything.
func (c *Client) Validate(ctx context.Context, token, module, action string, req ontology.ActionRequest) (Validation, error) {
	var out Validation
	m, err := c.module(module)
	if err != nil {
		return out, err
	}
	return out, c.do(ctx, token, m, "POST", "/v1/actions/"+url.PathEscape(action)+"/validate", req, nil, &out)
}

// Apply runs an action. key is its Idempotency-Key, needed by an idempotent one.
func (c *Client) Apply(ctx context.Context, token, module, action string, req ontology.ActionRequest, key string) (ontology.ApplyAnswer, error) {
	var out ontology.ApplyAnswer
	m, err := c.module(module)
	if err != nil {
		return out, err
	}
	var headers map[string]string
	if key != "" {
		headers = map[string]string{"Idempotency-Key": key}
	}
	return out, c.do(ctx, token, m, "POST", "/v1/actions/"+url.PathEscape(action)+"/apply", req, headers, &out)
}

// Undo undoes an applied action by its undo ID.
func (c *Client) Undo(ctx context.Context, token, module, id string) (ontology.ApplyAnswer, error) {
	var out ontology.ApplyAnswer
	m, err := c.module(module)
	if err != nil {
		return out, err
	}
	return out, c.do(ctx, token, m, "POST", "/v1/undo/"+url.PathEscape(id), map[string]any{}, nil, &out)
}

// Undos are the person's applied actions in a module that can still be undone,
// newest first.
func (c *Client) Undos(ctx context.Context, token, module string) ([]ontology.UndoAnswer, error) {
	var out struct {
		Undo []ontology.UndoAnswer `json:"undo"`
	}
	m, err := c.module(module)
	if err != nil {
		return nil, err
	}
	err = c.do(ctx, token, m, "GET", "/v1/undo", nil, nil, &out)
	return out.Undo, err
}

// Done is what agents applied for the person in a module that lists
// auto_apply, newest first.
func (c *Client) Done(ctx context.Context, token, module string) ([]ontology.DoneAnswer, error) {
	var out struct {
		Done []ontology.DoneAnswer `json:"done"`
	}
	m, err := c.module(module)
	if err != nil {
		return nil, err
	}
	err = c.do(ctx, token, m, "GET", "/v1/done", nil, nil, &out)
	return out.Done, err
}

// Proposals are the person's open drafts in a module.
func (c *Client) Proposals(ctx context.Context, token, module string) ([]ontology.ProposalAnswer, error) {
	var out struct {
		Proposals []ontology.ProposalAnswer `json:"proposals"`
	}
	m, err := c.module(module)
	if err != nil {
		return nil, err
	}
	err = c.do(ctx, token, m, "GET", "/v1/proposals", nil, nil, &out)
	return out.Proposals, err
}

// Confirm applies a draft, with edited params replacing the draft's.
func (c *Client) Confirm(ctx context.Context, token, module, id string, body ontology.ConfirmRequest) (ontology.ApplyAnswer, error) {
	var out ontology.ApplyAnswer
	m, err := c.module(module)
	if err != nil {
		return out, err
	}
	return out, c.do(ctx, token, m, "POST", "/v1/proposals/"+url.PathEscape(id)+"/confirm", body, nil, &out)
}

// Dismiss closes a draft with no other effect.
func (c *Client) Dismiss(ctx context.Context, token, module, id string) error {
	m, err := c.module(module)
	if err != nil {
		return err
	}
	return c.do(ctx, token, m, "POST", "/v1/proposals/"+url.PathEscape(id)+"/dismiss", map[string]any{}, nil, nil)
}
