package types

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// Phases are collab's four statuses. Every state maps to one, so Home, filters,
// attention counts and the sweep work on any type without knowing it.
var Phases = []string{"todo", "doing", "done", "cancelled"}

// Relations a move can be taken by: the ones policy already computes.
const (
	ByCreator      = "creator"
	ByOwner        = "owner"
	ByCollaborator = "collaborator"
	ByRecipient    = "recipient"
	ByListMember   = "list_member"
)

// Payload keys a move can carry, stored on its event as its result.
const (
	PayloadReply = "reply"
	PayloadLinks = "links"
)

// NoteRequired is a move's note setting when it must have one.
const NoteRequired = "required"

// Offer events a type can map to states. A share send never moves the state.
const (
	OfferSend     = "send"
	OfferAssign   = "assign"
	OfferAccept   = "accept"
	OfferDecline  = "decline"
	OfferWithdraw = "withdraw"
)

const maxTransitions = 50

// State is one state of a type, and the phase it counts as.
type State struct {
	Phase string `json:"phase"`
	Label string `json:"label"`
}

// OfferMove is what one transfer offer event does to an item's state.
type OfferMove struct {
	// From limits the states the event moves an item from. For accept, decline
	// and withdraw it defaults to the state send moves to, so an answer moves
	// only an item still waiting on it.
	From     []string `json:"from,omitempty"`
	To       string   `json:"to"`
	Requires []string `json:"requires,omitempty"`
	// Note is required for decline when set to "required".
	Note string `json:"note,omitempty"`
}

// Offers maps offer events to states. Missing ones leave the state alone.
type Offers struct {
	Send     *OfferMove `json:"send,omitempty"`
	Assign   *OfferMove `json:"assign,omitempty"`
	Accept   *OfferMove `json:"accept,omitempty"`
	Decline  *OfferMove `json:"decline,omitempty"`
	Withdraw *OfferMove `json:"withdraw,omitempty"`
}

// Relations is one relation or several. A type may write `by: owner`.
type Relations []string

// UnmarshalJSON takes a string or a list.
func (r *Relations) UnmarshalJSON(data []byte) error {
	var one string
	if json.Unmarshal(data, &one) == nil {
		*r = Relations{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*r = many
	return nil
}

// Transition is a move that isn't an offer event.
type Transition struct {
	ID    string   `json:"id"`
	Label string   `json:"label,omitempty"`
	From  []string `json:"from"`
	To    string   `json:"to"`
	// By is who may take it: any one of these relations.
	By Relations `json:"by"`
	// Requires names fields that must be filled, or payload keys that must be
	// given, for the move.
	Requires []string `json:"requires,omitempty"`
	// Payload is the result keys the move may carry.
	Payload []string `json:"payload,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// HasStates reports whether the type has a lifecycle of its own.
func (t Type) HasStates() bool { return len(t.States) > 0 }

// Phase is a state's phase, or empty for a state the type no longer declares.
func (t Type) Phase(state string) string { return t.States[state].Phase }

// Offer returns the mapping for an offer event, or nil.
func (o *Offers) Offer(event string) *OfferMove {
	if o == nil {
		return nil
	}
	return map[string]*OfferMove{
		OfferSend: o.Send, OfferAssign: o.Assign, OfferAccept: o.Accept, OfferDecline: o.Decline, OfferWithdraw: o.Withdraw,
	}[event]
}

// Applies reports whether an offer event moves an item in state.
func (t Type) Applies(event, state string) (*OfferMove, bool) {
	m := t.Offers.Offer(event)
	if m == nil {
		return nil, false
	}
	from := m.From
	if len(from) == 0 && event != OfferSend && event != OfferAssign {
		from = []string{t.Offers.Send.To}
	}
	return m, len(from) == 0 || slices.Contains(from, state)
}

// Transition returns a transition by ID.
func (t Type) Transition(id string) (Transition, bool) {
	i := slices.IndexFunc(t.Transitions, func(tr Transition) bool { return tr.ID == id })
	if i < 0 {
		return Transition{}, false
	}
	return t.Transitions[i], true
}

// Allows reports whether a transition can be taken from state. An item left in
// a state a later version removed can only be cancelled.
func (t Type) Allows(tr Transition, state string) bool {
	if _, known := t.States[state]; !known {
		return t.Phase(tr.To) == "cancelled"
	}
	return slices.Contains(tr.From, state)
}

// Missing returns the required fields that are empty, and the required payload
// keys left out.
func Missing(requires []string, fields map[string]any, payload map[string]any) []string {
	missing := []string{}
	for _, key := range requires {
		value, ok := fields[key]
		if key == PayloadReply || key == PayloadLinks {
			value, ok = payload[key]
		}
		if !ok || value == nil || value == "" {
			missing = append(missing, key)
		} else if list, isList := value.([]any); isList && len(list) == 0 {
			missing = append(missing, key)
		}
	}
	return missing
}

// orderedKeys returns an object's keys in the order they're written.
func orderedKeys(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, invalid("expected an object")
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key := tok.(string)
		if slices.Contains(keys, key) {
			return nil, invalid("%s is declared twice", key)
		}
		keys = append(keys, key)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// UnmarshalJSON reads a definition strictly, keeping the order its states
// were declared in.
func (d *Definition) UnmarshalJSON(data []byte) error {
	type plain Definition
	var p plain
	if err := strictly(data, &p); err != nil {
		return err
	}
	*d = Definition(p)
	if d.StateOrder == nil && len(d.States) > 0 {
		var raw struct {
			States json.RawMessage `json:"states"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		order, err := orderedKeys(raw.States)
		if err != nil {
			return err
		}
		d.StateOrder = order
	}
	return nil
}

func (d *Definition) validateStates() error {
	if len(d.States) == 0 {
		if d.Initial != "" || d.Cancelled != "" || d.Offers != nil || len(d.Transitions) > 0 || len(d.StateOrder) > 0 {
			return invalid("initial, cancelled, offers and transitions need states")
		}
		return nil
	}
	if len(d.States) > maxFields {
		return invalid("at most %d states", maxFields)
	}
	if d.StateOrder == nil {
		d.StateOrder = slices.Sorted(maps.Keys(d.States))
	}
	order := slices.Clone(d.StateOrder)
	slices.Sort(order)
	if !slices.Equal(order, slices.Sorted(maps.Keys(d.States))) {
		return invalid("state_order must name each state once")
	}
	for name, st := range d.States {
		if !fieldKey.MatchString(name) {
			return invalid("state %q must be lowercase letters, digits and _", name)
		}
		if !slices.Contains(Phases, st.Phase) {
			return invalid("state %s: phase must be todo, doing, done or cancelled", name)
		}
		if !validLabel(st.Label) {
			return invalid("state %s: label must be 1 to %d characters", name, maxLabel)
		}
	}
	if p := d.States[d.Initial].Phase; p != "todo" && p != "doing" {
		return invalid("initial must be a todo or doing state")
	}
	if d.Cancelled != "" && d.States[d.Cancelled].Phase != "cancelled" {
		return invalid("cancelled must be a cancelled state")
	}
	if d.Cancelled == "" && d.Departure != DepartureReassign {
		return invalid("a type that cancels items when owners leave must name its cancelled state")
	}
	if err := d.validateOffers(); err != nil {
		return err
	}
	return d.validateTransitions()
}

func (d *Definition) states(names []string, what string) error {
	for _, name := range names {
		if _, ok := d.States[name]; !ok {
			return invalid("%s names %q, which isn't a state", what, name)
		}
	}
	return nil
}

func (d *Definition) fieldsOnly(names []string, what string, payload []string) error {
	for _, name := range names {
		if _, ok := d.Fields.Properties[name]; !ok && !slices.Contains(payload, name) {
			return invalid("%s requires %q, which isn't a field", what, name)
		}
	}
	return nil
}

func (d *Definition) validateOffers() error {
	if d.Offers == nil {
		return nil
	}
	for _, event := range []string{OfferSend, OfferAssign, OfferAccept, OfferDecline, OfferWithdraw} {
		m := d.Offers.Offer(event)
		if m == nil {
			continue
		}
		what := "offers." + event
		if err := d.states(append(slices.Clone(m.From), m.To), what); err != nil {
			return err
		}
		if (event == OfferAccept || event == OfferDecline || event == OfferWithdraw) && len(m.From) == 0 && d.Offers.Send == nil {
			return invalid("%s needs from, or a send mapping to answer", what)
		}
		if len(m.Requires) > 0 && event != OfferSend && event != OfferAssign {
			return invalid("%s can't require fields", what)
		}
		if err := d.fieldsOnly(m.Requires, what, nil); err != nil {
			return err
		}
		if m.Note != "" && (m.Note != NoteRequired || event != OfferDecline) {
			return invalid("only offers.decline can require a note")
		}
	}
	return nil
}

func (d *Definition) validateTransitions() error {
	if len(d.Transitions) > maxTransitions {
		return invalid("at most %d transitions", maxTransitions)
	}
	seen := map[string]bool{}
	for _, tr := range d.Transitions {
		what := "transition " + tr.ID
		switch {
		case !fieldKey.MatchString(tr.ID) || seen[tr.ID]:
			return invalid("transition ids must be unique lowercase letters, digits and _")
		case len(tr.From) == 0:
			return invalid("%s needs from", what)
		case len(tr.By) == 0:
			return invalid("%s needs by", what)
		case tr.Label != "" && !validLabel(tr.Label):
			return invalid("%s: label must be 1 to %d characters", what, maxLabel)
		case tr.Note != "" && tr.Note != NoteRequired:
			return invalid("%s: note can only be required", what)
		}
		seen[tr.ID] = true
		if err := d.states(append(slices.Clone(tr.From), tr.To), what); err != nil {
			return err
		}
		for _, by := range tr.By {
			if !slices.Contains([]string{ByCreator, ByOwner, ByCollaborator, ByRecipient, ByListMember}, by) {
				return invalid("%s: by must be creator, owner, collaborator, recipient or list_member", what)
			}
		}
		for _, key := range tr.Payload {
			if key != PayloadReply && key != PayloadLinks {
				return invalid("%s: payload can be reply and links", what)
			}
		}
		if err := d.fieldsOnly(tr.Requires, what, tr.Payload); err != nil {
			return err
		}
	}
	return nil
}

// String names a move for an error.
func (tr Transition) String() string { return fmt.Sprintf("%s (%v → %s)", tr.ID, tr.From, tr.To) }
