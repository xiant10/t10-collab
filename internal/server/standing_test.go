package server

import (
	"net/http"
	"testing"
)

func TestAnItemShowsTheAgentWorkingOnIt(t *testing.T) {
	h := newHarness(t)
	ts := h.teammateServer()
	h.want("eve", "PUT", "/v1/admin/agents/acc_lead/intake", map[string]any{"everyone": true}, http.StatusOK)
	item := h.item("ada")
	offer := h.want("ada", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "acc_lead"}, http.StatusCreated)
	working := func() []any {
		t.Helper()
		got, _ := h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK)["working"].([]any)
		return got
	}
	// Told of the offer, and hasn't answered.
	if got := working(); len(got) != 1 || got[0].(map[string]any)["user_id"] != "acc_lead" {
		t.Fatalf("after the offer: %+v", got)
	}
	call(t, ts.URL, "teammate:acc_lead", "work_accept", map[string]any{"offer_id": offer["id"]})
	if got := working(); len(got) != 0 {
		t.Errorf("after it accepted: %+v", got)
	}
	// Ada comments, so it's on it again until it answers.
	h.want("ada", "POST", "/v1/work-items/"+item+"/comments", map[string]any{"body": "Partner tier"}, http.StatusCreated)
	if got := working(); len(got) != 1 {
		t.Errorf("after a comment: %+v", got)
	}
	call(t, ts.URL, "teammate:acc_lead", "work_comment", map[string]any{"id": item, "body": "Thanks"})
	if got := working(); len(got) != 0 {
		t.Errorf("after it answered: %+v", got)
	}
}

func TestAnItemSaysWhoMentionedYouUntilYouAnswer(t *testing.T) {
	h := newHarness(t)
	item := h.item("ada")
	h.want("ada", "PUT", "/v1/work-items/"+item+"/collaborators/bob", nil, http.StatusNoContent)
	h.want("bob", "POST", "/v1/work-items/"+item+"/comments", map[string]any{"body": "@Ada which tier?", "mentions": []string{"ada"}}, http.StatusCreated)
	mention, _ := h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK)["mention"].(map[string]any)
	if mention["from"] != "bob" || mention["body"] != "@Ada which tier?" {
		t.Fatalf("mention: %+v", mention)
	}
	if got := h.want("bob", "GET", "/v1/work-items/"+item, nil, http.StatusOK)["mention"]; got != nil {
		t.Errorf("bob sees his own mention: %+v", got)
	}
	h.want("ada", "POST", "/v1/work-items/"+item+"/comments", map[string]any{"body": "Partner"}, http.StatusCreated)
	if got := h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK)["mention"]; got != nil {
		t.Errorf("answered, still: %+v", got)
	}
}
