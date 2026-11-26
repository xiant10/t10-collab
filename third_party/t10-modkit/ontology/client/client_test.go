package client_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiant10/t10-modkit/ontology"
	"github.com/xiant10/t10-modkit/ontology/client"
	"github.com/xiant10/t10-modkit/ontology/testdata/ledgermod"
)

// The fixture both clients are held to: recorded module answers, and the
// result each operation must give. ONTOLOGY_RECORD=1 records it again from
// ledgermod; web/ontology.test.ts replays the same file.
const fixturePath = "../testdata/client/fixture.json"

type exchange struct {
	Module   string          `json:"module"`
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Body     string          `json:"body,omitempty"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
}

type fixtureCase struct {
	Name   string          `json:"name"`
	Op     string          `json:"op"`
	Token  string          `json:"token"`
	Args   json.RawMessage `json:"args"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *caseError      `json:"error,omitempty"`
}

type caseError struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
}

type fixture struct {
	Modules   []fixtureModule   `json:"modules"`
	Tokens    map[string]string `json:"tokens"`
	Exchanges []exchange        `json:"exchanges"`
	Cases     []fixtureCase     `json:"cases"`
}

type fixtureModule struct {
	ID   string `json:"id"`
	Down bool   `json:"down,omitempty"`
}

func jwt(claims map[string]any) string {
	enc := func(v any) string { raw, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(raw) }
	return enc(map[string]string{"alg": "ES256", "typ": "JWT"}) + "." + enc(claims) + ".c2ln"
}

var tokens = map[string]string{
	"person":    jwt(map[string]any{"sub": "rep1", "tid": "t1", "sid": "s1", "aud": "t10"}),
	"service":   jwt(map[string]any{"sub": "svc_reports", "aud": "t10"}),
	"delegated": jwt(map[string]any{"sub": "rep1", "tid": "t1", "act": map[string]string{"sub": "agent-ti"}, "grant": "grt_1"}),
}

// canonical is a JSON body with sorted keys, so two clients' bodies match.
func canonical(raw []byte) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	out, _ := json.Marshal(v) // encoding/json sorts map keys
	return string(out)
}

// replayServer answers each request with the next recorded answer to it.
func replayServer(t *testing.T, f fixture) *httptest.Server {
	var mu sync.Mutex
	used := make([]bool, len(f.Exchanges))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		module, path, _ := strings.Cut(strings.TrimPrefix(r.URL.RequestURI(), "/m/"), "/")
		path = "/" + path
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		for i, e := range f.Exchanges {
			if !used[i] && e.Module == module && e.Method == r.Method && e.Path == path && e.Body == canonical(body) {
				used[i] = true
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(e.Status)
				w.Write(e.Response)
				return
			}
		}
		t.Errorf("no recorded answer to %s %s %s %s", module, r.Method, path, canonical(body))
		http.Error(w, `{"error":"unrecorded"}`, 500)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func modules(f fixture, base string) []client.Module {
	var out []client.Module
	for _, m := range f.Modules {
		u := base + "/m/" + m.ID
		if m.Down {
			u = "http://127.0.0.1:1"
		}
		out = append(out, client.Module{ID: m.ID, URL: u})
	}
	return out
}

// run performs one case's operation and answers its result as JSON.
func run(ctx context.Context, c *client.Client, token, op string, rawArgs json.RawMessage) (any, error) {
	var a struct {
		Module, Type, Ref, Sources, Link, Cursor, Action, Key, ID string
		Limit                                                     int
		Query                                                     json.RawMessage
		Refs                                                      []string
		Request                                                   ontology.ActionRequest
		Body                                                      ontology.ConfirmRequest
	}
	_ = json.Unmarshal(rawArgs, &a)
	switch op {
	case "load":
		l, err := c.Load(ctx, token)
		ids := []string{}
		for id := range l.Descriptors {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return map[string]any{"ontologies": ids, "unavailable": l.Unavailable}, err
	case "describe":
		return c.Describe(ctx, token, a.Module)
	case "search":
		var q any
		_ = json.Unmarshal(a.Query, &q)
		return c.Search(ctx, token, a.Module, a.Type, q)
	case "get":
		return c.Get(ctx, token, a.Ref, a.Sources)
	case "links":
		return c.Links(ctx, token, a.Ref, a.Link, a.Limit, a.Cursor)
	case "resolve":
		return c.Resolve(ctx, token, a.Refs)
	case "validate":
		return c.Validate(ctx, token, a.Module, a.Action, a.Request)
	case "apply":
		return c.Apply(ctx, token, a.Module, a.Action, a.Request, a.Key)
	case "undo":
		return c.Undo(ctx, token, a.Module, a.ID)
	case "proposals":
		ps, err := c.Proposals(ctx, token, a.Module)
		return map[string]any{"proposals": ps}, err
	case "confirm":
		return c.Confirm(ctx, token, a.Module, a.ID, a.Body)
	case "dismiss":
		return map[string]any{}, c.Dismiss(ctx, token, a.Module, a.ID)
	}
	return nil, errors.New("unknown op " + op)
}

func normal(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestRecordedAnswers(t *testing.T) {
	if os.Getenv("ONTOLOGY_RECORD") != "" {
		record(t)
	}
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	srv := replayServer(t, f)
	c := &client.Client{Modules: modules(f, srv.URL)}
	for _, tc := range f.Cases {
		got, err := run(context.Background(), c, f.Tokens[tc.Token], tc.Op, tc.Args)
		if tc.Error != nil {
			var e *client.Error
			if !errors.As(err, &e) || e.Status != tc.Error.Status || e.Code != tc.Error.Code {
				t.Errorf("%s: want error %d %s, got %v", tc.Name, tc.Error.Status, tc.Error.Code, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.Name, err)
			continue
		}
		var want any
		_ = json.Unmarshal(tc.Result, &want)
		if !reflect.DeepEqual(normal(got), want) {
			g, _ := json.Marshal(normal(got))
			t.Errorf("%s:\n got  %s\n want %s", tc.Name, g, tc.Result)
		}
	}
}

// The client-spec scenarios, against the recorded answers.
func TestClientSpec(t *testing.T) {
	raw, _ := os.ReadFile(fixturePath)
	var f fixture
	_ = json.Unmarshal(raw, &f)
	byName := map[string]fixtureCase{}
	for _, tc := range f.Cases {
		byName[tc.Name] = tc
	}
	var load struct{ Ontologies, Unavailable []string }
	_ = json.Unmarshal(byName["load"].Result, &load)
	// Two modules, one with an ontology; a newer major version; one down.
	if !reflect.DeepEqual(load.Ontologies, []string{"sales"}) || !reflect.DeepEqual(load.Unavailable, []string{"down"}) {
		t.Fatalf("load: %+v", load)
	}
	var resolved client.Resolved
	_ = json.Unmarshal(byName["resolve a mixed list"].Result, &resolved)
	if len(resolved.Objects) != 1 || !reflect.DeepEqual(resolved.Unresolved, []string{"collab", "crm"}) {
		t.Fatalf("resolve: %+v", resolved)
	}
	for name, want := range map[string]caseError{
		"a stale revision": {409, "stale_revision"}, "a service's own token": {0, "person_token_required"},
		"a delegated token": {0, "person_token_required"}, "a hidden deal": {404, "not_found"},
	} {
		if e := byName[name].Error; e == nil || *e != want {
			t.Errorf("%s: %+v", name, e)
		}
	}
	if client.PersonToken(tokens["service"]) || client.PersonToken(tokens["delegated"]) || !client.PersonToken(tokens["person"]) || client.PersonToken("opaque") {
		t.Fatal("PersonToken")
	}
}

// record runs the cases against ledgermod behind a recording proxy and writes
// the fixture.
func record(t *testing.T) {
	now := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	m := ledgermod.New(func() time.Time { return now })
	m.SetPack("t1", ledgermod.Pack{Version: 3, Stages: []string{"线索", "商机", "在跟"}, Roles: []string{"决策人"}, Kinds: []string{"拜访"},
		Fields: []ledgermod.Field{{Key: "F", Name: "Family", NameZh: "家庭", Sensitive: true}}})
	rep := ledgermod.Rep("t1", "rep1")
	account := m.AddAccount(rep, "Example Works")
	deal := m.AddDeal(rep, account, "Line 2 upgrade", 120_000_00, "")
	m.AddDeal(rep, account, "Spare parts", 8_000_00, "")
	m.Link(deal, m.AddStakeholder(rep, account, "Wang", "13800000000", nil), "决策人")
	other := m.AddDeal(ledgermod.Rep("t1", "rep2"), m.AddAccount(ledgermod.Rep("t1", "rep2"), "Rival"), "Hidden", 1, "")

	mux := http.NewServeMux()
	ontology.Mount(mux, m.Registry.Routes())
	manifests := map[string]string{
		"sales":  `{"id":"sales","api_version":1,"action_prefix":"sales","enabled":true,"features":["ontology"]}`,
		"collab": `{"id":"collab","api_version":1,"action_prefix":"work","enabled":true,"features":["attention"]}`,
		"future": `{"id":"future","api_version":2,"action_prefix":"future","enabled":true,"features":["ontology"]}`,
	}
	var f fixture
	f.Tokens = tokens
	f.Modules = []fixtureModule{{ID: "sales"}, {ID: "collab"}, {ID: "future"}, {ID: "down", Down: true}}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		module, path, _ := strings.Cut(strings.TrimPrefix(r.URL.RequestURI(), "/m/"), "/")
		path = "/" + path
		body, _ := io.ReadAll(r.Body)
		rec := httptest.NewRecorder()
		switch {
		case path == "/v1/module":
			rec.Header().Set("Content-Type", "application/json")
			rec.WriteString(manifests[module])
		case module == "sales":
			// ledgermod's test tokens stand in for the person's verified JWT.
			req := r.Clone(r.Context())
			req.URL.Path, req.RequestURI = strings.SplitN(path, "?", 2)[0], ""
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer person:t1:rep1")
			mux.ServeHTTP(rec, req)
		default:
			rec.WriteHeader(404)
			rec.WriteString(`{"error":"not_found"}`)
		}
		f.Exchanges = append(f.Exchanges, exchange{Module: module, Method: r.Method, Path: path, Body: canonical(body), Status: rec.Code, Response: json.RawMessage(bytes.TrimSpace(rec.Body.Bytes()))})
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}))
	defer proxy.Close()

	c := &client.Client{Modules: modules(f, proxy.URL)}
	ctx := context.Background()
	dealRef := "sales:deal:" + deal
	add := func(name, op, token string, args map[string]any) map[string]any {
		raw, _ := json.Marshal(args)
		got, err := run(ctx, c, tokens[token], op, raw)
		tc := fixtureCase{Name: name, Op: op, Token: token, Args: raw}
		var e *client.Error
		if errors.As(err, &e) {
			tc.Error = &caseError{Status: e.Status, Code: e.Code}
		} else if err != nil {
			t.Fatalf("%s: %v", name, err)
		} else {
			tc.Result, _ = json.Marshal(normal(got))
		}
		f.Cases = append(f.Cases, tc)
		out, _ := normal(got).(map[string]any)
		return out
	}
	add("load", "load", "person", map[string]any{})
	add("describe", "describe", "person", map[string]any{"module": "sales"})
	add("search a page", "search", "person", map[string]any{"module": "sales", "type": "deal", "query": map[string]any{"limit": 1, "sort": map[string]any{"property": "amount", "desc": true}}})
	add("get a deal", "get", "person", map[string]any{"ref": dealRef})
	add("a hidden deal", "get", "person", map[string]any{"ref": "sales:deal:" + other})
	add("follow a link", "links", "person", map[string]any{"ref": dealRef, "link": "stakeholders", "limit": 10})
	add("resolve a mixed list", "resolve", "person", map[string]any{"refs": []string{dealRef, "sales:deal:" + other, "collab:work_item:b", "crm:deal:c"}})
	add("validate", "validate", "person", map[string]any{"module": "sales", "action": "ticket.send", "request": map[string]any{"object": dealRef, "params": map[string]any{}}})
	add("a stale revision", "apply", "person", map[string]any{"module": "sales", "action": "ticket.send", "request": map[string]any{"object": dealRef, "revision": 99, "params": map[string]any{"title": "x"}}})
	applied := add("apply", "apply", "person", map[string]any{"module": "sales", "action": "record.add", "key": "fixture-0001",
		"request": map[string]any{"object": dealRef, "revision": 1, "params": map[string]any{"kind": "拜访", "summary": "met the plant manager"}}})
	add("sources", "get", "person", map[string]any{"ref": dealRef, "sources": "score"})
	add("undo", "undo", "person", map[string]any{"module": "sales", "id": applied["undo"].(map[string]any)["id"]})
	// An agent drafts two tickets for the rep, through the tools.
	var drafts []string
	for _, title := range []string{"Quote", "Visit"} {
		for _, tool := range m.Registry.Tools() {
			if tool.Name == "sales_propose" {
				out := tool.Run(ctx, rep, "agent:fixture", json.RawMessage(`{"action":"ticket.send","object":"`+dealRef+`","params":{"title":"`+title+`"}}`))
				text := out["content"].([]map[string]any)[0]["text"].(string)
				text = strings.TrimSuffix(strings.TrimPrefix(text, "<module_record module=\"sales\">\n"), "\n</module_record>")
				var d map[string]any
				_ = json.Unmarshal([]byte(text), &d)
				drafts = append(drafts, d["proposal"].(string))
			}
		}
		now = now.Add(time.Minute)
	}
	add("proposals", "proposals", "person", map[string]any{"module": "sales"})
	add("confirm with an edit", "confirm", "person", map[string]any{"module": "sales", "id": drafts[0], "body": map[string]any{"params": map[string]any{"title": "Quote, revised"}}})
	add("dismiss", "dismiss", "person", map[string]any{"module": "sales", "id": drafts[1]})
	add("a service's own token", "get", "service", map[string]any{"ref": dealRef})
	add("a delegated token", "apply", "delegated", map[string]any{"module": "sales", "action": "record.add", "request": map[string]any{}})
	add("a module that isn't loaded", "get", "person", map[string]any{"ref": "crm:deal:c"})

	out, _ := json.MarshalIndent(f, "", "  ")
	if err := os.WriteFile(fixturePath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
