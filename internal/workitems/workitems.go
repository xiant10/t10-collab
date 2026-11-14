// Package workitems owns items, their status, owners, collaborators and
// comments. Every change to an item first locks its row, so two people leaving
// or editing at once can't leave it with nobody or lose a write.
package workitems

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xiant10/t10-modkit/ids"
	"github.com/xiant10/t10-modkit/page"
	"github.com/xiant10/t10-modkit/people"

	"t10-collab/internal/activity"
	"t10-collab/internal/policy"
)

// Statuses of an item.
const (
	StatusTodo      = "todo"
	StatusDoing     = "doing"
	StatusDone      = "done"
	StatusCancelled = "cancelled"
)

// Views of the item list.
const (
	ViewOwned         = "owned"
	ViewSent          = "sent"
	ViewCollaborating = "collaborating"
	ViewList          = "list"
	ViewReports       = "reports"
	// ViewClaimable is open, unowned items on the caller's unarchived lists.
	ViewClaimable = "claimable"
)

// StatusOpen filters for todo and doing together. It's never an item's status.
const StatusOpen = "open"

const (
	maxTitle       = 300
	maxDescription = 20000
	maxComment     = 10000
	// maxMentions is the most people one comment can mention.
	maxMentions = 20
)

var (
	// ErrInvalid means a value failed validation.
	ErrInvalid = errors.New("invalid value")
	// ErrVersionConflict means someone else changed the item first.
	ErrVersionConflict = errors.New("version conflict")
	// ErrLastOwner means leaving would leave the item with nobody.
	ErrLastOwner = errors.New("last owner")
	// ErrAlreadyOwner means the person named already owns the item.
	ErrAlreadyOwner = errors.New("already an owner")
	// ErrBadPerson means the person named isn't an active member of the tenant.
	ErrBadPerson = errors.New("not an active member")
	// ErrAlreadyOwned means someone claimed the item first.
	ErrAlreadyOwned = errors.New("item already has an owner")
	// ErrClosed means a done or cancelled item can't be claimed.
	ErrClosed = errors.New("item is closed")
	// ErrListArchived means an archived list takes no new items.
	ErrListArchived = errors.New("list is archived")
)

// Org is the org reads this package needs.
type Org interface {
	Member(ctx context.Context, tenantID, userID string) (people.Member, error)
	CachedReports(ctx context.Context, tenantID, userID string, transitive bool) ([]string, error)
}

// Item is one piece of work for people to do.
type Item struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Description   string     `json:"description"`
	Status        string     `json:"status"`
	Priority      string     `json:"priority"`
	DueAt         *time.Time `json:"due_at"`
	CreatorID     string     `json:"creator_id"`
	ListID        string     `json:"list_id,omitempty"`
	Version       int        `json:"version"`
	Owners        []string   `json:"owners"`
	Collaborators []string   `json:"collaborators"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	// Suggestion is a send an assistant drafted, for an owner to make. Only
	// owners are shown it.
	Suggestion *Suggestion `json:"suggestion,omitempty"`
	// Links point to where its work happens, such as a pull request.
	Links []Link `json:"links"`
}

// Suggestion is who an assistant thinks an item should go to, and the note.
type Suggestion struct {
	To   string    `json:"to"`
	Note string    `json:"note"`
	At   time.Time `json:"at"`
}

// ForReader leaves out what only an owner may see.
func (i Item) ForReader(userID string) Item {
	if !slices.Contains(i.Owners, userID) {
		i.Suggestion = nil
	}
	return i
}

// Closed reports whether the item is finished, so it can't be sent.
func (i Item) Closed() bool { return closed(i.Status) }

func closed(status string) bool { return status == StatusDone || status == StatusCancelled }

const itemColumns = `i.id, i.title, i.description, i.status, i.priority, i.due_at, i.creator_id, COALESCE(i.list_id, ''), i.version,
	ARRAY(SELECT o.user_id FROM collab.work_item_owners o WHERE o.tenant_id = i.tenant_id AND o.item_id = i.id ORDER BY o.user_id),
	ARRAY(SELECT c.user_id FROM collab.work_item_collaborators c WHERE c.tenant_id = i.tenant_id AND c.item_id = i.id ORDER BY c.user_id),
	i.created_at, i.updated_at, i.suggested_to, i.suggested_note, i.suggested_at, ` + linksColumn

func scanItem(row pgx.Row) (Item, error) {
	var i Item
	var to *string
	var note string
	var at *time.Time
	var links []byte
	err := row.Scan(&i.ID, &i.Title, &i.Description, &i.Status, &i.Priority, &i.DueAt, &i.CreatorID, &i.ListID, &i.Version,
		&i.Owners, &i.Collaborators, &i.CreatedAt, &i.UpdatedAt, &to, &note, &at, &links)
	if to != nil && at != nil {
		i.Suggestion = &Suggestion{To: *to, Note: note, At: *at}
	}
	if err == nil {
		i.Links, err = scanLinks(links)
	}
	return i, err
}

// Service manages items.
type Service struct {
	pool *pgxpool.Pool
	org  Org
}

// NewService builds a Service.
func NewService(pool *pgxpool.Pool, org Org) *Service {
	return &Service{pool: pool, org: org}
}

// NewItem describes an item to create.
type NewItem struct {
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Priority    string     `json:"priority"`
	DueAt       *time.Time `json:"due_at"`
	// ClientRef makes a retry return the first item. It's unique per creator.
	ClientRef string `json:"client_ref"`
	// ListID puts the item on a list the caller is a member of.
	ListID string `json:"list_id"`
	// Unowned starts a list item with no owner, for any member to claim.
	Unowned bool `json:"unowned"`
}

func validPriority(p string) bool { return p == "low" || p == "normal" || p == "high" }

func validTitle(title string) bool {
	return title != "" && utf8.RuneCountInString(title) <= maxTitle
}

// Create adds an item owned by the caller. after runs in the same transaction
// on a new item only, so that a send that fails leaves no item behind.
func (s *Service) Create(ctx context.Context, c policy.Caller, n NewItem, after func(pgx.Tx, Item) error) (Item, error) {
	n.Title = strings.TrimSpace(n.Title)
	if n.Priority == "" {
		n.Priority = "normal"
	}
	if !validTitle(n.Title) || utf8.RuneCountInString(n.Description) > maxDescription || !validPriority(n.Priority) ||
		(n.Unowned && n.ListID == "") {
		return Item{}, ErrInvalid
	}
	var ref, listID *string
	if n.ListID != "" {
		listID = &n.ListID
	}
	if n.ClientRef != "" {
		ref = &n.ClientRef
	}
	var item Item
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if n.ListID != "" {
			var open bool
			err := tx.QueryRow(ctx,
				`SELECT l.archived_at IS NULL FROM collab.lists l
				 JOIN collab.list_members m ON m.tenant_id = l.tenant_id AND m.list_id = l.id AND m.user_id = $3
				 WHERE l.tenant_id = $1 AND l.id = $2 FOR KEY SHARE OF l`, c.TenantID, n.ListID, c.UserID).Scan(&open)
			if errors.Is(err, pgx.ErrNoRows) {
				return policy.ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("read list: %w", err)
			}
			if !open {
				return ErrListArchived
			}
		}
		id := ids.New("wi")
		tag, err := tx.Exec(ctx,
			`INSERT INTO collab.work_items (tenant_id, id, title, description, priority, due_at, creator_id, client_ref, list_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			 ON CONFLICT (tenant_id, creator_id, client_ref) WHERE client_ref IS NOT NULL DO NOTHING`,
			c.TenantID, id, n.Title, n.Description, n.Priority, n.DueAt, c.UserID, ref, listID)
		if err != nil {
			return fmt.Errorf("insert item: %w", err)
		}
		if tag.RowsAffected() == 0 {
			item, err = scanItem(tx.QueryRow(ctx,
				"SELECT "+itemColumns+" FROM collab.work_items i WHERE i.tenant_id = $1 AND i.creator_id = $2 AND i.client_ref = $3 AND i.deleted_at IS NULL",
				c.TenantID, c.UserID, n.ClientRef))
			if errors.Is(err, pgx.ErrNoRows) {
				return policy.ErrNotFound
			}
			return err
		}
		if !n.Unowned {
			if _, err := tx.Exec(ctx,
				"INSERT INTO collab.work_item_owners (tenant_id, item_id, user_id, added_by) VALUES ($1, $2, $3, $3)",
				c.TenantID, id, c.UserID); err != nil {
				return fmt.Errorf("insert owner: %w", err)
			}
		}
		// An agent account on the item's list is told, so it can claim the item
		// as any member may. People see new list items on the list itself.
		var notify []string
		if n.ListID != "" {
			rows, err := tx.Query(ctx,
				`SELECT user_id FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND user_id LIKE 'acc\_%'`,
				c.TenantID, n.ListID)
			if err != nil {
				return fmt.Errorf("read list agents: %w", err)
			}
			if notify, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
				return fmt.Errorf("read list agents: %w", err)
			}
		}
		if err := activity.Record(ctx, tx, c, activity.Change{ItemID: id, Kind: "item.created", Notify: notify}); err != nil {
			return err
		}
		if after != nil {
			if item, err = readRow(ctx, tx, c.TenantID, id); err != nil {
				return err
			}
			if err := after(tx, item); err != nil {
				return err
			}
		}
		item, err = readRow(ctx, tx, c.TenantID, id)
		return err
	})
	if err != nil {
		return Item{}, err
	}
	return item, nil
}

func readRow(ctx context.Context, q policy.Querier, tenantID, id string) (Item, error) {
	item, err := scanItem(q.QueryRow(ctx,
		"SELECT "+itemColumns+" FROM collab.work_items i WHERE i.tenant_id = $1 AND i.id = $2 AND i.deleted_at IS NULL",
		tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, policy.ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("read item: %w", err)
	}
	return item, nil
}

// LockRow locks an item with no access check, for a caller whose right comes from
// somewhere else, such as an offer it holds.
//
// The read is a second statement on purpose. One that waits for a lock rechecks
// the locked row, but its subqueries keep the snapshot from before the wait, so
// it would return owners who have since left. NO KEY UPDATE serializes writers
// without blocking a foreign key check on the row.
func LockRow(ctx context.Context, tx pgx.Tx, tenantID, id string) (Item, error) {
	_, err := tx.Exec(ctx, "SELECT 1 FROM collab.work_items WHERE tenant_id = $1 AND id = $2 FOR NO KEY UPDATE", tenantID, id)
	if err != nil {
		return Item{}, fmt.Errorf("lock item: %w", err)
	}
	return readRow(ctx, tx, tenantID, id)
}

// Lock locks an item the caller can read, and says how they stand to it.
func Lock(ctx context.Context, tx pgx.Tx, c policy.Caller, id string) (Item, policy.Relation, error) {
	item, err := LockRow(ctx, tx, c.TenantID, id)
	if err != nil {
		return Item{}, policy.Relation{}, err
	}
	return relate(ctx, tx, c, item)
}

func relate(ctx context.Context, q policy.Querier, c policy.Caller, item Item) (Item, policy.Relation, error) {
	relation, err := policy.RelationTo(ctx, q, c, item.ID, item.CreatorID, item.ListID)
	if err != nil {
		return Item{}, policy.Relation{}, err
	}
	if !relation.CanRead() {
		return Item{}, policy.Relation{}, policy.ErrNotFound
	}
	return item, relation, nil
}

// Read returns an item the caller can read, and how they stand to it.
func (s *Service) Read(ctx context.Context, c policy.Caller, id string) (Item, policy.Relation, error) {
	item, err := readRow(ctx, s.pool, c.TenantID, id)
	if err != nil {
		return Item{}, policy.Relation{}, err
	}
	return relate(ctx, s.pool, c, item.ForReader(c.UserID))
}

// View returns an item the caller can read, or one owned by someone they manage
// if the tenant allows that. It's read-only, so nothing that writes may use it.
// The org read happens only for a read that would otherwise fail, so ordinary
// reads keep working through an auth outage, and a failed read grants nothing.
func (s *Service) View(ctx context.Context, c policy.Caller, id string) (Item, error) {
	item, _, err := s.ViewAs(ctx, c, id)
	return item, err
}

// ViewAs is View with the caller's relation, which is empty for a manager's read.
func (s *Service) ViewAs(ctx context.Context, c policy.Caller, id string) (Item, policy.Relation, error) {
	item, relation, err := s.Read(ctx, c, id)
	if !errors.Is(err, policy.ErrNotFound) {
		return item, relation, err
	}
	item, err = readRow(ctx, s.pool, c.TenantID, id)
	if err != nil {
		return Item{}, policy.Relation{}, err
	}
	reports, err := s.managed(ctx, c)
	if err != nil {
		return Item{}, policy.Relation{}, policy.ErrNotFound
	}
	if !slices.ContainsFunc(item.Owners, func(owner string) bool { return slices.Contains(reports, owner) }) {
		return Item{}, policy.Relation{}, policy.ErrNotFound
	}
	return item.ForReader(c.UserID), policy.Relation{}, nil
}

// managed returns whose owned items the tenant's setting lets the caller read.
func (s *Service) managed(ctx context.Context, c policy.Caller) ([]string, error) {
	visibility, err := policy.ManagerVisibility(ctx, s.pool, c.TenantID)
	if err != nil || visibility == policy.VisibilityNone {
		return nil, err
	}
	return s.org.CachedReports(ctx, c.TenantID, c.UserID, visibility == policy.VisibilityAll)
}

// Settings are the tenant's choices. Every member can read them, so people know
// who can see their work.
type Settings struct {
	ManagerVisibility string `json:"manager_visibility"`
}

// Settings reads the tenant's settings.
func (s *Service) Settings(ctx context.Context, tenantID string) (Settings, error) {
	value, err := policy.ManagerVisibility(ctx, s.pool, tenantID)
	return Settings{ManagerVisibility: value}, err
}

// SetSettings changes the tenant's settings, and records the admin who did. The
// caller must already have been confirmed as a module admin.
func (s *Service) SetSettings(ctx context.Context, c policy.Caller, next Settings) error {
	if !policy.ValidVisibility(next.ManagerVisibility) {
		return ErrInvalid
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO collab.tenant_settings (tenant_id, manager_visibility, updated_by) VALUES ($1, $2, $3)
			 ON CONFLICT (tenant_id) DO UPDATE SET manager_visibility = $2, updated_by = $3, updated_at = now()`,
			c.TenantID, next.ManagerVisibility, c.UserID)
		if err != nil {
			return fmt.Errorf("write settings: %w", err)
		}
		return activity.Record(ctx, tx, c, activity.Change{
			Kind: "settings.changed", Detail: map[string]any{"manager_visibility": next.ManagerVisibility},
		})
	})
}

// Reasons an item waits in the admin queue.
const (
	// ReasonNoSuccessor means its owners left, and nobody took it on.
	ReasonNoSuccessor = "no_successor"
	// ReasonSuspended means every owner is suspended, which may be temporary.
	ReasonSuspended = "owner_suspended"
)

// Queued is what a module admin sees: titles and owners, never the work itself.
type Queued struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Status   string    `json:"status"`
	Owners   []string  `json:"owners"`
	Reason   string    `json:"reason"`
	QueuedAt time.Time `json:"queued_at"`
}

// Queue returns one page of a tenant's queued items, oldest first. A closed item
// needs nobody, so it stays out, and comes back if someone reopens it. The caller
// must already have been confirmed as a module admin.
func (s *Service) Queue(ctx context.Context, tenantID, cursor string, limit int) ([]Queued, string, error) {
	at, id, after, err := page.Decode(cursor)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT i.id, i.title, i.status,
		        ARRAY(SELECT o.user_id FROM collab.work_item_owners o WHERE o.tenant_id = i.tenant_id AND o.item_id = i.id ORDER BY o.user_id),
		        i.queue_reason, i.queued_at
		 FROM collab.work_items i
		 WHERE i.tenant_id = $1 AND i.queued_at IS NOT NULL AND i.deleted_at IS NULL AND i.status IN ('todo', 'doing')
		   AND (NOT $2 OR (i.queued_at, i.id) > ($3, $4))
		 ORDER BY i.queued_at, i.id LIMIT $5`, tenantID, after, at, id, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list queue: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Queued])
	if err != nil {
		return nil, "", fmt.Errorf("list queue: %w", err)
	}
	if len(found) > limit {
		found = found[:limit]
		last := found[limit-1]
		return found, page.Encode(last.QueuedAt, last.ID), nil
	}
	return found, "", nil
}

// Reassign gives a queued item a new owner, with no acceptance, because nobody
// else can act on it. A suspended owner stays on as a collaborator, so they find
// their work if they return. It reaches queued items only: anything else answers
// not found, because a module admin has no general reach over items. The caller
// must already have been confirmed as a module admin.
func (s *Service) Reassign(ctx context.Context, c policy.Caller, id, to string) (Item, error) {
	member, err := s.org.Member(ctx, c.TenantID, to)
	if errors.Is(err, people.ErrNotFound) || (err == nil && !member.Active()) {
		return Item{}, ErrBadPerson
	}
	if err != nil {
		return Item{}, err
	}
	var item Item
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if item, err = LockRow(ctx, tx, c.TenantID, id); err != nil {
			return err
		}
		var reason *string
		if err := tx.QueryRow(ctx, "SELECT queue_reason FROM collab.work_items WHERE tenant_id = $1 AND id = $2", c.TenantID, id).Scan(&reason); err != nil {
			return fmt.Errorf("read queue: %w", err)
		}
		if reason == nil || item.Closed() {
			return policy.ErrNotFound
		}
		if *reason == ReasonSuspended {
			if _, err := tx.Exec(ctx,
				`INSERT INTO collab.work_item_collaborators (tenant_id, item_id, user_id, added_by)
				 SELECT tenant_id, item_id, user_id, $3 FROM collab.work_item_owners WHERE tenant_id = $1 AND item_id = $2
				 ON CONFLICT DO NOTHING`, c.TenantID, id, c.UserID); err != nil {
				return fmt.Errorf("keep suspended owner: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM collab.work_item_owners WHERE tenant_id = $1 AND item_id = $2", c.TenantID, id); err != nil {
			return fmt.Errorf("remove owners: %w", err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO collab.work_item_owners (tenant_id, item_id, user_id, added_by) VALUES ($1, $2, $3, $4)",
			c.TenantID, id, to, c.UserID); err != nil {
			return fmt.Errorf("add owner: %w", err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM collab.work_item_collaborators WHERE tenant_id = $1 AND item_id = $2 AND user_id = $3",
			c.TenantID, id, to); err != nil {
			return fmt.Errorf("promote collaborator: %w", err)
		}
		if err := WithdrawOffers(ctx, tx, c, id, OfferScope{}, "reassigned"); err != nil {
			return err
		}
		if err := Unqueue(ctx, tx, c.TenantID, id); err != nil {
			return err
		}
		notify, err := activity.PeopleOn(ctx, tx, c.TenantID, id)
		if err != nil {
			return err
		}
		if err := activity.Record(ctx, tx, c, activity.Change{
			ItemID: id, Kind: "item.reassigned", Detail: map[string]any{"from": item.Owners, "to": to}, Notify: notify,
		}); err != nil {
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

// Enqueue puts an item in the admin queue, and records it the first time or if
// the reason changes. The caller holds the item's lock.
func Enqueue(ctx context.Context, tx pgx.Tx, c policy.Caller, id, reason string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE collab.work_items SET queued_at = COALESCE(queued_at, now()), queue_reason = $3
		 WHERE tenant_id = $1 AND id = $2 AND queue_reason IS DISTINCT FROM $3`, c.TenantID, id, reason)
	if err != nil {
		return fmt.Errorf("queue item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return activity.Record(ctx, tx, c, activity.Change{ItemID: id, Kind: "item.queued", Detail: map[string]any{"reason": reason}})
}

// Unqueue takes an item out of the admin queue. The caller holds the item's lock.
func Unqueue(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	_, err := tx.Exec(ctx,
		"UPDATE collab.work_items SET queued_at = NULL, queue_reason = NULL WHERE tenant_id = $1 AND id = $2 AND queued_at IS NOT NULL", tenantID, id)
	if err != nil {
		return fmt.Errorf("unqueue item: %w", err)
	}
	return nil
}

// RemoveDeparted takes people who have left off an item's owners, and tells
// whoever remains. The caller holds the item's lock.
func RemoveDeparted(ctx context.Context, tx pgx.Tx, c policy.Caller, id string, gone []string) error {
	if _, err := tx.Exec(ctx, "DELETE FROM collab.work_item_owners WHERE tenant_id = $1 AND item_id = $2 AND user_id = ANY($3::text[])",
		c.TenantID, id, gone); err != nil {
		return fmt.Errorf("remove departed owners: %w", err)
	}
	notify, err := activity.PeopleOn(ctx, tx, c.TenantID, id)
	if err != nil {
		return err
	}
	return activity.Record(ctx, tx, c, activity.Change{ItemID: id, Kind: "owner.departed", Detail: map[string]any{"user_ids": gone}, Notify: notify})
}

// Filter selects a page of the item list.
type Filter struct {
	View, Status, ListID, Cursor string
	// Query matches part of a title, ignoring case.
	Query string
	// DueFrom and DueBefore bound due_at, from inclusive and before exclusive.
	// Either one leaves out items with no due date.
	DueFrom, DueBefore *time.Time
	Limit              int
}

// likeEscaper keeps a search's own % and _ from acting as wildcards.
var likeEscaper = strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)

// List returns one page of the items the caller can see. The policy filter is in
// the query, so a page is always full if more visible rows exist.
func (s *Service) List(ctx context.Context, c policy.Caller, f Filter) ([]Item, string, error) {
	at, id, after, err := page.Decode(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	visible := policy.VisibleSQL("$1", "$2")
	args := []any{c.TenantID, c.UserID, f.Status, after, at, id, f.Limit + 1}
	// arg adds a value and returns its placeholder, after the seven fixed ones.
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	view := ""
	switch f.View {
	case "":
	case ViewList:
		if f.ListID == "" {
			return nil, "", ErrInvalid
		}
		view = "AND i.list_id = " + arg(f.ListID)
	case ViewReports:
		reports, err := s.managed(ctx, c)
		if err != nil {
			return nil, "", err
		}
		// $2 stays in the query so that the arguments keep their numbers.
		visible = policy.ReportsSQL("$1", arg(reports)+"::text[]") + " AND $2::text <> ''"
	case ViewOwned:
		view = "AND EXISTS (SELECT 1 FROM collab.work_item_owners v WHERE v.tenant_id = i.tenant_id AND v.item_id = i.id AND v.user_id = $2)"
	case ViewSent:
		view = "AND EXISTS (SELECT 1 FROM collab.offers v WHERE v.tenant_id = i.tenant_id AND v.item_id = i.id AND v.from_user_id = $2)"
	case ViewCollaborating:
		view = "AND EXISTS (SELECT 1 FROM collab.work_item_collaborators v WHERE v.tenant_id = i.tenant_id AND v.item_id = i.id AND v.user_id = $2)"
	case ViewClaimable:
		view = `AND i.status IN ('todo', 'doing')
		   AND EXISTS (SELECT 1 FROM collab.list_members v WHERE v.tenant_id = i.tenant_id AND v.list_id = i.list_id AND v.user_id = $2)
		   AND EXISTS (SELECT 1 FROM collab.lists l WHERE l.tenant_id = i.tenant_id AND l.id = i.list_id AND l.archived_at IS NULL)
		   AND NOT EXISTS (SELECT 1 FROM collab.work_item_owners o WHERE o.tenant_id = i.tenant_id AND o.item_id = i.id)`
	default:
		return nil, "", ErrInvalid
	}
	if f.Status != "" && !slices.Contains([]string{StatusTodo, StatusDoing, StatusDone, StatusCancelled, StatusOpen}, f.Status) {
		return nil, "", ErrInvalid
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		if utf8.RuneCountInString(q) > maxTitle {
			return nil, "", ErrInvalid
		}
		view += " AND i.title ILIKE '%' || " + arg(likeEscaper.Replace(q)) + " || '%'"
	}
	if f.DueFrom != nil {
		view += " AND i.due_at >= " + arg(*f.DueFrom)
	}
	if f.DueBefore != nil {
		view += " AND i.due_at < " + arg(*f.DueBefore)
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+itemColumns+" FROM collab.work_items i WHERE "+visible+" "+view+`
		   AND ($3 = '' OR i.status = $3 OR ($3 = 'open' AND i.status IN ('todo', 'doing')))
		   AND (NOT $4 OR (i.created_at, i.id) < ($5, $6))
		 ORDER BY i.created_at DESC, i.id DESC LIMIT $7`,
		args...)
	if err != nil {
		return nil, "", fmt.Errorf("list items: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) {
		item, err := scanItem(row)
		return item.ForReader(c.UserID), err
	})
	if err != nil {
		return nil, "", fmt.Errorf("list items: %w", err)
	}
	if len(items) > f.Limit {
		items = items[:f.Limit]
		last := items[f.Limit-1]
		return items, page.Encode(last.CreatedAt, last.ID), nil
	}
	return items, "", nil
}

// OptionalTime tells a field left out from one set to null.
type OptionalTime struct {
	Set  bool
	Time *time.Time
}

// UnmarshalJSON runs only when the key is present.
func (o *OptionalTime) UnmarshalJSON(data []byte) error {
	o.Set = true
	return json.Unmarshal(data, &o.Time)
}

// Patch changes an item. Nil fields stay as they are. Version must be the one
// the caller read.
type Patch struct {
	Version     int          `json:"version"`
	Title       *string      `json:"title"`
	Description *string      `json:"description"`
	Priority    *string      `json:"priority"`
	Status      *string      `json:"status"`
	DueAt       OptionalTime `json:"due_at"`
}

func (p Patch) apply(item *Item) ([]string, error) {
	changed := []string{}
	if p.Title != nil {
		title := strings.TrimSpace(*p.Title)
		if !validTitle(title) {
			return nil, ErrInvalid
		}
		if title != item.Title {
			item.Title = title
			changed = append(changed, "title")
		}
	}
	if p.Description != nil && *p.Description != item.Description {
		if utf8.RuneCountInString(*p.Description) > maxDescription {
			return nil, ErrInvalid
		}
		item.Description = *p.Description
		changed = append(changed, "description")
	}
	if p.Priority != nil && *p.Priority != item.Priority {
		if !validPriority(*p.Priority) {
			return nil, ErrInvalid
		}
		item.Priority = *p.Priority
		changed = append(changed, "priority")
	}
	if p.DueAt.Set {
		item.DueAt = p.DueAt.Time
		changed = append(changed, "due_at")
	}
	return changed, nil
}

// Update edits fields and status. A status of cancelled, in or out, needs the
// right to cancel. Closing an item withdraws its pending offers.
func (s *Service) Update(ctx context.Context, c policy.Caller, id string, p Patch) (Item, error) {
	if p.Version < 1 {
		return Item{}, ErrInvalid
	}
	var item Item
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var relation policy.Relation
		var err error
		if item, relation, err = Lock(ctx, tx, c, id); err != nil {
			return err
		}
		if !relation.CanEdit() {
			return policy.ErrForbidden
		}
		if item.Version != p.Version {
			return ErrVersionConflict
		}
		from := item.Status
		changed, err := p.apply(&item)
		if err != nil {
			return err
		}
		detail := map[string]any{"fields": changed}
		if p.Status != nil && *p.Status != from {
			to := *p.Status
			if !slices.Contains([]string{StatusTodo, StatusDoing, StatusDone, StatusCancelled}, to) {
				return ErrInvalid
			}
			if (to == StatusCancelled || from == StatusCancelled) && !relation.CanCancel() {
				return policy.ErrForbidden
			}
			item.Status = to
			detail["status_from"], detail["status_to"] = from, to
		} else if len(changed) == 0 {
			return nil
		}
		err = tx.QueryRow(ctx,
			`UPDATE collab.work_items SET title = $3, description = $4, priority = $5, status = $6, due_at = $7,
			        version = version + 1, updated_at = now()
			 WHERE tenant_id = $1 AND id = $2 RETURNING version, updated_at`,
			c.TenantID, id, item.Title, item.Description, item.Priority, item.Status, item.DueAt).Scan(&item.Version, &item.UpdatedAt)
		if err != nil {
			return fmt.Errorf("update item: %w", err)
		}
		notify, err := activity.PeopleOn(ctx, tx, c.TenantID, id)
		if err != nil {
			return err
		}
		if err := activity.Record(ctx, tx, c, activity.Change{ItemID: id, Kind: "item.updated", Detail: detail, Notify: notify}); err != nil {
			return err
		}
		if closed(item.Status) && !closed(from) {
			return WithdrawOffers(ctx, tx, c, id, OfferScope{}, "item_closed")
		}
		return nil
	})
	if err != nil {
		return Item{}, err
	}
	return item.ForReader(c.UserID), nil
}

// Delete hides an item from everyone. Its pending offers are withdrawn first, so
// a recipient isn't left holding one they can't act on.
func (s *Service) Delete(ctx context.Context, c policy.Caller, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, relation, err := Lock(ctx, tx, c, id)
		if err != nil {
			return err
		}
		if !relation.CanDelete() {
			return policy.ErrForbidden
		}
		notify, err := activity.PeopleOn(ctx, tx, c.TenantID, id)
		if err != nil {
			return err
		}
		if err := WithdrawOffers(ctx, tx, c, id, OfferScope{}, "item_deleted"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE collab.work_items SET deleted_at = now() WHERE tenant_id = $1 AND id = $2", c.TenantID, id); err != nil {
			return fmt.Errorf("delete item: %w", err)
		}
		return activity.Record(ctx, tx, c, activity.Change{ItemID: id, Kind: "item.deleted", Notify: notify})
	})
}

// OfferScope narrows WithdrawOffers. Empty fields match every offer.
type OfferScope struct {
	From, To string
	// NonOwners matches offers from anyone who doesn't own the item now.
	NonOwners bool
}

// WithdrawOffers ends an item's pending offers. The caller holds the item's lock.
func WithdrawOffers(ctx context.Context, tx pgx.Tx, c policy.Caller, itemID string, scope OfferScope, reason string) error {
	rows, err := tx.Query(ctx,
		`UPDATE collab.offers SET state = 'withdrawn', responded_at = now()
		 WHERE tenant_id = $1 AND item_id = $2 AND state = 'pending'
		   AND ($3 = '' OR from_user_id = $3) AND ($4 = '' OR to_user_id = $4)
		   AND (NOT $5 OR from_user_id NOT IN (
		       SELECT user_id FROM collab.work_item_owners WHERE tenant_id = $1 AND item_id = $2))
		 RETURNING id, from_user_id, to_user_id`, c.TenantID, itemID, scope.From, scope.To, scope.NonOwners)
	if err != nil {
		return fmt.Errorf("withdraw offers: %w", err)
	}
	type ended struct{ id, from, to string }
	offers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ended, error) {
		var o ended
		return o, row.Scan(&o.id, &o.from, &o.to)
	})
	if err != nil {
		return fmt.Errorf("withdraw offers: %w", err)
	}
	for _, o := range offers {
		err := activity.Record(ctx, tx, c, activity.Change{
			ItemID: itemID, Kind: "offer.withdrawn", Detail: map[string]any{"offer_id": o.id, "reason": reason}, Notify: []string{o.from, o.to},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// StopOwning removes the caller as an owner, and withdraws their pending offers.
func (s *Service) StopOwning(ctx context.Context, c policy.Caller, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		item, relation, err := Lock(ctx, tx, c, id)
		if err != nil {
			return err
		}
		if !relation.Owner {
			return policy.ErrForbidden
		}
		if len(item.Owners) == 1 && item.ListID == "" {
			return ErrLastOwner
		}
		if _, err := tx.Exec(ctx, "DELETE FROM collab.work_item_owners WHERE tenant_id = $1 AND item_id = $2 AND user_id = $3",
			c.TenantID, id, c.UserID); err != nil {
			return fmt.Errorf("remove owner: %w", err)
		}
		if err := WithdrawOffers(ctx, tx, c, id, OfferScope{From: c.UserID}, "sender_left"); err != nil {
			return err
		}
		notify, err := activity.PeopleOn(ctx, tx, c.TenantID, id)
		if err != nil {
			return err
		}
		return activity.Record(ctx, tx, c, activity.Change{ItemID: id, Kind: "owner.left", Notify: notify})
	})
}

// Claim makes a list member the owner of an unowned item. The row lock means two
// people claiming at once can't both win. Offers sent while it had no owner end
// here, or one could later take the item from the person who claimed it.
func (s *Service) Claim(ctx context.Context, c policy.Caller, id string) (Item, error) {
	var item Item
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var relation policy.Relation
		var err error
		if item, relation, err = Lock(ctx, tx, c, id); err != nil {
			return err
		}
		if !relation.ListMember {
			return policy.ErrForbidden
		}
		if !relation.CanClaim() {
			return ErrAlreadyOwned
		}
		if item.Closed() {
			return ErrClosed
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO collab.work_item_owners (tenant_id, item_id, user_id, added_by) VALUES ($1, $2, $3, $3)",
			c.TenantID, id, c.UserID); err != nil {
			return fmt.Errorf("add owner: %w", err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM collab.work_item_collaborators WHERE tenant_id = $1 AND item_id = $2 AND user_id = $3",
			c.TenantID, id, c.UserID); err != nil {
			return fmt.Errorf("promote collaborator: %w", err)
		}
		if err := WithdrawOffers(ctx, tx, c, id, OfferScope{}, "item_claimed"); err != nil {
			return err
		}
		notify, err := activity.PeopleOn(ctx, tx, c.TenantID, id)
		if err != nil {
			return err
		}
		if err := activity.Record(ctx, tx, c, activity.Change{ItemID: id, Kind: "item.claimed", Notify: notify}); err != nil {
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

// AddCollaborator lets someone edit an item and comment on it. The org read runs
// before the transaction, so no row stays locked while it waits on the network.
func (s *Service) AddCollaborator(ctx context.Context, c policy.Caller, id, userID string) error {
	_, relation, err := s.Read(ctx, c, id)
	if err != nil {
		return err
	}
	if !relation.CanManagePeople() {
		return policy.ErrForbidden
	}
	member, err := s.org.Member(ctx, c.TenantID, userID)
	if errors.Is(err, people.ErrNotFound) || (err == nil && !member.Active()) {
		return ErrBadPerson
	}
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		item, relation, err := Lock(ctx, tx, c, id)
		if err != nil {
			return err
		}
		if !relation.CanManagePeople() {
			return policy.ErrForbidden
		}
		if slices.Contains(item.Owners, userID) {
			return ErrAlreadyOwner
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO collab.work_item_collaborators (tenant_id, item_id, user_id, added_by) VALUES ($1, $2, $3, $4)
			 ON CONFLICT DO NOTHING`, c.TenantID, id, userID, c.UserID)
		if err != nil {
			return fmt.Errorf("add collaborator: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		notify, err := activity.PeopleOn(ctx, tx, c.TenantID, id)
		if err != nil {
			return err
		}
		return activity.Record(ctx, tx, c, activity.Change{
			ItemID: id, Kind: "collaborator.added", Detail: map[string]any{"user_id": userID}, Notify: notify,
		})
	})
}

// RemoveCollaborator takes someone off an item. Removing nobody is no error, so
// a retry succeeds.
func (s *Service) RemoveCollaborator(ctx context.Context, c policy.Caller, id, userID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, relation, err := Lock(ctx, tx, c, id)
		if err != nil {
			return err
		}
		if !relation.CanManagePeople() {
			return policy.ErrForbidden
		}
		tag, err := tx.Exec(ctx, "DELETE FROM collab.work_item_collaborators WHERE tenant_id = $1 AND item_id = $2 AND user_id = $3",
			c.TenantID, id, userID)
		if err != nil {
			return fmt.Errorf("remove collaborator: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		notify, err := activity.PeopleOn(ctx, tx, c.TenantID, id)
		if err != nil {
			return err
		}
		return activity.Record(ctx, tx, c, activity.Change{
			ItemID: id, Kind: "collaborator.removed", Detail: map[string]any{"user_id": userID}, Notify: append(notify, userID),
		})
	})
}

// Comment is a note on one item.
type Comment struct {
	ID        string    `json:"id"`
	AuthorID  string    `json:"author_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// AddComment posts a comment on an item the caller can read.
func (s *Service) AddComment(ctx context.Context, c policy.Caller, id, body string, mentions []string) (Comment, error) {
	if strings.TrimSpace(body) == "" || utf8.RuneCountInString(body) > maxComment || len(mentions) > maxMentions {
		return Comment{}, ErrInvalid
	}
	body, mentions = s.namedMentions(ctx, c.TenantID, body, mentions)
	comment := Comment{ID: ids.New("cmt"), AuthorID: c.UserID, Body: body}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		item, _, err := Lock(ctx, tx, c, id)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx,
			"INSERT INTO collab.comments (tenant_id, id, item_id, author_id, body) VALUES ($1, $2, $3, $4, $5) RETURNING created_at",
			c.TenantID, comment.ID, id, c.UserID, body).Scan(&comment.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert comment: %w", err)
		}
		notify, err := activity.PeopleOn(ctx, tx, c.TenantID, id)
		if err != nil {
			return err
		}
		// A mention tells someone who can already read the item, such as a list
		// member who isn't on it. It never gives access: anyone else is left out.
		detail := map[string]any{"comment_id": comment.ID}
		var mentioned []string
		for _, userID := range mentions {
			relation, err := policy.RelationTo(ctx, tx, policy.Caller{TenantID: c.TenantID, UserID: userID}, id, item.CreatorID, item.ListID)
			if err != nil {
				return err
			}
			if relation.CanRead() && !slices.Contains(mentioned, userID) {
				mentioned = append(mentioned, userID)
			}
		}
		if len(mentioned) > 0 {
			detail["mentions"] = mentioned
		}
		return activity.Record(ctx, tx, c, activity.Change{
			ItemID: id, Kind: "comment.added", Detail: detail, Notify: append(notify, mentioned...),
		})
	})
	if err != nil {
		return Comment{}, err
	}
	return comment, nil
}

// DismissSuggestion clears a suggested send, for someone who could make it.
func (s *Service) DismissSuggestion(ctx context.Context, c policy.Caller, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		item, relation, err := Lock(ctx, tx, c, id)
		if err != nil {
			return err
		}
		if !relation.CanSend() {
			return policy.ErrForbidden
		}
		if item.Suggestion == nil {
			return nil
		}
		if _, err := tx.Exec(ctx,
			"UPDATE collab.work_items SET suggested_to = NULL, suggested_note = '', suggested_at = NULL WHERE tenant_id = $1 AND id = $2",
			c.TenantID, id); err != nil {
			return fmt.Errorf("dismiss suggestion: %w", err)
		}
		return activity.Record(ctx, tx, c, activity.Change{ItemID: id, Kind: "item.suggestion_dismissed", Detail: map[string]any{"to": item.Suggestion.To}})
	})
}

// Working is an agent account that was told about an item and hasn't acted on
// it since: it's most likely working on it now.
type Working struct {
	UserID string    `json:"user_id"`
	Since  time.Time `json:"since"`
}

// workingFor is how long an agent told about an item counts as working on it.
// A run takes minutes, and one that ends without acting, or never starts, stops
// showing after this.
const workingFor = 15 * time.Minute

// Working returns the agent accounts expected to act on the item, its owners and
// those with an offer waiting, that were told about it since they last acted.
func (s *Service) Working(ctx context.Context, tenantID string, item Item, offeredTo []string) ([]Working, error) {
	var expected []string
	for _, user := range append(slices.Clone(item.Owners), offeredTo...) {
		if people.IsAgentID(user) && !slices.Contains(expected, user) {
			expected = append(expected, user)
		}
	}
	if len(expected) == 0 {
		return []Working{}, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT o.for_user_id, max(o.created_at) FROM collab.outbox o
		 WHERE o.tenant_id = $1 AND o.item_id = $2 AND o.for_user_id = ANY($3) AND o.created_at > now() - $4::interval
		   AND o.created_at > COALESCE((SELECT max(e.at) FROM collab.events e
		       WHERE e.tenant_id = o.tenant_id AND e.item_id = o.item_id AND e.actor_user_id = o.for_user_id), '-infinity')
		 GROUP BY o.for_user_id ORDER BY o.for_user_id`,
		tenantID, item.ID, expected, fmt.Sprintf("%d seconds", int(workingFor.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("read working agents: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Working])
}

// Mention is the latest comment that mentioned someone, which they haven't
// answered: they've done nothing on the item since.
type Mention struct {
	From string    `json:"from"`
	Body string    `json:"body"`
	At   time.Time `json:"at"`
}

// MentionFor returns the caller's unanswered mention on an item, if there is one.
func (s *Service) MentionFor(ctx context.Context, c policy.Caller, itemID string) (*Mention, error) {
	var m Mention
	err := s.pool.QueryRow(ctx,
		`SELECT e.actor_user_id, cm.body, e.at FROM collab.events e
		 JOIN collab.comments cm ON cm.tenant_id = e.tenant_id AND cm.id = e.detail->>'comment_id' AND cm.deleted_at IS NULL
		 WHERE e.tenant_id = $1 AND e.item_id = $2 AND e.kind = 'comment.added' AND e.detail->'mentions' ? $3 AND e.actor_user_id <> $3
		   AND NOT EXISTS (SELECT 1 FROM collab.events later
		       WHERE later.tenant_id = e.tenant_id AND later.item_id = e.item_id AND later.actor_user_id = $3 AND later.seq > e.seq)
		 ORDER BY e.seq DESC LIMIT 1`, c.TenantID, itemID, c.UserID).Scan(&m.From, &m.Body, &m.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read mention: %w", err)
	}
	return &m, nil
}

// mentionID is a member's ID written as a mention, as an agent that knows people
// only by the IDs in an item writes one. auth-server issues usr_ and acc_ IDs.
var mentionID = regexp.MustCompile(`@((?:usr|acc)_[0-9A-Za-z]+)`)

// namedMentions writes each member's ID mentioned in a comment as their name,
// which is what people read, and adds them to those mentioned. An ID that isn't
// an active member of the tenant, or whose read fails, stays as written.
func (s *Service) namedMentions(ctx context.Context, tenantID, body string, mentions []string) (string, []string) {
	names := map[string]string{}
	for _, match := range mentionID.FindAllStringSubmatch(body, -1) {
		id := match[1]
		if _, seen := names[id]; seen || len(mentions) >= maxMentions {
			continue
		}
		names[id] = ""
		member, err := s.org.Member(ctx, tenantID, id)
		if err != nil || !member.Active() || strings.TrimSpace(member.DisplayName) == "" {
			continue
		}
		names[id] = member.DisplayName
		if !slices.Contains(mentions, id) {
			mentions = append(mentions, id)
		}
	}
	body = mentionID.ReplaceAllStringFunc(body, func(mention string) string {
		if name := names[mention[1:]]; name != "" {
			return "@" + name
		}
		return mention
	})
	return body, mentions
}

// Comments returns one page of an item's comments, oldest first.
func (s *Service) Comments(ctx context.Context, c policy.Caller, id, cursor string, limit int) ([]Comment, string, error) {
	if _, err := s.View(ctx, c, id); err != nil {
		return nil, "", err
	}
	at, lastID, after, err := page.Decode(cursor)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, author_id, body, created_at FROM collab.comments
		 WHERE tenant_id = $1 AND item_id = $2 AND deleted_at IS NULL AND (NOT $3 OR (created_at, id) > ($4, $5))
		 ORDER BY created_at, id LIMIT $6`, c.TenantID, id, after, at, lastID, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list comments: %w", err)
	}
	comments, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Comment])
	if err != nil {
		return nil, "", fmt.Errorf("list comments: %w", err)
	}
	if len(comments) > limit {
		comments = comments[:limit]
		last := comments[limit-1]
		return comments, page.Encode(last.CreatedAt, last.ID), nil
	}
	return comments, "", nil
}
