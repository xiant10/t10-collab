package verifier

import "testing"

func TestAllowsNeedsTheTokenAndEveryLink(t *testing.T) {
	chain := []Link{{Actions: []string{"a:b", "c:d"}}, {Actions: []string{"a:b"}}}
	agent := &Identity{UserID: "usr_1", Scopes: []string{"a:b", "c:d"}, Agent: &Agent{Chain: chain}}
	if !agent.Allows("a:b") {
		t.Error("refused an action on the token and every link")
	}
	if agent.Allows("c:d") {
		t.Error("allowed an action a link had dropped")
	}
	if agent.Allows("e:f") {
		t.Error("allowed an action nobody holds")
	}
	narrow := &Identity{UserID: "usr_1", Scopes: []string{"c:d"}, Agent: &Agent{Chain: chain}}
	if narrow.Allows("a:b") {
		t.Error("allowed an action missing from the token")
	}
	if (&Identity{ServiceID: "svc_x"}).Allows("a:b") {
		t.Error("a service is not a person")
	}
}
