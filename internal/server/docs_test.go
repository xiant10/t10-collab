package server

import (
	"slices"
	"testing"
)

// The image carries its docs, so one with broken front matter must fail here,
// not at startup.
func TestDocsServe(t *testing.T) {
	h, err := Docs("test")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range h.Docs() {
		ids = append(ids, d.ID)
	}
	for _, want := range []string{"api", "types"} {
		if !slices.Contains(ids, want) {
			t.Errorf("%s isn't served: %v", want, ids)
		}
	}
	for _, internal := range []string{"design", "development"} {
		if slices.Contains(ids, internal) {
			t.Errorf("%s is for contributors, but it's served", internal)
		}
	}
}
