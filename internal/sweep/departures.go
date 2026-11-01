package sweep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"

	"t10-collab/internal/lists"
	"t10-collab/internal/offers"
	"t10-collab/internal/people"
	"t10-collab/internal/policy"
	"t10-collab/internal/workitems"
)

// roster is one complete read of a tenant's members. Someone missing from it is
// left alone, because only an explicit status means anything.
type roster map[string]people.Member

func (r roster) departed() []string { return r.filter(people.Member.Departed) }

func (r roster) suspended() []string { return r.filter(people.Member.Suspended) }

func (r roster) filter(match func(people.Member) bool) []string {
	found := []string{}
	for id, m := range r {
		if match(m) {
			found = append(found, id)
		}
	}
	slices.Sort(found)
	return found
}

func (r roster) is(id string, match func(people.Member) bool) bool {
	m, ok := r[id]
	return ok && match(m)
}

// departures settles every tenant this module holds anything for.
func (s *Sweeper) departures(ctx context.Context) error {
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id FROM collab.work_item_owners UNION SELECT tenant_id FROM collab.list_members
		 UNION SELECT tenant_id FROM collab.offers WHERE state = 'pending'`)
	if err != nil {
		return fmt.Errorf("read tenants: %w", err)
	}
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read tenants: %w", err)
	}
	for _, tenantID := range tenants {
		// One tenant's failure mustn't stop the rest, and it retries next pass.
		if err := s.tenant(ctx, tenantID); err != nil {
			slog.Warn("sweep skipped a tenant", "tenant", tenantID, "error", err)
		}
	}
	return ctx.Err()
}

// tenant settles one tenant. It reads the whole roster first and then touches
// only Postgres, so a failed read changes nothing and no row stays locked while
// it waits on the network.
func (s *Sweeper) tenant(ctx context.Context, tenantID string) error {
	members, err := s.org.Members(ctx, tenantID)
	if err != nil {
		return err
	}
	r := make(roster, len(members))
	for _, m := range members {
		r[m.UserID] = m
	}
	c := policy.Caller{TenantID: tenantID, UserID: policy.System}
	gone, away := r.departed(), r.suspended()

	rows, err := s.pool.Query(ctx,
		`SELECT i.id FROM collab.work_items i
		 WHERE i.tenant_id = $1 AND i.deleted_at IS NULL AND (
		     i.queued_at IS NOT NULL
		     OR (i.status IN ('todo', 'doing') AND EXISTS (
		         SELECT 1 FROM collab.work_item_owners o WHERE o.tenant_id = i.tenant_id AND o.item_id = i.id AND o.user_id = ANY($2::text[])))
		     OR EXISTS (
		         SELECT 1 FROM collab.offers f WHERE f.tenant_id = i.tenant_id AND f.item_id = i.id AND f.state = 'pending'
		           AND (f.to_user_id = ANY($3::text[]) OR f.expires_at <= now())))`,
		tenantID, append(slices.Clone(gone), away...), gone)
	if err != nil {
		return fmt.Errorf("find items: %w", err)
	}
	items, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("find items: %w", err)
	}
	for _, id := range items {
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return settle(ctx, tx, c, r, id) })
		if err != nil {
			// One item's failure mustn't stop the rest either.
			slog.Warn("sweep skipped an item", "item", id, "error", err)
		}
	}
	return s.settleLists(ctx, c, gone, away)
}

// settle decides what one item needs, from its owners and offers as they stand
// under its lock. Nothing else remembers how far an item has got, so a retry, or
// someone leaving a second time, needs nothing special.
func settle(ctx context.Context, tx pgx.Tx, c policy.Caller, r roster, id string) error {
	item, err := workitems.LockRow(ctx, tx, c.TenantID, id)
	if errors.Is(err, policy.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := offers.Retire(ctx, tx, c, id, r.departed()); err != nil {
		return err
	}
	if item.Closed() {
		// Nothing needs doing on it. If it's reopened, the next pass picks it up.
		return workitems.Unqueue(ctx, tx, c.TenantID, id)
	}
	var gone, away, here []string
	for _, owner := range item.Owners {
		switch {
		case r.is(owner, people.Member.Departed):
			gone = append(gone, owner)
		case r.is(owner, people.Member.Suspended):
			away = append(away, owner)
		default:
			here = append(here, owner)
		}
	}
	// An item off a list always keeps an owner, so its last one stays until
	// someone takes it on.
	if len(gone) > 0 && (len(here)+len(away) > 0 || item.ListID != "") {
		if err := workitems.RemoveDeparted(ctx, tx, c, id, gone); err != nil {
			return err
		}
		gone = nil
	}
	if len(here) > 0 || (item.ListID != "" && len(away) == 0) {
		return workitems.Unqueue(ctx, tx, c.TenantID, id)
	}
	if len(gone) == 0 {
		return workitems.Enqueue(ctx, tx, c, id, workitems.ReasonSuspended)
	}
	held, err := offers.Hold(ctx, tx, c.TenantID, id)
	if err != nil {
		return err
	}
	if held {
		return workitems.Unqueue(ctx, tx, c.TenantID, id)
	}
	next, err := successor(ctx, tx, c.TenantID, item, gone, r)
	if err != nil {
		return err
	}
	if next == "" {
		return workitems.Enqueue(ctx, tx, c, id, workitems.ReasonNoSuccessor)
	}
	if err := offers.OfferFromSweep(ctx, tx, c, id, next); err != nil {
		return err
	}
	return workitems.Unqueue(ctx, tx, c.TenantID, id)
}

// successor picks who the sweep offers an item to next: its creator, then the
// manager of each departed owner. It's empty once a successor has declined, which
// is a real answer, or when nobody is left to ask. An offer that went unanswered
// or was voided just moves on to the next person.
func successor(ctx context.Context, tx pgx.Tx, tenantID string, item workitems.Item, gone []string, r roster) (string, error) {
	rows, err := tx.Query(ctx,
		`SELECT to_user_id, state FROM collab.offers
		 WHERE tenant_id = $1 AND item_id = $2 AND from_user_id = $3 AND state IN ('declined', 'expired', 'voided')`,
		tenantID, item.ID, policy.System)
	if err != nil {
		return "", fmt.Errorf("read sweep offers: %w", err)
	}
	type answer struct{ to, state string }
	answers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (answer, error) {
		var a answer
		return a, row.Scan(&a.to, &a.state)
	})
	if err != nil {
		return "", fmt.Errorf("read sweep offers: %w", err)
	}
	asked := map[string]bool{}
	for _, a := range answers {
		if a.state == "declined" {
			return "", nil
		}
		asked[a.to] = true
	}
	candidates := []string{item.CreatorID}
	for _, owner := range gone {
		candidates = append(candidates, r[owner].ManagerUserID)
	}
	for _, id := range candidates {
		if !asked[id] && r.is(id, people.Member.Active) {
			return id, nil
		}
	}
	return "", nil
}

// settleLists removes people who have left from every list, then works out which
// lists nobody can manage. Suspended people stay on, but can't manage anything.
func (s *Sweeper) settleLists(ctx context.Context, c policy.Caller, gone, away []string) error {
	rows, err := s.pool.Query(ctx,
		"SELECT DISTINCT list_id FROM collab.list_members WHERE tenant_id = $1 AND user_id = ANY($2::text[])", c.TenantID, gone)
	if err != nil {
		return fmt.Errorf("find lists: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("find lists: %w", err)
	}
	for _, id := range found {
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT 1 FROM collab.lists WHERE tenant_id = $1 AND id = $2 FOR NO KEY UPDATE", c.TenantID, id); err != nil {
				return fmt.Errorf("lock list: %w", err)
			}
			return lists.RemoveDeparted(ctx, tx, c, id, gone)
		})
		if err != nil {
			slog.Warn("sweep skipped a list", "list", id, "error", err)
		}
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE collab.lists l SET queue_reason = r.reason,
		        queued_at = CASE WHEN r.reason IS NULL THEN NULL ELSE COALESCE(l.queued_at, now()) END
		 FROM (
		     SELECT a.tenant_id, a.id, CASE
		         WHEN a.archived_at IS NOT NULL THEN NULL
		         WHEN EXISTS (SELECT 1 FROM collab.list_members m WHERE m.tenant_id = a.tenant_id AND m.list_id = a.id
		                        AND m.role = 'owner' AND NOT m.user_id = ANY($2::text[])) THEN NULL
		         WHEN EXISTS (SELECT 1 FROM collab.list_members m WHERE m.tenant_id = a.tenant_id AND m.list_id = a.id
		                        AND NOT m.user_id = ANY($2::text[])) THEN $3
		         WHEN EXISTS (SELECT 1 FROM collab.work_items i WHERE i.tenant_id = a.tenant_id AND i.list_id = a.id
		                        AND i.deleted_at IS NULL AND i.status IN ('todo', 'doing')) THEN $4
		     END AS reason
		     FROM collab.lists a WHERE a.tenant_id = $1
		 ) r
		 WHERE l.tenant_id = r.tenant_id AND l.id = r.id AND l.queue_reason IS DISTINCT FROM r.reason`,
		c.TenantID, append(slices.Clone(gone), away...), lists.ReasonNoActiveOwner, lists.ReasonNoActiveMembers)
	if err != nil {
		return fmt.Errorf("update list queue: %w", err)
	}
	return nil
}
