package lists

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/xiant10/t10-modkit/ids"
	"github.com/xiant10/t10-modkit/people"

	"t10-collab/internal/activity"
	"t10-collab/internal/policy"
)

const (
	maxStages     = 12
	maxStageName  = 60
	maxGuide      = 2000
	defaultRounds = 3
	maxRounds     = 20
)

var (
	// ErrNotOnList means a stage's holder or the lead isn't where they must be:
	// every holder on the list, the lead one of its owners and a person.
	ErrNotOnList = errors.New("every stage's holder must be on the list, and the lead one of its owners")
	// ErrHolderNotAccepting means an agent on a stage doesn't take work from the
	// list's members, so the route would reach it past its admin.
	ErrHolderNotAccepting = errors.New("an agent on a stage doesn't take work from this list's members")
)

// Stage is one step of a workflow.
type Stage struct {
	// ID stays across edits, so items in flight keep their place. A new stage
	// leaves it out.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Holder gets the item at this stage: a list member, person or agent.
	Holder string `json:"holder"`
	// OnChanges is the earlier stage an item goes back to when its holder finds
	// something. Empty when the stage only passes.
	OnChanges string `json:"on_changes,omitempty"`
	// Guide is a note for whoever holds the item at this stage.
	Guide string `json:"guide,omitempty"`
}

// Workflow is a list's route for its items. See docs/workflows.md.
type Workflow struct {
	ListID  string  `json:"list_id"`
	Version int     `json:"version"`
	Stages  []Stage `json:"stages"`
	// Rounds is how many times an item may go back before it goes to the lead.
	Rounds    int       `json:"rounds"`
	Lead      string    `json:"lead"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Stage returns the stage with this ID, and its place.
func (w Workflow) Stage(id string) (Stage, int, bool) {
	i := slices.IndexFunc(w.Stages, func(s Stage) bool { return s.ID == id })
	if i < 0 {
		return Stage{}, -1, false
	}
	return w.Stages[i], i, true
}

// WorkflowInput is a workflow as a list owner saves it.
type WorkflowInput struct {
	Stages []Stage `json:"stages"`
	// Rounds is 3 when left out.
	Rounds int `json:"rounds"`
	// Lead is the caller when left out.
	Lead string `json:"lead"`
}

// clean checks a workflow's shape and gives new stages their IDs. Who the
// holders are is checked against the list and the org separately.
func (in *WorkflowInput) clean(caller string) error {
	if len(in.Stages) == 0 || len(in.Stages) > maxStages {
		return ErrInvalid
	}
	if in.Rounds == 0 {
		in.Rounds = defaultRounds
	}
	if in.Rounds < 1 || in.Rounds > maxRounds {
		return ErrInvalid
	}
	if in.Lead == "" {
		in.Lead = caller
	}
	seen := map[string]bool{}
	for i := range in.Stages {
		st := &in.Stages[i]
		st.Name, st.Guide = strings.TrimSpace(st.Name), strings.TrimSpace(st.Guide)
		if st.Name == "" || utf8.RuneCountInString(st.Name) > maxStageName || utf8.RuneCountInString(st.Guide) > maxGuide || st.Holder == "" {
			return ErrInvalid
		}
		if st.ID == "" {
			st.ID = ids.New("stg")
		}
		if seen[st.ID] || !strings.HasPrefix(st.ID, "stg_") {
			return ErrInvalid
		}
		// Back only, to a stage named before this one, so a route always ends.
		if st.OnChanges != "" && !seen[st.OnChanges] {
			return ErrInvalid
		}
		seen[st.ID] = true
	}
	return nil
}

func (in WorkflowInput) holders() []string {
	var found []string
	for _, st := range in.Stages {
		if !slices.Contains(found, st.Holder) {
			found = append(found, st.Holder)
		}
	}
	return found
}

// ReadWorkflow returns a list's workflow, or nil if it has none. It checks no
// access: callers come with a right to the list or to one of its items.
func ReadWorkflow(ctx context.Context, q policy.Querier, tenantID, listID string) (*Workflow, error) {
	w := Workflow{ListID: listID}
	var stages []byte
	err := q.QueryRow(ctx,
		"SELECT version, stages, rounds, lead_id, updated_by, updated_at FROM collab.list_workflows WHERE tenant_id = $1 AND list_id = $2",
		tenantID, listID).Scan(&w.Version, &stages, &w.Rounds, &w.Lead, &w.UpdatedBy, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read workflow: %w", err)
	}
	if err := json.Unmarshal(stages, &w.Stages); err != nil {
		return nil, fmt.Errorf("read workflow stages: %w", err)
	}
	return &w, nil
}

// SetWorkflow saves a list's workflow, for a list owner. Items at a stage it
// no longer has leave the workflow and stay with their holder. The org reads run
// before the transaction, so no row stays locked while they wait.
func (s *Service) SetWorkflow(ctx context.Context, c policy.Caller, listID string, in WorkflowInput) (Workflow, error) {
	if err := in.clean(c.UserID); err != nil {
		return Workflow{}, err
	}
	if _, err := s.Read(ctx, c, listID); err != nil {
		return Workflow{}, err
	}
	for _, id := range append(in.holders(), in.Lead) {
		m, err := s.org.Member(ctx, c.TenantID, id)
		if errors.Is(err, people.ErrNotFound) || (err == nil && !m.Active()) {
			return Workflow{}, ErrBadPerson
		}
		if err != nil {
			return Workflow{}, err
		}
	}
	stages, err := json.Marshal(in.Stages)
	if err != nil {
		return Workflow{}, fmt.Errorf("encode stages: %w", err)
	}
	var out *Workflow
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwned(ctx, tx, c, listID); err != nil {
			return err
		}
		if err := checkHolders(ctx, tx, c.TenantID, listID, in); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO collab.list_workflows (tenant_id, list_id, stages, rounds, lead_id, updated_by)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (tenant_id, list_id) DO UPDATE SET version = list_workflows.version + 1, stages = $3, rounds = $4,
			   lead_id = $5, updated_by = $6, updated_at = now()`,
			c.TenantID, listID, stages, in.Rounds, in.Lead, c.UserID); err != nil {
			return fmt.Errorf("save workflow: %w", err)
		}
		kept := make([]string, 0, len(in.Stages))
		for _, st := range in.Stages {
			kept = append(kept, st.ID)
		}
		if err := leaveWorkflow(ctx, tx, c, listID, kept); err != nil {
			return err
		}
		if err := activity.Record(ctx, tx, c, activity.Change{Kind: "list.workflow_changed", Detail: map[string]any{"list_id": listID}}); err != nil {
			return err
		}
		out, err = ReadWorkflow(ctx, tx, c.TenantID, listID)
		return err
	})
	if err != nil {
		return Workflow{}, err
	}
	return *out, nil
}

// RemoveWorkflow ends a list's workflow, for a list owner. Its items stay with
// their holders.
func (s *Service) RemoveWorkflow(ctx context.Context, c policy.Caller, listID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwned(ctx, tx, c, listID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "DELETE FROM collab.list_workflows WHERE tenant_id = $1 AND list_id = $2", c.TenantID, listID)
		if err != nil {
			return fmt.Errorf("remove workflow: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		if err := leaveWorkflow(ctx, tx, c, listID, nil); err != nil {
			return err
		}
		return activity.Record(ctx, tx, c, activity.Change{Kind: "list.workflow_removed", Detail: map[string]any{"list_id": listID}})
	})
}

// checkHolders checks that every holder is on the list, the lead is one of its
// owners, and every agent holder takes work from the list's members.
func checkHolders(ctx context.Context, tx pgx.Tx, tenantID, listID string, in WorkflowInput) error {
	rows, err := tx.Query(ctx, "SELECT user_id, role FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2", tenantID, listID)
	if err != nil {
		return fmt.Errorf("read list members: %w", err)
	}
	roles := map[string]string{}
	var id, role string
	if _, err := pgx.ForEachRow(rows, []any{&id, &role}, func() error { roles[id] = role; return nil }); err != nil {
		return fmt.Errorf("read list members: %w", err)
	}
	if roles[in.Lead] != RoleOwner || people.IsAgentID(in.Lead) {
		return ErrNotOnList
	}
	for _, holder := range in.holders() {
		if roles[holder] == "" {
			return ErrNotOnList
		}
	}
	for _, holder := range in.holders() {
		if !people.IsAgentID(holder) {
			continue
		}
		var open bool
		err := tx.QueryRow(ctx,
			"SELECT COALESCE((SELECT everyone OR $3 = ANY(lists) FROM collab.agent_intake WHERE tenant_id = $1 AND user_id = $2), false)",
			tenantID, holder, listID).Scan(&open)
		if err != nil {
			return fmt.Errorf("read intake: %w", err)
		}
		if !open {
			return ErrHolderNotAccepting
		}
	}
	return nil
}

// leaveWorkflow takes the list's items at a stage not in kept off the
// workflow. They stay with their holders, and their history says why.
func leaveWorkflow(ctx context.Context, tx pgx.Tx, c policy.Caller, listID string, kept []string) error {
	if kept == nil {
		kept = []string{}
	}
	rows, err := tx.Query(ctx,
		`UPDATE collab.work_items i SET stage = NULL, rounds = 0, version = i.version + 1, updated_at = now()
		 FROM (SELECT id, stage FROM collab.work_items
		       WHERE tenant_id = $1 AND list_id = $2 AND stage IS NOT NULL AND NOT stage = ANY($3::text[]) FOR NO KEY UPDATE) o
		 WHERE i.tenant_id = $1 AND i.id = o.id
		 RETURNING i.id, o.stage`, c.TenantID, listID, kept)
	if err != nil {
		return fmt.Errorf("take items off the workflow: %w", err)
	}
	type left struct{ Item, Stage string }
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[left])
	if err != nil {
		return fmt.Errorf("take items off the workflow: %w", err)
	}
	for _, l := range found {
		itemID := l.Item
		notify, err := activity.PeopleOn(ctx, tx, c.TenantID, itemID)
		if err != nil {
			return err
		}
		if err := activity.Record(ctx, tx, c, activity.Change{
			ItemID: itemID, Kind: "workflow.left", Detail: map[string]any{"reason": "stage_removed", "stage": l.Stage}, Notify: notify,
		}); err != nil {
			return err
		}
	}
	return nil
}
