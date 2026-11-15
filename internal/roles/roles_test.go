package roles

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xiant10/t10-modkit/people"
)

type org map[string]people.Member

func (o org) Member(_ context.Context, _, userID string) (people.Member, error) {
	m, ok := o[userID]
	if !ok {
		return people.Member{}, people.ErrNotFound
	}
	return m, nil
}

func token(context.Context) (string, error) { return "collab-token", nil }

// auth answers entitlements the way the auth service does, from users keyed by ID.
func auth(t *testing.T, status int, users map[string]map[string]any) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/v1/entitlements" || q.Get("module") != "collab" || r.Header.Get("Authorization") != "Bearer collab-token" {
			t.Errorf("unexpected request %s %s", r.URL, r.Header.Get("Authorization"))
		}
		w.WriteHeader(status)
		switch status {
		case http.StatusOK:
			u := users[q.Get("user")]
			_ = json.NewEncoder(w).Encode(map[string]any{"tenant_id": q.Get("tenant"), "module": "collab", "users": []any{u}})
		case http.StatusNotFound:
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown_module"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "server_error"})
		}
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestABusinessRoleOrTenantAdminAdministersCollab(t *testing.T) {
	url := auth(t, http.StatusOK, map[string]map[string]any{
		"ada":  {"user_id": "ada", "active": true, "grants": []any{map[string]string{"permission": "work:admin", "assignment_id": "asg_1"}}},
		"bob":  {"user_id": "bob", "active": true, "grants": []any{}},
		"eve":  {"user_id": "eve", "active": true, "tenant_admin": true, "grants": []any{}},
		"gone": {"user_id": "gone", "active": false, "grants": []any{map[string]string{"permission": "work:admin"}}},
		"odd":  {"user_id": "someone-else", "active": true, "tenant_admin": true},
	})
	c := New(url, token, org{})
	for user, want := range map[string]bool{"ada": true, "bob": false, "eve": true, "gone": false} {
		got, err := c.Administers(context.Background(), "ten_acme", user)
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v; want %v", user, got, err, want)
		}
	}
	if got, err := c.Administers(context.Background(), "ten_acme", "odd"); got || !errors.Is(err, people.ErrUnavailable) {
		t.Errorf("an answer for someone else: got %v, %v", got, err)
	}
}

func TestWithoutCollabInTheCatalogOnlyTenantAdmins(t *testing.T) {
	members := org{
		"eve": {UserID: "eve", Status: "active", UserStatus: "active", Role: "admin"},
		"bob": {UserID: "bob", Status: "active", UserStatus: "active", Role: "member"},
	}
	c := New(auth(t, http.StatusNotFound, nil), token, members)
	for user, want := range map[string]bool{"eve": true, "bob": false, "nobody": false} {
		got, err := c.Administers(context.Background(), "ten_acme", user)
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v; want %v", user, got, err, want)
		}
	}
}

func TestAFailedReadIsNeverAYes(t *testing.T) {
	c := New(auth(t, http.StatusInternalServerError, nil), token, org{})
	if got, err := c.Administers(context.Background(), "ten_acme", "eve"); got || !errors.Is(err, people.ErrUnavailable) {
		t.Fatalf("got %v, %v", got, err)
	}
}
