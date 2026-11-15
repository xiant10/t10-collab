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
	// ErrCantSkip means a pass asked to go to a stage it can't reach: a later
	// one with only stages that may be skipped between.
	ErrCantSkip = errors.New("the item can pass only to the next stage, or over stages that may be skipped")
)

// Step is where a result would take an item.
type Step struct {
	Stage string `json:"stage,omitempty"`
	Name  string `json:"name,omitempty"`
	// To gets the item. Empty when it's done, or when the stage has several
	// holders, so who gets it is picked at the move.
	To string `json:"to,omitempty"`
	// Holders are the stage's holders when it has several.
	Holders []string `json:"holders,omitempty"`
	// Skips are the stages a skip passes over.
	Skips []string `json:"skips,omitempty"`
	Done  bool     `json:"done,omitempty"`
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
	// Skip are the later stages a pass may go to instead, over stages whose
	// skip_when the holder judges true.
	Skip   []Step `json:"skip,omitempty"`
	Rounds int    `json:"rounds"`
}

// stepTo is a move to a stage.
func stepTo(st lists.Stage) Step {
	if len(st.Holders) > 1 {
		return Step{Stage: st.ID, Name: st.Name, Holders: st.Holders}
	}
	return Step{Stage: st.ID, Name: st.Name, To: st.Holder}
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
		next := stepTo(wf.Stages[i+1])
		p.Pass = &next
	}
	var skips []string
	for j := i + 1; j < len(wf.Stages)-1 && wf.Stages[j].SkipWhen != ""; j++ {
		skips = append(skips, wf.Stages[j].ID)
		to := stepTo(wf.Stages[j+1])
		to.Skips = slices.Clone(skips)
		p.Skip = append(p.Skip, to)
	}
	if back, _, ok := wf.Stage(st.OnChanges); ok {
		back := stepTo(back)
		p.Changes = &back
		if item.Rounds >= wf.Rounds {
			p.Changes = &Step{To: wf.Lead, Reason: "rounds"}
		}
	}
	return p
}

// Advance finishes the caller's stage of an item: pass sends it to the next
// stage's holder, or marks it done after the last; changes sends it back, with
// the note saying what to fix, or to the lead past the workflow's rounds. A
// pass with to goes to that later stage instead, over stages that may be
// skipped, with the note saying why.
func (s *Service) Advance(ctx context.Context, c policy.Caller, itemID, result, note, to string) (workitems.Item, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxNote || (result != ResultPass && result != ResultChanges) ||
		(result == ResultChanges && note == "") || (to != "" && (result != ResultPass || note == "")) {
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
	case to != "" && (p.Pass.Done || to != p.Pass.Stage):
		i := slices.IndexFunc(p.Skip, func(st Step) bool { return st.Stage == to })
		if i < 0 {
			return workitems.Item{}, ErrCantSkip
		}
		m.step = p.Skip[i]
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
// stageID is empty, or moves it there. An owner of the item may start it, and
// any list member may start an item nobody owns at the first stage, as they
// may claim it; a list owner may put it anywhere at any time. Either way its rounds start over:
// a person has looked at it.
func (s *Service) PutAtStage(ctx context.Context, c policy.Caller, itemID, stageID, note string) (workitems.Item, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxNote {
		return workitems.Item{}, ErrInvalid
	}
	item, relation, err := s.items.Read(ctx, c, itemID)
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
	if stageID == "" {
		stageID = wf.Stages[0].ID
	}
	starting := item.Stage == "" && (slices.Contains(item.Owners, c.UserID) || (relation.CanClaim() && stageID == wf.Stages[0].ID))
	if !listOwner && !starting {
		return workitems.Item{}, policy.ErrForbidden
	}
	st, _, ok := wf.Stage(stageID)
	if !ok {
		return workitems.Item{}, ErrInvalid
	}
	return s.moveItem(ctx, c, item, wf, move{from: item.Stage, note: note, step: stepTo(st)})
}

// StartNew starts a new item at the first stage when its list's workflow
// starts each new item. The list's owner chose that, so it needs no right of
// the caller's beyond creating the item; an item already moved is left as is.
func (s *Service) StartNew(ctx context.Context, c policy.Caller, item workitems.Item) (workitems.Item, error) {
	if item.Version != 1 {
		return item, nil
	}
	return s.StartJoined(ctx, c, item)
}

// StartJoined starts an item that has just joined its list, made on it or put
// on it, at the first stage when the list's workflow starts each new item.
func (s *Service) StartJoined(ctx context.Context, c policy.Caller, item workitems.Item) (workitems.Item, error) {
	if item.ListID == "" || item.Stage != "" || item.Closed() {
		return item, nil
	}
	wf, err := lists.ReadWorkflow(ctx, s.pool, c.TenantID, item.ListID)
	if err != nil || wf == nil || !wf.AutoStart {
		return item, err
	}
	return s.moveItem(ctx, c, item, wf, move{step: stepTo(wf.Stages[0]), reason: "auto_start"})
}

// move is one move of an item along its workflow.
type move struct {
	from, result, note string
	step               Step
	rounds             int
	// reason says why a move no holder asked for happened.
	reason string
}

// moveItem makes a move. The holders' org reads run before the transaction;
// holders who have left the organization or the list don't get the item, and
// with none left it goes to the lead. The item must still be as it was read,
// or the move fails with a version conflict, so two holders' results can't
// both land.
func (s *Service) moveItem(ctx context.Context, c policy.Caller, item workitems.Item, wf *lists.Workflow, m move) (workitems.Item, error) {
	var holders []string
	if m.step.Stage != "" {
		for _, id := range append([]string{m.step.To}, m.step.Holders...) {
			if id == "" {
				continue
			}
			holder, err := s.org.Member(ctx, c.TenantID, id)
			if err != nil && !errors.Is(err, people.ErrNotFound) {
				return workitems.Item{}, err
			}
			if err == nil && holder.Active() {
				holders = append(holders, id)
			}
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
		if m.step.Stage != "" {
			to, err := pickHolder(ctx, tx, c.TenantID, item, holders)
			if err != nil {
				return err
			}
			if to == "" {
				m.step, m.rounds = Step{To: wf.Lead, Reason: "holder_gone"}, 0
			} else {
				m.step.To = to
			}
		}
		detail := map[string]any{"from_stage": m.from, "to_stage": m.step.Stage, "to": m.step.To, "round": m.rounds}
		if len(m.step.Skips) > 0 {
			detail["skipped"] = m.step.Skips
		}
		if m.reason != "" {
			detail["reason"] = m.reason
		}
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

// pickHolder picks who of a stage's holders gets the item. It must still be on
// the list; a person who is a plain member must have agreed to hold stages,
// as only the reporting line gives work without asking; and an agent must
// still take work from the list, as its admin may have changed that since the
// workflow was saved. Of those, the one that held it
// before and handed it on, so is a collaborator now, or else the one with the
// fewest open items on the list, the first named on a tie. Owning it now
// doesn't count: whoever makes an item doesn't keep every one. It returns empty
// when none is left on the list, as a holder taken off it since the workflow
// was saved.
func pickHolder(ctx context.Context, tx pgx.Tx, tenantID string, item workitems.Item, holders []string) (string, error) {
	if len(holders) == 0 {
		return "", nil
	}
	var to string
	err := tx.QueryRow(ctx,
		`SELECT h.id FROM unnest($4::text[]) WITH ORDINALITY h(id, n)
		 WHERE EXISTS (SELECT 1 FROM collab.list_members m WHERE m.tenant_id = $1 AND m.list_id = $2 AND m.user_id = h.id
		                AND (m.role = 'owner' OR m.holds_at IS NOT NULL OR h.id LIKE 'acc\_%'))
		   AND (h.id NOT LIKE 'acc\_%' OR EXISTS (SELECT 1 FROM collab.agent_intake a
		        WHERE a.tenant_id = $1 AND a.user_id = h.id AND (a.everyone OR $2 = ANY(a.lists))))
		 ORDER BY EXISTS (SELECT 1 FROM collab.work_item_collaborators o WHERE o.tenant_id = $1 AND o.item_id = $3 AND o.user_id = h.id) DESC,
		   (SELECT count(*) FROM collab.work_item_owners o
		    JOIN collab.work_items i ON i.tenant_id = o.tenant_id AND i.id = o.item_id
		    WHERE o.tenant_id = $1 AND o.user_id = h.id AND i.list_id = $2 AND i.id <> $3
		      AND i.status IN ('todo', 'doing') AND i.deleted_at IS NULL),
		   h.n
		 LIMIT 1`, tenantID, item.ListID, item.ID, holders).Scan(&to)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("pick holder: %w", err)
	}
	return to, nil
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
