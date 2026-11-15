package offers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/xiant10/t10-modkit/ids"
	"github.com/xiant10/t10-modkit/people"

	"t10-collab/internal/activity"
	"t10-collab/internal/lists"
	"t10-collab/internal/policy"
	"t10-collab/internal/workitems"
)

// Results of a workflow stage.
const (
	ResultPass    = "pass"
	ResultChanges = "changes"
)

var (
	// ErrNotOnWorkflow means the item isn't at a stage of its list's workflow.
	ErrNotOnWorkflow = errors.New("this item isn't at a stage of a workflow")
	// ErrNoWayBack means the item's stage has nowhere to send changes back to.
	ErrNoWayBack = errors.New("this stage only passes: it has no stage to send changes back to")
)

// Step is where a result would take an item.
type Step struct {
	Stage string `json:"stage,omitempty"`
	Name  string `json:"name,omitempty"`
	// To gets the item. Empty when it's done.
	To   string `json:"to,omitempty"`
	Done bool   `json:"done,omitempty"`
	// Reason says why it leaves the workflow: rounds when it went back too
	// often, so it goes to the lead.
	Reason string `json:"reason,omitempty"`
}

// Place is where an item stands on its list's workflow, and where each result
// takes it, for the task page and for an agent at the stage.
type Place struct {
	Workflow *lists.Workflow `json:"workflow"`
	// Stage is the item's stage, nil when it isn't on the workflow.
	Stage *lists.Stage `json:"stage"`
	// Pass and Changes are where each result takes it. Changes is nil when the
	// stage only passes.
	Pass    *Step `json:"pass,omitempty"`
	Changes *Step `json:"changes,omitempty"`
	Rounds  int   `json:"rounds"`
}

// PlaceOf returns where an item stands on its list's workflow, or nil if its
// list has none. The caller can read the item.
func (s *Service) PlaceOf(ctx context.Context, tenantID string, item workitems.Item) (*Place, error) {
	if item.ListID == "" {
		return nil, nil
	}
	wf, err := lists.ReadWorkflow(ctx, s.pool, tenantID, item.ListID)
	if err != nil || wf == nil {
		return nil, err
	}
	return place(wf, item), nil
}

func place(wf *lists.Workflow, item workitems.Item) *Place {
	p := &Place{Workflow: wf, Rounds: item.Rounds}
	st, i, ok := wf.Stage(item.Stage)
	if !ok || item.Closed() {
		return p
	}
	p.Stage = &st
	if i == len(wf.Stages)-1 {
		p.Pass = &Step{Done: true}
	} else {
		next := wf.Stages[i+1]
		p.Pass = &Step{Stage: next.ID, Name: next.Name, To: next.Holder}
	}
	if back, _, ok := wf.Stage(st.OnChanges); ok {
		p.Changes = &Step{Stage: back.ID, Name: back.Name, To: back.Holder}
		if item.Rounds >= wf.Rounds {
			p.Changes = &Step{To: wf.Lead, Reason: "rounds"}
		}
	}
	return p
}

// Advance finishes the caller's stage of an item: pass sends it to the next
// stage's holder, or marks it done after the last; changes sends it back, with
// the note saying what to fix, or to the lead past the workflow's rounds.
func (s *Service) Advance(ctx context.Context, c policy.Caller, itemID, result, note string) (workitems.Item, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxNote || (result != ResultPass && result != ResultChanges) ||
		(result == ResultChanges && note == "") {
		return workitems.Item{}, ErrInvalid
	}
	item, _, err := s.items.Read(ctx, c, itemID)
	if err != nil {
		return workitems.Item{}, err
	}
	if !slices.Contains(item.Owners, c.UserID) {
		return workitems.Item{}, policy.ErrForbidden
	}
	p, err := s.PlaceOf(ctx, c.TenantID, item)
	if err != nil {
		return workitems.Item{}, err
	}
	if p == nil || p.Stage == nil {
		return workitems.Item{}, ErrNotOnWorkflow
	}
	m := move{from: p.Stage.ID, result: result, note: note, rounds: item.Rounds}
	switch {
	case result == ResultPass:
		m.step = *p.Pass
	case p.Changes == nil:
		return workitems.Item{}, ErrNoWayBack
	default:
		m.step = *p.Changes
		m.rounds++
	}
	return s.moveItem(ctx, c, item, p.Workflow, m)
}

// PutAtStage starts an item at a stage of its list's workflow, the first when
// stageID is empty, or moves it there. An owner of the item may start it; a
// list owner may put it anywhere at any time. Either way its rounds start over:
// a person has looked at it.
func (s *Service) PutAtStage(ctx context.Context, c policy.Caller, itemID, stageID, note string) (workitems.Item, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxNote {
		return workitems.Item{}, ErrInvalid
	}
	item, _, err := s.items.Read(ctx, c, itemID)
	if err != nil {
		return workitems.Item{}, err
	}
	if item.Closed() {
		return workitems.Item{}, ErrItemClosed
	}
	if item.ListID == "" {
		return workitems.Item{}, ErrNotOnWorkflow
	}
	wf, err := lists.ReadWorkflow(ctx, s.pool, c.TenantID, item.ListID)
	if err != nil {
		return workitems.Item{}, err
	}
	if wf == nil {
		return workitems.Item{}, ErrNotOnWorkflow
	}
	var listOwner bool
	if err := s.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND user_id = $3 AND role = 'owner')",
		c.TenantID, item.ListID, c.UserID).Scan(&listOwner); err != nil {
		return workitems.Item{}, fmt.Errorf("read list role: %w", err)
	}
	if !listOwner && !(item.Stage == "" && slices.Contains(item.Owners, c.UserID)) {
		return workitems.Item{}, policy.ErrForbidden
	}
	if stageID == "" {
		stageID = wf.Stages[0].ID
	}
	st, _, ok := wf.Stage(stageID)
	if !ok {
		return workitems.Item{}, ErrInvalid
	}
	return s.moveItem(ctx, c, item, wf, move{from: item.Stage, note: note, step: Step{Stage: st.ID, Name: st.Name, To: st.Holder}})
}

// move is one move of an item along its workflow.
type move struct {
	from, result, note string
	step               Step
	rounds             int
}

// moveItem makes a move. The holder's org read runs before the transaction; a
// holder who has left the organization or the list sends the item to the lead
// instead. The item must still
// be as it was read, or the move fails with a version conflict, so two
// holders' results can't both land.
func (s *Service) moveItem(ctx context.Context, c policy.Caller, item workitems.Item, wf *lists.Workflow, m move) (workitems.Item, error) {
	if m.step.To != "" && m.step.To != wf.Lead {
		holder, err := s.org.Member(ctx, c.TenantID, m.step.To)
		if errors.Is(err, people.ErrNotFound) || (err == nil && !holder.Active()) {
			m.step = Step{To: wf.Lead, Reason: "holder_gone"}
		} else if err != nil {
			return workitems.Item{}, err
		}
	}
	if m.step.Stage == "" {
		m.rounds = 0
	}
	var out workitems.Item
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		locked, err := workitems.LockRow(ctx, tx, c.TenantID, item.ID)
		if err != nil {
			return err
		}
		if locked.Version != item.Version {
			return workitems.ErrVersionConflict
		}
		// A holder taken off the list since the workflow was saved doesn't get
		// its items; the lead does.
		if m.step.To != "" && m.step.To != wf.Lead {
			var on bool
			if err := tx.QueryRow(ctx,
				"SELECT EXISTS (SELECT 1 FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND user_id = $3)",
				c.TenantID, item.ListID, m.step.To).Scan(&on); err != nil {
				return fmt.Errorf("read list member: %w", err)
			}
			if !on {
				m.step, m.rounds = Step{To: wf.Lead, Reason: "holder_gone"}, 0
			}
		}
		detail := map[string]any{"from_stage": m.from, "to_stage": m.step.Stage, "to": m.step.To, "round": m.rounds}
		if m.result != "" {
			detail["result"] = m.result
		}
		if m.note != "" {
			detail["note"] = m.note
		}
		if m.step.Reason != "" {
			detail["reason"] = m.step.Reason
		}
		status := locked.Status
		if m.step.Done {
			status = workitems.StatusDone
			detail["done"] = true
		} else {
			offer, err := handTo(ctx, tx, c, locked, m.step.To, m.note)
			if err != nil {
				return err
			}
			if offer != "" {
				detail["offer_id"] = offer
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE collab.work_items SET stage = NULLIF($3, ''), rounds = $4, status = $5, version = version + 1, updated_at = now(),
			        suggested_to = NULL, suggested_note = '', suggested_at = NULL
			 WHERE tenant_id = $1 AND id = $2`, c.TenantID, item.ID, m.step.Stage, m.rounds, status); err != nil {
			return fmt.Errorf("move item: %w", err)
		}
		if m.step.Done {
			if err := workitems.WithdrawOffers(ctx, tx, c, item.ID, workitems.OfferScope{}, "item_closed"); err != nil {
				return err
			}
		}
		notify, err := activity.PeopleOn(ctx, tx, c.TenantID, item.ID)
		if err != nil {
			return err
		}
		if err := activity.Record(ctx, tx, c, activity.Change{ItemID: item.ID, Kind: "workflow.moved", Detail: detail, Notify: notify}); err != nil {
			return err
		}
		out, err = workitems.LockRow(ctx, tx, c.TenantID, item.ID)
		return err
	})
	if err != nil {
		return workitems.Item{}, err
	}
	return out.ForReader(c.UserID), nil
}

// handTo makes to the item's only owner: the owners before become
// collaborators, so they're told about it but not woken, and pending offers
// end. The move is recorded as an assignment from the caller, as a send is.
// It returns that offer's ID, or empty if to already held it alone.
func handTo(ctx context.Context, tx pgx.Tx, c policy.Caller, item workitems.Item, to, note string) (string, error) {
	if len(item.Owners) == 1 && item.Owners[0] == to {
		return "", nil
	}
	offer := ""
	if !slices.Contains(item.Owners, to) {
		offer = ids.New("ofr")
		if _, err := tx.Exec(ctx,
			`INSERT INTO collab.offers (tenant_id, id, item_id, from_user_id, to_user_id, mode, note, state)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			c.TenantID, offer, item.ID, c.UserID, to, ModeTransfer, note, StateAssigned); err != nil {
			return "", fmt.Errorf("record move: %w", err)
		}
	}
	if err := workitems.WithdrawOffers(ctx, tx, c, item.ID, workitems.OfferScope{}, "workflow_moved"); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO collab.work_item_collaborators (tenant_id, item_id, user_id, added_by)
		 SELECT tenant_id, item_id, user_id, $3 FROM collab.work_item_owners WHERE tenant_id = $1 AND item_id = $2 AND user_id <> $4
		 ON CONFLICT DO NOTHING`, c.TenantID, item.ID, c.UserID, to); err != nil {
		return "", fmt.Errorf("keep holders as collaborators: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM collab.work_item_owners WHERE tenant_id = $1 AND item_id = $2 AND user_id <> $3",
		c.TenantID, item.ID, to); err != nil {
		return "", fmt.Errorf("remove holders: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO collab.work_item_owners (tenant_id, item_id, user_id, added_by) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		c.TenantID, item.ID, to, c.UserID); err != nil {
		return "", fmt.Errorf("add holder: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM collab.work_item_collaborators WHERE tenant_id = $1 AND item_id = $2 AND user_id = $3",
		c.TenantID, item.ID, to); err != nil {
		return "", fmt.Errorf("promote holder: %w", err)
	}
	return offer, nil
}
