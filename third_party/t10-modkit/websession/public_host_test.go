package websession

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSignInStartsOnThePublicHostName(t *testing.T) {
	m := &Manager{opts: Options{PublicURL: "http://127.0.0.1:8083"}}
	for _, c := range []struct{ host, want string }{
		{"localhost:8083", "http://127.0.0.1:8083/auth/login?next=%2Ft%2Fa%2F"},
		{"127.0.0.1:8083", ""},
		{"127.0.0.1:5173", ""}, // a dev proxy: cookies ignore ports
	} {
		r := httptest.NewRequest("GET", "/auth/login?next=%2Ft%2Fa%2F", nil)
		r.Host = c.host
		w := httptest.NewRecorder()
		moved := m.OnPublicHost(w, r)
		if moved != (c.want != "") || (moved && (w.Code != http.StatusFound || w.Header().Get("Location") != c.want)) {
			t.Errorf("%s: moved=%v %d %q", c.host, moved, w.Code, w.Header().Get("Location"))
		}
	}
}
