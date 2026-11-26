package websession

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSealerKeepsPurposes(t *testing.T) {
	s, err := NewSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed := s.Seal([]byte("token"), "crm")
	if plain, err := s.Open(sealed, "crm"); err != nil || string(plain) != "token" {
		t.Fatalf("open: %q %v", plain, err)
	}
	if _, err := s.Open(sealed, "sales"); err == nil {
		t.Fatal("a value opened for another purpose")
	}
	if _, err := NewSealer([]byte("short")); err == nil {
		t.Fatal("a short key")
	}
}

func TestTokenErrorKeepsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()
	_, err := (&AuthClient{URL: srv.URL, HTTP: srv.Client()}).Redeem(context.Background(), "c", "r", "v", "")
	var refused *TokenError
	if !errors.As(err, &refused) || refused.Status != 400 || refused.Code != "invalid_grant" || !errors.Is(err, ErrSignedOut) {
		t.Fatalf("refusal: %#v", err)
	}
}
