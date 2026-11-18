package types

import (
	"errors"
	"slices"
	"testing"
)

func TestFieldsKeepTheirDeclaredOrder(t *testing.T) {
	d, err := Parse([]byte(`{"id": "crm.ask", "owner_module": "crm", "version": 1, "label": "Ask",
		"fields": {"properties": {"when": {"type": "string"}, "what": {"type": "string"}, "for_whom": {"type": "string"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.Fields.Order, []string{"when", "what", "for_whom"}) {
		t.Errorf("order = %v", d.Fields.Order)
	}
	if d.Visibility != VisibilityTeam || d.Departure != DepartureReassign {
		t.Errorf("defaults = %s, %s", d.Visibility, d.Departure)
	}
}

func TestParseRefusesWhatIsntSupported(t *testing.T) {
	for _, raw := range []string{
		`{"id": "crm.ask", "owner_module": "crm", "version": 1, "label": "Ask", "hooks": {}}`,
		`{"id": "crm.ask", "owner_module": "crm", "version": 1, "label": "Ask", "fields": {"properties": {"a": {"type": "string", "format": "email"}}}}`,
		`{"id": "crm.ask", "owner_module": "crm", "version": 1, "label": "Ask", "fields": {"properties": {"a": {"type": "string"}, "a": {"type": "string"}}}}`,
		`{"id": "crm.ask", "owner_module": "crm", "version": 1, "label": "Ask", "fields": {"properties": {"a": {"type": "string"}}, "order": ["b"]}}`,
		`{"id": "crm.ask", "owner_module": "crm", "version": 1, "label": "Ask", "fields": {"properties": {"n": {"type": "integer", "enum": [1.5]}}}}`,
	} {
		if _, err := Parse([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func TestSubjectRefs(t *testing.T) {
	typed := Type{Definition: Definition{ID: "crm.ask", Subject: []string{"crm:deal"}}}
	for ref, ok := range map[string]bool{
		"":                true,
		"crm:deal:d_12":   true,
		"crm:deal:d-12.3": true,
		"crm:lead:l_1":    false,
		"crm:deal":        false,
		"crm:deal:":       false,
		"CRM:deal:d_1":    false,
		"crm:deal:d 1":    false,
		"crm:deal:a:b":    false,
	} {
		if err := typed.CheckSubject(ref); (err == nil) != ok {
			t.Errorf("%q: %v", ref, err)
		}
	}
	if err := TaskType.CheckSubject("crm:lead:l_1"); err != nil {
		t.Errorf("a task can be about any record: %v", err)
	}
}
