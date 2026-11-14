package server

import (
	"net/http"
	"testing"
)

func TestAnAssistantSuggestsASendThePersonMakes(t *testing.T) {
	h := newHarness(t)
	ts := h.teammateServer()
	h.want("eve", "PUT", "/v1/admin/agents/acc_lead/intake", map[string]any{"everyone": true}, http.StatusOK)

	// Ada's assistant creates the item and suggests Lead desk. It can't send.
	r := call(t, ts.URL, "agent-write", "work_create", map[string]any{"title": "Lead: Contoso", "suggest_to": "acc_lead", "suggest_note": "New lead"})
	if r.failed {
		t.Fatalf("create: %+v", r)
	}
	item, _ := r.data["id"].(string)
	suggestion, _ := r.data["suggestion"].(map[string]any)
	if item == "" || suggestion["to"] != "acc_lead" || suggestion["note"] != "New lead" {
		t.Fatalf("created: %+v", r.data)
	}
	if got := call(t, ts.URL, "agent-write", "work_send", map[string]any{"id": item, "to": "acc_lead", "note": "x"}); !got.failed {
		t.Fatal("an assistant sent work")
	}

	// Only an owner sees it.
	h.want("ada", "PUT", "/v1/work-items/"+item+"/collaborators/bob", nil, http.StatusNoContent)
	if got := h.want("bob", "GET", "/v1/work-items/"+item, nil, http.StatusOK); got["suggestion"] != nil {
		t.Errorf("a collaborator saw the suggestion: %+v", got["suggestion"])
	}
	if got := h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK); got["suggestion"] == nil {
		t.Error("the owner didn't see the suggestion")
	}

	// Ada sends it to someone else: the suggestion is answered either way, and
	// the item shows the offer waiting.
	h.want("ada", "POST", "/v1/work-items/"+item+"/send", map[string]any{"to": "dan", "mode": "share"}, http.StatusCreated)
	got := h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK)
	if got["suggestion"] != nil {
		t.Errorf("a send left the suggestion: %+v", got["suggestion"])
	}
	pending, _ := got["pending_offers"].([]any)
	if len(pending) != 1 || pending[0].(map[string]any)["to_user_id"] != "dan" {
		t.Errorf("pending offers: %+v", got["pending_offers"])
	}
}

func TestASuggestionIsCheckedLikeASend(t *testing.T) {
	h := newHarness(t)
	ts := h.teammateServer()
	item := h.item("ada")
	// acc_close takes work from nobody but its manager.
	if got := call(t, ts.URL, "agent-write", "work_suggest", map[string]any{"id": item, "to": "acc_close"}); !got.failed {
		t.Errorf("suggested a send the agent wouldn't take: %+v", got)
	}
	// A refused suggestion on create keeps the item, and says why.
	r := call(t, ts.URL, "agent-write", "work_create", map[string]any{"title": "Quote", "suggest_to": "acc_close"})
	if r.failed || r.data["suggestion_refused"] == nil || r.data["item"] == nil {
		t.Errorf("create with a refused suggestion: %+v", r)
	}
	// Ada dismisses one she doesn't want.
	if got := call(t, ts.URL, "agent-write", "work_suggest", map[string]any{"id": item, "to": "bob", "note": "Bob knows"}); got.failed {
		t.Fatalf("suggest: %+v", got)
	}
	h.want("ada", "DELETE", "/v1/work-items/"+item+"/suggestion", nil, http.StatusNoContent)
	if got := h.want("ada", "GET", "/v1/work-items/"+item, nil, http.StatusOK); got["suggestion"] != nil {
		t.Errorf("dismissed, still: %+v", got["suggestion"])
	}
	if code, _ := h.as("bob", "DELETE", "/v1/work-items/"+item+"/suggestion", nil); code != http.StatusNotFound {
		t.Errorf("bob dismissed on an item he can't see: %d", code)
	}
}

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
