package httpjson

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestErrorBody(t *testing.T) {
	w := httptest.NewRecorder()
	Error(w, http.StatusForbidden, "scope_unavailable", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control %q", got)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"error":"scope_unavailable"}` {
		t.Fatalf("body %s", got)
	}
	w = httptest.NewRecorder()
	Error(w, http.StatusBadRequest, "invalid_request", "name is required")
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] != "invalid_request" || body["message"] != "name is required" {
		t.Fatalf("body %s", w.Body)
	}
}

func TestDecode(t *testing.T) {
	cases := []struct {
		body   string
		ok     bool
		status int
	}{
		{`{"name":"a"}`, true, 200},
		{`{"name":`, false, 400},
		{`{"name":"a"} {"name":"b"}`, false, 400},
		{`{"name":"` + strings.Repeat("x", 100) + `"}`, false, 413},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		var into struct{ Name string }
		r := httptest.NewRequest("POST", "/", strings.NewReader(c.body))
		r.Body = http.MaxBytesReader(w, r.Body, 64)
		ok := Decode(w, r, &into)
		if ok != c.ok || (!ok && w.Code != c.status) {
			t.Errorf("%.20q: ok=%v status=%d", c.body, ok, w.Code)
		}
	}
}
