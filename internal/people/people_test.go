package people

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeAuth answers token requests, and passes every org read to org.
func fakeAuth(t *testing.T, org http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	var tokens atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/token", func(w http.ResponseWriter, _ *http.Request) {
		tokens.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"access_token": "svc", "expires_in": 300})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer svc" {
			t.Errorf("no service token on %s", r.URL)
		}
		org(w, r)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return New(Options{URL: ts.URL, ClientID: "t10-collab", ClientSecret: "s"}), &tokens
}

func TestReadsNameTheTenantAndReuseTheToken(t *testing.T) {
	c, tokens := fakeAuth(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tenant") != "ten_a" {
			t.Errorf("tenant = %q", r.URL.RawQuery)
		}
		if strings.HasSuffix(r.URL.Path, "/reports") {
			if r.URL.Query().Get("transitive") != "true" {
				t.Error("reports must read everyone beneath")
			}
			json.NewEncoder(w).Encode(map[string]any{"user_ids": []string{"b"}})
			return
		}
		json.NewEncoder(w).Encode(Member{UserID: "a", Status: "active", UserStatus: "active"})
	})
	ctx := context.Background()
	if m, err := c.Member(ctx, "ten_a", "a"); err != nil || !m.Active() {
		t.Fatalf("member = %+v, %v", m, err)
	}
	if ids, err := c.Reports(ctx, "ten_a", "a"); err != nil || len(ids) != 1 {
		t.Fatalf("reports = %v, %v", ids, err)
	}
	if tokens.Load() != 1 {
		t.Errorf("minted %d service tokens", tokens.Load())
	}
}

func TestAnIDCannotChangeTheURL(t *testing.T) {
	var calls atomic.Int32
	c, _ := fakeAuth(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	for _, id := range []string{"usr_1?tenant=ten_other", "../members", "usr 1", "a/b", "a%2Fb", ""} {
		if _, err := c.Member(context.Background(), "ten_a", id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: %v", id, err)
		}
		if _, err := c.Reports(context.Background(), "ten_a", id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: %v", id, err)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("%d reads reached the auth service", calls.Load())
	}
}

func TestFailuresAreNeverEmptyAnswers(t *testing.T) {
	status := http.StatusInternalServerError
	body := ""
	c, _ := fakeAuth(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	})
	ctx := context.Background()
	if _, err := c.Reports(ctx, "ten_a", "a"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("500: %v", err)
	}
	status, body = http.StatusOK, "not json"
	if _, err := c.Reports(ctx, "ten_a", "a"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("bad body: %v", err)
	}
	status = http.StatusNotFound
	if _, err := c.Member(ctx, "ten_a", "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: %v", err)
	}
	broken := New(Options{URL: "http://127.0.0.1:1"})
	if _, err := broken.Member(ctx, "ten_a", "a"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("no service: %v", err)
	}
}

func TestCachedReportsReuseAFreshAnswerAndNeverAFailedOne(t *testing.T) {
	var calls atomic.Int32
	var failing atomic.Bool
	c, _ := fakeAuth(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.URL.Query().Get("transitive") == "true" {
			json.NewEncoder(w).Encode(map[string]any{"user_ids": []string{"a", "b"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"user_ids": []string{"a"}})
	})
	ctx := context.Background()
	for range 3 {
		if ids, err := c.CachedReports(ctx, "ten_a", "m", true); err != nil || len(ids) != 2 {
			t.Fatalf("all = %v, %v", ids, err)
		}
	}
	if ids, err := c.CachedReports(ctx, "ten_a", "m", false); err != nil || len(ids) != 1 {
		t.Fatalf("a direct read shared the transitive answer: %v, %v", ids, err)
	}
	if calls.Load() != 2 {
		t.Errorf("made %d reads, want 2", calls.Load())
	}

	// A send must never see the cache.
	failing.Store(true)
	if _, err := c.Reports(ctx, "ten_a", "m"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a fresh read used the cache: %v", err)
	}
	if _, err := c.CachedReports(ctx, "ten_b", "m", true); !errors.Is(err, ErrUnavailable) {
		t.Errorf("tenants shared a cache entry: %v", err)
	}
}

func TestUnitMembersTellEmptyFromDeleted(t *testing.T) {
	status := http.StatusOK
	c, _ := fakeAuth(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(`{"user_ids":[]}`))
	})
	ctx := context.Background()
	if ids, err := c.UnitMembers(ctx, "ten_a", "org_1"); err != nil || ids == nil || len(ids) != 0 {
		t.Fatalf("empty unit = %#v, %v", ids, err)
	}
	status = http.StatusNotFound
	if _, err := c.UnitMembers(ctx, "ten_a", "org_1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted unit: %v", err)
	}
	status = http.StatusInternalServerError
	if _, err := c.UnitMembers(ctx, "ten_a", "org_1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("failed read: %v", err)
	}
	if _, err := c.UnitMembers(ctx, "ten_a", "../x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bad id: %v", err)
	}
}

func TestMembersReadsEveryPageWithInactiveMembers(t *testing.T) {
	c, _ := fakeAuth(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Has("status") {
			t.Error("the read must include inactive members")
		}
		if q.Get("cursor") == "" {
			json.NewEncoder(w).Encode(map[string]any{"members": []Member{{UserID: "a", Status: "inactive"}}, "next_cursor": "a"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"members": []Member{{UserID: "b", Status: "active", UserStatus: "suspended"}}})
	})
	got, err := c.Members(context.Background(), "ten_a")
	if err != nil || len(got) != 2 || !got[0].Departed() || got[1].Departed() || !got[1].Suspended() {
		t.Fatalf("members = %+v, %v", got, err)
	}
}

func TestMembersFailsOnAnyFailedPage(t *testing.T) {
	c, _ := fakeAuth(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			json.NewEncoder(w).Encode(map[string]any{"members": []Member{{UserID: "a"}}, "next_cursor": "a"})
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	if got, err := c.Members(context.Background(), "ten_a"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("members = %+v, %v", got, err)
	}
}
