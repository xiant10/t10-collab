// Package activity records item history and in-app notifications. A change, its
// event and its notifications commit in one transaction, so history can't
// disagree with the data.
package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xiant10/t10-modkit/ids"
	"github.com/xiant10/t10-modkit/page"
	"github.com/xiant10/t10-modkit/people"

	"t10-collab/internal/policy"
)

// Change is one thing that happened to an item, or to a list or setting if
// ItemID is empty.
type Change struct {
	ItemID string
	Kind   string
	Detail map[string]any
	// Notify is told about it. The actor never is.
	Notify []string
}

// Record writes the event and one notification per person to tell.
func Record(ctx context.Context, tx pgx.Tx, c policy.Caller, ch Change) error {
	detail, err := json.Marshal(withDefault(ch.Detail))
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	eventID := ids.New("evt")
	_, err = tx.Exec(ctx,
		"INSERT INTO collab.events (tenant_id, id, item_id, actor_user_id, actor_grant_id, kind, detail) VALUES ($1, $2, NULLIF($3, ''), $4, NULLIF($5, ''), $6, $7)",
		c.TenantID, eventID, ch.ItemID, c.UserID, c.GrantID, ch.Kind, detail)
	if err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	notify := slices.Clone(ch.Notify)
	slices.Sort(notify)
	hops := -1
	for _, userID := range slices.Compact(notify) {
		if userID == c.UserID || userID == policy.System {
			continue
		}
		_, err := tx.Exec(ctx,
			"INSERT INTO collab.notifications (tenant_id, id, user_id, event_id) VALUES ($1, $2, $3, $4)",
			c.TenantID, ids.New("ntf"), userID, eventID)
		if err != nil {
			return fmt.Errorf("write notification: %w", err)
		}
		kind, ok := AgentEvents[ch.Kind]
		if !ok || !people.IsAgentID(userID) {
			continue
		}
		if hops < 0 && ch.ItemID != "" {
			sends, comments, err := AgentHops(ctx, tx, c.TenantID, ch.ItemID)
			if err != nil {
				return err
			}
			// Two agents answering each other's comments loop as surely as
			// two passing an item, so either count stops the firing.
			hops = max(sends, comments)
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO collab.outbox (tenant_id, id, event_id, kind, item_id, for_user_id, actor_user_id, cause_grant_id, hops)
			 VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, NULLIF($8, ''), $9)`,
			c.TenantID, ids.New("obx"), eventID, kind, ch.ItemID, userID, c.UserID, c.GrantID, max(hops, 0))
		if err != nil {
			return fmt.Errorf("write outbox: %w", err)
		}
	}
	return nil
}

// AgentEvents maps a change an agent account is told about to the event kind
// t10-automations fires its automations on. Changes not here reach an agent
// account's notifications only.
var AgentEvents = map[string]string{
	"offer.sent":      "work.offered",
	"offer.assigned":  "work.assigned",
	"item.reassigned": "work.assigned",
	"offer.accepted":  "work.accepted",
	"offer.declined":  "work.declined",
	"comment.added":   "work.commented",
	"item.created":    "list.item_added",
}

// AgentHops counts the sends and the comments agent accounts made on an item
// since a person last acted on it. The sweep is nobody, so it neither counts
// nor resets.
func AgentHops(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, tenantID, itemID string) (sends, comments int, err error) {
	err = q.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE e.kind IN ('offer.sent', 'offer.assigned')),
		        count(*) FILTER (WHERE e.kind = 'comment.added')
		 FROM collab.events e
		 WHERE e.tenant_id = $1 AND e.item_id = $2 AND e.kind IN ('offer.sent', 'offer.assigned', 'comment.added')
		   AND e.actor_user_id LIKE 'acc\_%'
		   AND e.seq > COALESCE((SELECT max(p.seq) FROM collab.events p
		                         WHERE p.tenant_id = $1 AND p.item_id = $2
		                           AND p.actor_user_id NOT LIKE 'acc\_%' AND p.actor_user_id <> $3), 0)`,
		tenantID, itemID, policy.System).Scan(&sends, &comments)
	if err != nil {
		return 0, 0, fmt.Errorf("count agent hops: %w", err)
	}
	return sends, comments, nil
}

func withDefault(detail map[string]any) map[string]any {
	if detail == nil {
		return map[string]any{}
	}
	return detail
}

// PeopleOn returns who a change to an item should reach: its owners,
// collaborators and the recipients of its pending offers.
func PeopleOn(ctx context.Context, tx pgx.Tx, tenantID, itemID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT user_id FROM collab.work_item_owners WHERE tenant_id = $1 AND item_id = $2
		 UNION SELECT user_id FROM collab.work_item_collaborators WHERE tenant_id = $1 AND item_id = $2
		 UNION SELECT to_user_id FROM collab.offers WHERE tenant_id = $1 AND item_id = $2 AND state = 'pending'`,
		tenantID, itemID)
	if err != nil {
		return nil, fmt.Errorf("read people on item: %w", err)
	}
	people, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read people on item: %w", err)
	}
	return people, nil
}

// Event is one line of an item's history.
type Event struct {
	ID          string `json:"id"`
	ItemID      string `json:"item_id,omitempty"`
	ActorUserID string `json:"actor_user_id"`
	// ActorGrantID is set when an agent acted for the person.
	ActorGrantID string         `json:"actor_grant_id,omitempty"`
	Kind         string         `json:"kind"`
	Detail       map[string]any `json:"detail"`
	At           time.Time      `json:"at"`
}

// Notification tells a user about an event.
type Notification struct {
	ID     string     `json:"id"`
	ReadAt *time.Time `json:"read_at"`
	Event  Event      `json:"event"`
	// Target references the record this is about, as <module>:<type>:<id>. It's
	// empty when there's no record, as with a settings change.
	Target string `json:"target,omitempty"`
}

// target builds a notification's record reference.
func target(e Event) string {
	if e.ItemID != "" {
		return "collab:work_item:" + e.ItemID
	}
	if listID, ok := e.Detail["list_id"].(string); ok && listID != "" {
		return "collab:list:" + listID
	}
	return ""
}

// Service reads history and notifications.
type Service struct {
	pool *pgxpool.Pool
}

// NewService builds a Service.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// History returns one page of an item's events, oldest first. The caller must
// already have been allowed to read the item.
func (s *Service) History(ctx context.Context, tenantID, itemID, cursor string, limit int) ([]Event, string, error) {
	after := int64(0)
	if cursor != "" {
		var err error
		if after, err = strconv.ParseInt(cursor, 10, 64); err != nil {
			return nil, "", page.ErrBadCursor
		}
	}
	rows, err := s.pool.Query(ctx,
		`SELECT seq, id, COALESCE(item_id, ''), actor_user_id, COALESCE(actor_grant_id, ''), kind, detail, at FROM collab.events
		 WHERE tenant_id = $1 AND item_id = $2 AND seq > $3 ORDER BY seq LIMIT $4`,
		tenantID, itemID, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list events: %w", err)
	}
	seqs := []int64{}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		var seq int64
		err := row.Scan(&seq, &e.ID, &e.ItemID, &e.ActorUserID, &e.ActorGrantID, &e.Kind, &e.Detail, &e.At)
		seqs = append(seqs, seq)
		return e, err
	})
	if err != nil {
		return nil, "", fmt.Errorf("list events: %w", err)
	}
	if len(events) > limit {
		return events[:limit], strconv.FormatInt(seqs[limit-1], 10), nil
	}
	return events, "", nil
}

// Since returns the caller's notifications after a cursor, oldest first, and the
// cursor to send next. It stops short of any transaction still running, so a
// slow commit is delivered late rather than skipped.
func (s *Service) Since(ctx context.Context, c policy.Caller, cursor string, limit int) ([]Notification, string, error) {
	xid, seq := "0", int64(0)
	if cursor != "" {
		var seqText string
		var ok bool
		if xid, seqText, ok = strings.Cut(cursor, "."); !ok {
			return nil, "", page.ErrBadCursor
		}
		var err error
		seq, err = strconv.ParseInt(seqText, 10, 64)
		if _, xidErr := strconv.ParseUint(xid, 10, 64); err != nil || xidErr != nil {
			return nil, "", page.ErrBadCursor
		}
	}
	rows, err := s.pool.Query(ctx,
		`SELECT n.id, n.read_at, n.xid::text, n.seq, e.id, COALESCE(e.item_id, ''), e.actor_user_id, COALESCE(e.actor_grant_id, ''), e.kind, e.detail, e.at
		 FROM collab.notifications n
		 JOIN collab.events e ON e.tenant_id = n.tenant_id AND e.id = n.event_id
		 WHERE n.tenant_id = $1 AND n.user_id = $2
		   AND (n.xid, n.seq) > ($3::text::xid8, $4::bigint)
		   AND n.xid < pg_snapshot_xmin(pg_current_snapshot())
		 ORDER BY n.xid, n.seq LIMIT $5`,
		c.TenantID, c.UserID, xid, seq, limit)
	if err != nil {
		return nil, "", fmt.Errorf("list notifications: %w", err)
	}
	next := cursor
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Notification, error) {
		var n Notification
		var rowXID string
		var rowSeq int64
		err := row.Scan(&n.ID, &n.ReadAt, &rowXID, &rowSeq, &n.Event.ID, &n.Event.ItemID, &n.Event.ActorUserID, &n.Event.ActorGrantID, &n.Event.Kind, &n.Event.Detail, &n.Event.At)
		next = rowXID + "." + strconv.FormatInt(rowSeq, 10)
		n.Target = target(n.Event)
		return n, err
	})
	if err != nil {
		return nil, "", fmt.Errorf("list notifications: %w", err)
	}
	return items, next, nil
}

// Attention is what needs one person, as counts only: digests leave t10, and
// titles and names must not.
type Attention struct {
	OffersToAnswer int `json:"offers_to_answer"`
	NewAssignments int `json:"new_assignments"`
	DueSoon        int `json:"due_soon"`
	Overdue        int `json:"overdue"`
	Declined       int `json:"declined"`
	WaitingOffers  int `json:"waiting_offers"`
}

// Attention counts what needs a person. New assignments, including an admin's
// reassignment, which writes no offer, and declines count from since. Every count goes through VisibleSQL, so a digest can't show what the
// person can't see.
func (s *Service) Attention(ctx context.Context, c policy.Caller, since time.Time) (Attention, error) {
	visible := policy.VisibleSQL("$1", "$2")
	var a Attention
	err := s.pool.QueryRow(ctx, fmt.Sprintf(`
		WITH items AS (SELECT i.* FROM collab.work_items i WHERE %s),
		offers AS (SELECT f.* FROM collab.offers f JOIN items i ON i.tenant_id = f.tenant_id AND i.id = f.item_id WHERE f.tenant_id = $1)
		SELECT
			(SELECT count(*) FROM offers WHERE to_user_id = $2 AND state = 'pending'),
			(SELECT count(*) FROM offers WHERE to_user_id = $2 AND state = 'assigned' AND created_at >= $3)
				+ (SELECT count(*) FROM collab.notifications n
				   JOIN collab.events e ON e.tenant_id = n.tenant_id AND e.id = n.event_id
				   JOIN items i ON i.tenant_id = e.tenant_id AND i.id = e.item_id
				   WHERE n.tenant_id = $1 AND n.user_id = $2 AND e.kind = 'item.reassigned' AND e.at >= $3),
			(SELECT count(*) FROM items i JOIN collab.work_item_owners o ON o.tenant_id = i.tenant_id AND o.item_id = i.id
			 WHERE o.user_id = $2 AND i.status IN ('todo', 'doing') AND i.due_at >= now() AND i.due_at < now() + interval '1 day'),
			(SELECT count(*) FROM items i JOIN collab.work_item_owners o ON o.tenant_id = i.tenant_id AND o.item_id = i.id
			 WHERE o.user_id = $2 AND i.status IN ('todo', 'doing') AND i.due_at < now()),
			(SELECT count(*) FROM offers WHERE from_user_id = $2 AND state = 'declined' AND responded_at >= $3),
			(SELECT count(*) FROM offers WHERE from_user_id = $2 AND state = 'pending' AND created_at < now() - interval '2 days')`, visible),
		c.TenantID, c.UserID, since).Scan(&a.OffersToAnswer, &a.NewAssignments, &a.DueSoon, &a.Overdue, &a.Declined, &a.WaitingOffers)
	if err != nil {
		return Attention{}, fmt.Errorf("count attention: %w", err)
	}
	return a, nil
}

// MarkRead marks the caller's own notifications as read. Other people's IDs
// change nothing.
func (s *Service) MarkRead(ctx context.Context, c policy.Caller, notificationIDs []string) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE collab.notifications SET read_at = now() WHERE tenant_id = $1 AND user_id = $2 AND id = ANY($3) AND read_at IS NULL",
		c.TenantID, c.UserID, notificationIDs)
	if err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	return nil
}
