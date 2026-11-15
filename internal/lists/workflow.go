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
	maxSkipWhen   = 300
	maxHolders    = 8
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
	// Holders are the list members, people or agents, the item goes to at this
	// stage: one of them, the one that held it before or else the one with the
	// fewest open items on the list.
	Holders []string `json:"holders"`
	// Holder is the first of Holders, for readers that know only one. A stage
	// saved before Holders has only this.
	Holder string `json:"holder"`
	// OnChanges is the earlier stage an item goes back to when its holder finds
	// something. Empty when the stage only passes.
	OnChanges string `json:"on_changes,omitempty"`
	// Guide is a note for whoever holds the item at this stage.
	Guide string `json:"guide,omitempty"`
	// SkipWhen says when the holder before may pass the item over this stage,
	// such as "only docs changed". Empty when it can't be skipped.
	SkipWhen string `json:"skip_when,omitempty"`
}

// fill sets Holders and Holder from each other, for a stage saved with only
// one of them.
func (s *Stage) fill() {
	if len(s.Holders) == 0 && s.Holder != "" {
		s.Holders = []string{s.Holder}
	}
	if len(s.Holders) > 0 {
		s.Holder = s.Holders[0]
	}
}

// Workflow is a list's route for its items. See docs/workflows.md.
type Workflow struct {
	ListID  string  `json:"list_id"`
	Version int     `json:"version"`
	Stages  []Stage `json:"stages"`
	// Rounds is how many times an item may go back before it goes to the lead.
	Rounds int    `json:"rounds"`
	Lead   string `json:"lead"`
	// AutoStart starts each new item on the list at the first stage.
	AutoStart bool      `json:"auto_start"`
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
	Lead      string `json:"lead"`
	AutoStart bool   `json:"auto_start"`
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
		st.Name, st.Guide, st.SkipWhen = strings.TrimSpace(st.Name), strings.TrimSpace(st.Guide), strings.TrimSpace(st.SkipWhen)
		st.fill()
		holders := st.Holders[:0:0]
		for _, h := range st.Holders {
			if h == "" || slices.Contains(holders, h) {
				return ErrInvalid
			}
			holders = append(holders, h)
		}
		st.Holders = holders
		if st.Name == "" || utf8.RuneCountInString(st.Name) > maxStageName || utf8.RuneCountInString(st.Guide) > maxGuide ||
			len(st.Holders) == 0 || len(st.Holders) > maxHolders || utf8.RuneCountInString(st.SkipWhen) > maxSkipWhen {
			return ErrInvalid
		}
		// The first stage is where items start, so nothing passes over it.
		if i == 0 && st.SkipWhen != "" {
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
		for _, h := range st.Holders {
			if !slices.Contains(found, h) {
				found = append(found, h)
			}
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
		"SELECT version, stages, rounds, lead_id, auto_start, updated_by, updated_at FROM collab.list_workflows WHERE tenant_id = $1 AND list_id = $2",
		tenantID, listID).Scan(&w.Version, &stages, &w.Rounds, &w.Lead, &w.AutoStart, &w.UpdatedBy, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read workflow: %w", err)
	}
	if err := json.Unmarshal(stages, &w.Stages); err != nil {
		return nil, fmt.Errorf("read workflow stages: %w", err)
	}
	for i := range w.Stages {
		w.Stages[i].fill()
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
		before, err := ReadWorkflow(ctx, tx, c.TenantID, listID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO collab.list_workflows (tenant_id, list_id, stages, rounds, lead_id, auto_start, updated_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 ON CONFLICT (tenant_id, list_id) DO UPDATE SET version = list_workflows.version + 1, stages = $3, rounds = $4,
			   lead_id = $5, auto_start = $6, updated_by = $7, updated_at = now()`,
			c.TenantID, listID, stages, in.Rounds, in.Lead, in.AutoStart, c.UserID); err != nil {
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
		if err := askToHold(ctx, tx, c, listID, before, in.Stages); err != nil {
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
	roles, err := memberRoles(ctx, tx, tenantID, listID)
	if err != nil {
		return err
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

// askToHold asks each person newly named a holder who must agree first, a
// plain member who hasn't, to agree on the list's page. Until they do, moves
// pass them over, as for a holder who has left.
func askToHold(ctx context.Context, tx pgx.Tx, c policy.Caller, listID string, before *Workflow, stages []Stage) error {
	held := map[string]bool{}
	if before != nil {
		for _, st := range before.Stages {
			for _, h := range st.Holders {
				held[h] = true
			}
		}
	}
	named := map[string][]string{}
	var order []string
	for _, st := range stages {
		for _, h := range st.Holders {
			if held[h] || people.IsAgentID(h) || h == c.UserID {
				continue
			}
			if _, ok := named[h]; !ok {
				order = append(order, h)
			}
			named[h] = append(named[h], st.Name)
		}
	}
	for _, h := range order {
		var ask bool
		if err := tx.QueryRow(ctx,
			"SELECT role = 'member' AND holds_at IS NULL FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND user_id = $3",
			c.TenantID, listID, h).Scan(&ask); err != nil {
			return fmt.Errorf("read holder: %w", err)
		}
		if !ask {
			continue
		}
		if err := activity.Record(ctx, tx, c, activity.Change{
			Kind: "list.holder_named", Detail: map[string]any{"list_id": listID, "user_id": h, "stages": named[h]}, Notify: []string{h},
		}); err != nil {
			return err
		}
	}
	return nil
}

// SetHolding records whether the caller, a list member, agrees to hold stages
// of the list's workflow. Withdrawing leaves the items they hold with them.
func (s *Service) SetHolding(ctx context.Context, c policy.Caller, listID string, agree bool) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		role, err := lockRole(ctx, tx, c, listID)
		if err != nil {
			return err
		}
		if role == "" {
			return policy.ErrNotFound
		}
		tag, err := tx.Exec(ctx,
			`UPDATE collab.list_members SET holds_at = CASE WHEN $4 THEN COALESCE(holds_at, now()) END
			 WHERE tenant_id = $1 AND list_id = $2 AND user_id = $3 AND (holds_at IS NULL) = $4`, c.TenantID, listID, c.UserID, agree)
		if err != nil {
			return fmt.Errorf("set holding: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		wf, err := ReadWorkflow(ctx, tx, c.TenantID, listID)
		if err != nil {
			return err
		}
		var notify []string
		if wf != nil {
			notify = []string{wf.Lead}
		}
		return activity.Record(ctx, tx, c, activity.Change{
			Kind: "list.holding_changed", Detail: map[string]any{"list_id": listID, "user_id": c.UserID, "agreed": agree}, Notify: notify,
		})
	})
}

// memberRoles returns each list member's role.
func memberRoles(ctx context.Context, tx pgx.Tx, tenantID, listID string) (map[string]string, error) {
	rows, err := tx.Query(ctx, "SELECT user_id, role FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2", tenantID, listID)
	if err != nil {
		return nil, fmt.Errorf("read list members: %w", err)
	}
	roles := map[string]string{}
	var id, role string
	if _, err := pgx.ForEachRow(rows, []any{&id, &role}, func() error { roles[id] = role; return nil }); err != nil {
		return nil, fmt.Errorf("read list members: %w", err)
	}
	return roles, nil
}

// KeepWorkflow keeps a list's workflow on its members after someone left the
// list or stopped owning it. A lead who is no longer an owner gives way to the
// person who has owned the list longest, who is told. A holder no longer on the
// list leaves each stage that has other holders; a stage whose only holder
// left keeps them, so its items go to the lead, as the list page warns. The
// caller holds the list's lock.
func KeepWorkflow(ctx context.Context, tx pgx.Tx, c policy.Caller, listID string) error {
	wf, err := ReadWorkflow(ctx, tx, c.TenantID, listID)
	if err != nil || wf == nil {
		return err
	}
	roles, err := memberRoles(ctx, tx, c.TenantID, listID)
	if err != nil {
		return err
	}
	detail := map[string]any{"list_id": listID, "reason": "members_changed"}
	var notify []string
	lead := wf.Lead
	if roles[lead] != RoleOwner {
		err := tx.QueryRow(ctx,
			`SELECT user_id FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND role = 'owner' AND user_id NOT LIKE 'acc\_%'
			 ORDER BY created_at, user_id LIMIT 1`, c.TenantID, listID).Scan(&lead)
		if errors.Is(err, pgx.ErrNoRows) {
			// No person owns the list: the admin queue gives it an owner, who
			// becomes the lead then.
			lead = wf.Lead
		} else if err != nil {
			return fmt.Errorf("read list owners: %w", err)
		} else {
			detail["lead_from"], detail["lead"] = wf.Lead, lead
			notify = []string{lead}
		}
	}
	var dropped []string
	for i := range wf.Stages {
		st := &wf.Stages[i]
		kept := st.Holders[:0:0]
		for _, h := range st.Holders {
			if roles[h] != "" {
				kept = append(kept, h)
			}
		}
		if len(kept) > 0 && len(kept) < len(st.Holders) {
			for _, h := range st.Holders {
				if !slices.Contains(kept, h) && !slices.Contains(dropped, h) {
					dropped = append(dropped, h)
				}
			}
			st.Holders, st.Holder = kept, kept[0]
		}
	}
	if lead == wf.Lead && len(dropped) == 0 {
		return nil
	}
	if len(dropped) > 0 {
		detail["holders_removed"] = dropped
	}
	stages, err := json.Marshal(wf.Stages)
	if err != nil {
		return fmt.Errorf("encode stages: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE collab.list_workflows SET version = version + 1, stages = $3, lead_id = $4, updated_by = $5, updated_at = now()
		 WHERE tenant_id = $1 AND list_id = $2`, c.TenantID, listID, stages, lead, c.UserID); err != nil {
		return fmt.Errorf("keep workflow: %w", err)
	}
	return activity.Record(ctx, tx, c, activity.Change{Kind: "list.workflow_changed", Detail: detail, Notify: notify})
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
