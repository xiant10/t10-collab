package workitems

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"t10-collab/internal/activity"
	"t10-collab/internal/policy"
	"t10-collab/internal/types"
)

// Moves between a typed item's states: the transitions its type declares, and
// what offer events do to it. Every state change goes through SetState, so a
// move that closes an item ends its offers and workflow as a status change does.

var (
	// ErrMoveNotAllowed means the item's type has no such move from its state,
	// or not for the caller.
	ErrMoveNotAllowed = errors.New("the item's type doesn't allow this move now")
	// ErrRequirementsUnmet means a field, payload or note the move needs is empty.
	ErrRequirementsUnmet = errors.New("the move needs fields or a note that are empty")
)

// transferMode is offers.ModeTransfer, which this package can't import.
const transferMode = "transfer"

const (
	maxMoveNote = 2000
	maxReply    = 10000
	maxMoveLink = 20
)

// Move is a transition the caller may take now, as a client shows it.
type Move struct {
	ID       string   `json:"id"`
	Label    string   `json:"label,omitempty"`
	To       string   `json:"to"`
	Requires []string `json:"requires,omitempty"`
	Payload  []string `json:"payload,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// takes reports whether a relation is one of a move's by.
func takes(r policy.Relation, by []string) bool {
	for _, b := range by {
		switch {
		case b == types.ByCreator && r.Creator, b == types.ByOwner && r.Owner, b == types.ByCollaborator && r.Collaborator,
			b == types.ByRecipient && r.Recipient, b == types.ByListMember && r.ListMember:
			return true
		}
	}
	return false
}

// Moves lists the transitions the caller may take on the item now. A manager's
// read has no relation, so it has none.
func Moves(t types.Type, item Item, r policy.Relation) []Move {
	moves := []Move{}
	if item.State == "" {
		return moves
	}
	for _, tr := range t.Transitions {
		if t.Allows(tr, item.State) && takes(r, tr.By) {
			moves = append(moves, Move{ID: tr.ID, Label: tr.Label, To: tr.To, Requires: tr.Requires, Payload: tr.Payload, Note: tr.Note})
		}
	}
	return moves
}

// Moves resolves the item's type and lists the caller's moves.
func (s *Service) Moves(ctx context.Context, c policy.Caller, item Item, r policy.Relation) ([]Move, error) {
	if item.State == "" {
		return []Move{}, nil
	}
	t, err := types.Resolve(ctx, s.pool, c.TenantID, item.Type)
	if err != nil {
		return nil, err
	}
	return Moves(t, item, r), nil
}

// SetState moves an item to a state of its type and records it. A state in a
// closed phase ends the item's offers and takes it off its workflow, as a
// status change does. The caller holds the item's lock.
func SetState(ctx context.Context, tx pgx.Tx, c policy.Caller, item Item, t types.Type, to string, detail map[string]any, notify []string) (Item, error) {
	from, phase := item.State, t.Phase(to)
	if phase == "" {
		return Item{}, fmt.Errorf("type %s has no state %q", t.ID, to)
	}
	err := tx.QueryRow(ctx,
		`UPDATE collab.work_items SET state = $3, status = $4, type_version = $5,
		        stage = CASE WHEN $4 IN ('done', 'cancelled') THEN NULL ELSE stage END,
		        rounds = CASE WHEN $4 IN ('done', 'cancelled') THEN 0 ELSE rounds END,
		        version = version + 1, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 RETURNING version, updated_at`,
		c.TenantID, item.ID, to, phase, t.Version).Scan(&item.Version, &item.UpdatedAt)
	if err != nil {
		return Item{}, fmt.Errorf("move item: %w", err)
	}
	wasClosed := item.Closed()
	item.State, item.Status, item.TypeVersion = to, phase, t.Version
	people, err := activity.PeopleOn(ctx, tx, c.TenantID, item.ID)
	if err != nil {
		return Item{}, err
	}
	full := map[string]any{"from_state": from, "to_state": to, "status_from": statusOf(t, from, item), "status_to": phase}
	for k, v := range detail {
		full[k] = v
	}
	// The creator is told of every move: on a typed item they're usually the
	// one asking, and waiting on it.
	if err := activity.Record(ctx, tx, c, activity.Change{
		ItemID: item.ID, Kind: "item.state_changed", Detail: full, Notify: append(append(people, item.CreatorID), notify...),
	}); err != nil {
		return Item{}, err
	}
	if item.Closed() && !wasClosed {
		if err := WithdrawOffers(ctx, tx, c, item.ID, OfferScope{}, "item_closed"); err != nil {
			return Item{}, err
		}
	}
	return item, nil
}

func statusOf(t types.Type, state string, item Item) string {
	if p := t.Phase(state); p != "" {
		return p
	}
	return item.Status
}

// OfferMove applies what an offer event does to a typed item's state, under
// the item's lock. A send or assignment the type doesn't allow fails, so the
// offer isn't made; an answer to an offer the item no longer waits on leaves
// it alone. A share send never moves the state. It returns the item as it now is.
func OfferMove(ctx context.Context, tx pgx.Tx, c policy.Caller, itemID, event, mode, note string) (Item, error) {
	item, err := readRow(ctx, tx, c.TenantID, itemID)
	if err != nil || item.State == "" || mode != transferMode {
		return item, err
	}
	t, err := types.Resolve(ctx, tx, c.TenantID, item.Type)
	if err != nil {
		return Item{}, err
	}
	sending := event == types.OfferSend || event == types.OfferAssign
	if event == types.OfferAssign && t.Offers.Offer(types.OfferAssign) == nil && t.Offers.Offer(types.OfferSend) != nil {
		// An assignment skips acceptance, which the type's send state waits for.
		return Item{}, ErrMoveNotAllowed
	}
	m, ok := t.Applies(event, item.State)
	if m == nil || (!ok && !sending) {
		return item, nil
	}
	if !ok {
		return Item{}, ErrMoveNotAllowed
	}
	if len(types.Missing(m.Requires, item.Fields, nil)) > 0 {
		return Item{}, ErrRequirementsUnmet
	}
	if m.Note == types.NoteRequired && strings.TrimSpace(note) == "" {
		return Item{}, ErrRequirementsUnmet
	}
	if m.To == item.State {
		return item, nil
	}
	return SetState(ctx, tx, c, item, t, m.To, map[string]any{"offer_event": event}, nil)
}

// OffersEnded applies a type's withdraw mapping after transfer offers ended
// unanswered: withdrawn, voided or expired. The caller holds the item's lock.
func OffersEnded(ctx context.Context, tx pgx.Tx, c policy.Caller, itemID string) error {
	_, err := OfferMove(ctx, tx, c, itemID, types.OfferWithdraw, transferMode, "")
	return err
}

// TransitionRequest is one transition a person or agent asks for.
type TransitionRequest struct {
	Transition string `json:"transition"`
	Note       string `json:"note"`
	// Payload is the move's result: reply and links, as the transition allows.
	Payload map[string]any `json:"payload"`
	// Version, when set, must be the one the caller read.
	Version int `json:"version"`
}

// checkPayload keeps only the keys a transition allows, in shapes a client can
// show: reply is text, and links are https URLs with titles.
func checkPayload(allowed []string, payload map[string]any) error {
	for key, value := range payload {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("%w: this move takes no %s", ErrInvalid, key)
		}
		switch key {
		case "reply":
			reply, ok := value.(string)
			if !ok || utf8.RuneCountInString(reply) > maxReply {
				return ErrInvalid
			}
		case "links":
			links, ok := value.([]any)
			if !ok || len(links) > maxMoveLink {
				return ErrInvalid
			}
			for _, raw := range links {
				link, ok := raw.(map[string]any)
				url, _ := link["url"].(string)
				title, _ := link["title"].(string)
				if !ok || len(link) > 2 || !strings.HasPrefix(url, "https://") || len(url) > 2000 || utf8.RuneCountInString(title) > 200 {
					return ErrInvalid
				}
			}
		}
	}
	return nil
}

// Transition takes one of the item's type's transitions.
func (s *Service) Transition(ctx context.Context, c policy.Caller, id string, req TransitionRequest) (Item, error) {
	req.Note = strings.TrimSpace(req.Note)
	if utf8.RuneCountInString(req.Note) > maxMoveNote {
		return Item{}, ErrInvalid
	}
	var item Item
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		locked, relation, err := Lock(ctx, tx, c, id)
		if err != nil {
			return err
		}
		if req.Version != 0 && req.Version != locked.Version {
			return ErrVersionConflict
		}
		if locked.State == "" {
			return ErrMoveNotAllowed
		}
		t, err := types.Resolve(ctx, tx, c.TenantID, locked.Type)
		if err != nil {
			return err
		}
		tr, ok := t.Transition(req.Transition)
		if !ok || !t.Allows(tr, locked.State) {
			return ErrMoveNotAllowed
		}
		if !takes(relation, tr.By) {
			return policy.ErrForbidden
		}
		if err := checkPayload(tr.Payload, req.Payload); err != nil {
			return err
		}
		if len(types.Missing(tr.Requires, locked.Fields, req.Payload)) > 0 || (tr.Note == types.NoteRequired && req.Note == "") {
			return ErrRequirementsUnmet
		}
		detail := map[string]any{"transition": tr.ID}
		if req.Note != "" {
			detail["note"] = req.Note
		}
		if len(req.Payload) > 0 {
			detail["payload"] = req.Payload
		}
		if _, err := SetState(ctx, tx, c, locked, t, tr.To, detail, nil); err != nil {
			return err
		}
		item, err = readRow(ctx, tx, c.TenantID, id)
		return err
	})
	if err != nil {
		return Item{}, err
	}
	return item, nil
}
