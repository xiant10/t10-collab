package docsmcp

import (
	"encoding/json"
	"testing"
)

func TestNegotiate(t *testing.T) {
	if got := Negotiate(json.RawMessage(`{"protocolVersion":"2025-03-26"}`)); got != "2025-03-26" {
		t.Errorf("a known version: %s", got)
	}
	if got := Negotiate(json.RawMessage(`{"protocolVersion":"1999-01-01"}`)); got != Versions[0] {
		t.Errorf("an unknown version: %s", got)
	}
	if got := Negotiate(nil); got != Versions[0] {
		t.Errorf("none: %s", got)
	}
}
