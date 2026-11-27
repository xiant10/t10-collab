package workitems

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"t10-collab/internal/activity"
	"t10-collab/internal/policy"
)

// Reads for the ontology (internal/ontology): items with where each stands for
// its caller, under the same policy as every list, in one query.

// ErrInUse means someone else is on the item now, so making it can't be undone.
var ErrInUse = errors.New("someone else is on the item")

// ErrGone means the item was already deleted.
var ErrGone = errors.New("the item was already deleted")

// Standing is an item and where it stands for one caller.
type Standing struct {
	Item
	Relation policy.Relation
	// Manager says the caller reads it only as a manager of one of its owners.
	Manager bool
	// PendingOffers counts its unanswered offers, Comments its comments, and
	// Working the agent teammates told about it who haven't acted since
	// (Service.Working).
	PendingOffers, Comments, Working int
}

// FirstArg is the number of a Query's first placeholder: $1 to $4 are the
// tenant, the caller, the reports a manager may read and the limit.
const FirstArg = 5

// Query narrows and orders a Find. Where and Order are SQL on alias i, over
// work_items' columns and these, for the caller: relation (policy.TieSQL),
// pending_offers and comment_count.
type Query struct {
	Where string
	Args  []any
	// Order is the ORDER BY list, ending with i.id so that pages are stable.
	Order string
	Limit int
}

// Find answers up to q.Limit items the caller can read, or a manager may as
// the tenant allows, that match q, in its order.
func (s *Service) Find(ctx context.Context, c policy.Caller, q Query) ([]Standing, error) {
	reports := s.Reports(ctx, c)
	where := "true"
	if q.Where != "" {
		where = q.Where
	}
	scoped := "SELECT i.*, t.*, " + policy.TieSQL + ` AS relation,
		(SELECT count(*) FROM collab.offers f WHERE f.tenant_id = i.tenant_id AND f.item_id = i.id AND f.state = 'pending') AS pending_offers,
		(SELECT count(*) FROM collab.comments m WHERE m.tenant_id = i.tenant_id AND m.item_id = i.id AND m.deleted_at IS NULL) AS comment_count
		FROM collab.work_items i CROSS JOIN LATERAL ` + policy.TiesSQL("$2") + ` t
		WHERE ` + policy.VisibleSQL("$1", "$2") + " OR " + policy.ReportsSQL("$1", "$3::text[]")
	sql := "WITH scoped AS (" + scoped + "), found AS (SELECT * FROM scoped i WHERE " + where + " ORDER BY " + q.Order + ` LIMIT $4)
		SELECT ` + itemColumns + `, tie_creator, tie_owner, tie_collaborator, tie_recipient, tie_list_member, tie_unowned,
		  pending_offers, comment_count, ` + workingSQL + `
		FROM found i ORDER BY ` + q.Order
	rows, err := s.pool.Query(ctx, sql, append([]any{c.TenantID, c.UserID, reports, q.Limit}, q.Args...)...)
	if err != nil {
		return nil, fmt.Errorf("find items: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Standing, error) {
		var st Standing
		r := &st.Relation
		item, err := scanItem(row, &r.Creator, &r.Owner, &r.Collaborator, &r.Recipient, &r.ListMember, &r.Unowned,
			&st.PendingOffers, &st.Comments, &st.Working)
		st.Item = item
		st.Manager = !r.CanRead()
		return st, err
	})
	if err != nil {
		return nil, fmt.Errorf("find items: %w", err)
	}
	return found, nil
}

// Reports are whose owned items the caller may read as their manager, as the
// tenant allows. A manager's view is extra: when the org can't be read now, a
// search still answers what the caller's own ties give them.
func (s *Service) Reports(ctx context.Context, c policy.Caller) []string {
	reports, err := s.managed(ctx, c)
	if err != nil {
		slog.Warn("manager view unavailable", "error", err)
		return []string{}
	}
	if reports == nil {
		return []string{}
	}
	return reports
}

// workingSQL counts Service.Working's agents for item i.
const workingSQL = `(SELECT count(DISTINCT o.for_user_id) FROM collab.outbox o
	WHERE o.tenant_id = i.tenant_id AND o.item_id = i.id AND o.for_user_id LIKE 'acc\_%' AND o.created_at > now() - interval '15 minutes'
	  AND (EXISTS (SELECT 1 FROM collab.work_item_owners w WHERE w.tenant_id = o.tenant_id AND w.item_id = o.item_id AND w.user_id = o.for_user_id)
	    OR EXISTS (SELECT 1 FROM collab.offers f WHERE f.tenant_id = o.tenant_id AND f.item_id = o.item_id AND f.to_user_id = o.for_user_id AND f.state = 'pending'))
	  AND o.created_at > COALESCE((SELECT max(e.at) FROM collab.events e
	      WHERE e.tenant_id = o.tenant_id AND e.item_id = o.item_id AND e.actor_user_id = o.for_user_id), '-infinity'))`

// UndoCreate deletes an item the caller made, while it's still only theirs: no
// one else owns it, follows it, was sent it or commented on it, and it isn't on
// a list. ErrInUse refuses it otherwise, and ErrGone answers an item already
// deleted, so an undo that was interrupted can run again.
func (s *Service) UndoCreate(ctx context.Context, c policy.Caller, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var deleted bool
		err := tx.QueryRow(ctx, "SELECT deleted_at IS NOT NULL FROM collab.work_items WHERE tenant_id = $1 AND id = $2 AND creator_id = $3",
			c.TenantID, id, c.UserID).Scan(&deleted)
		if errors.Is(err, pgx.ErrNoRows) {
			return policy.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read item: %w", err)
		}
		if deleted {
			return ErrGone
		}
		item, err := LockRow(ctx, tx, c.TenantID, id)
		if err != nil {
			return err
		}
		var others bool
		err = tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM collab.offers WHERE tenant_id = $1 AND item_id = $2)
			     OR EXISTS (SELECT 1 FROM collab.comments WHERE tenant_id = $1 AND item_id = $2 AND author_id <> $3)`,
			c.TenantID, id, c.UserID).Scan(&others)
		if err != nil {
			return fmt.Errorf("read who is on the item: %w", err)
		}
		if others || item.ListID != "" || len(item.Collaborators) > 0 || len(item.Owners) != 1 || item.Owners[0] != c.UserID {
			return ErrInUse
		}
		if _, err := tx.Exec(ctx, "UPDATE collab.work_items SET deleted_at = now() WHERE tenant_id = $1 AND id = $2", c.TenantID, id); err != nil {
			return fmt.Errorf("delete item: %w", err)
		}
		return activity.Record(ctx, tx, c, activity.Change{ItemID: id, Kind: "item.deleted", Detail: map[string]any{"undo": true}})
	})
}
