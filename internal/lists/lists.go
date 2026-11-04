// Package lists owns shared lists, their members and their unit links. A link
// only fills list_members, and every access check reads that table, so a linked
// list follows the same rules as any other and needs no org read per request.
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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xiant10/t10-modkit/ids"
	"github.com/xiant10/t10-modkit/page"
	"github.com/xiant10/t10-modkit/people"

	"t10-collab/internal/activity"
	"t10-collab/internal/policy"
)

// Roles and sources of a list member.
const (
	RoleOwner    = "owner"
	RoleMember   = "member"
	SourceManual = "manual"
	SourceOrg    = "org"
)

const maxName = 200

var (
	// ErrInvalid means a value failed validation.
	ErrInvalid = errors.New("invalid value")
	// ErrBadPerson means the person named isn't an active member of the tenant.
	ErrBadPerson = errors.New("not an active member")
	// ErrBadUnit means the unit isn't in this tenant.
	ErrBadUnit = errors.New("no such unit in this tenant")
	// ErrLastOwner means the change would leave nobody able to manage the list.
	ErrLastOwner = errors.New("a list needs an owner")
	// ErrManagedByUnit means the next sweep would add this member back.
	ErrManagedByUnit = errors.New("this member comes from the linked unit")
)

// Org is the org reads this package needs.
type Org interface {
	Member(ctx context.Context, tenantID, userID string) (people.Member, error)
	UnitMembers(ctx context.Context, tenantID, unitID string) ([]string, error)
}

// Member is one person on a list.
type Member struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
	Source string `json:"source"`
}

// List is a named group of items and the people who manage them.
type List struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	OrgUnitID  string     `json:"org_unit_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ArchivedAt *time.Time `json:"archived_at"`
	// Members is filled on a single read.
	Members []Member `json:"members,omitempty"`
}

// Service manages lists.
type Service struct {
	pool *pgxpool.Pool
	org  Org
}

// NewService builds a Service.
func NewService(pool *pgxpool.Pool, org Org) *Service { return &Service{pool: pool, org: org} }

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxName {
		return "", ErrInvalid
	}
	return name, nil
}

// Create adds a list owned by the caller.
func (s *Service) Create(ctx context.Context, c policy.Caller, name string) (List, error) {
	name, err := cleanName(name)
	if err != nil {
		return List{}, err
	}
	l := List{ID: ids.New("lst"), Name: name}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, "INSERT INTO collab.lists (tenant_id, id, name) VALUES ($1, $2, $3) RETURNING created_at",
			c.TenantID, l.ID, name).Scan(&l.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert list: %w", err)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO collab.list_members (tenant_id, list_id, user_id, role) VALUES ($1, $2, $3, 'owner')",
			c.TenantID, l.ID, c.UserID); err != nil {
			return fmt.Errorf("insert list owner: %w", err)
		}
		return activity.Record(ctx, tx, c, activity.Change{Kind: "list.created", Detail: map[string]any{"list_id": l.ID}})
	})
	if err != nil {
		return List{}, err
	}
	return l, nil
}

const listColumns = "l.id, l.name, COALESCE(l.org_unit_id, ''), l.created_at, l.archived_at"

func scanList(row pgx.Row) (List, error) {
	var l List
	err := row.Scan(&l.ID, &l.Name, &l.OrgUnitID, &l.CreatedAt, &l.ArchivedAt)
	return l, err
}

// Mine returns one page of the lists the caller is on, newest first.
func (s *Service) Mine(ctx context.Context, c policy.Caller, archived bool, cursor string, limit int) ([]List, string, error) {
	at, id, after, err := page.Decode(cursor)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+listColumns+` FROM collab.lists l
		 JOIN collab.list_members m ON m.tenant_id = l.tenant_id AND m.list_id = l.id AND m.user_id = $2
		 WHERE l.tenant_id = $1 AND (l.archived_at IS NOT NULL) = $3 AND (NOT $4 OR (l.created_at, l.id) < ($5, $6))
		 ORDER BY l.created_at DESC, l.id DESC LIMIT $7`,
		c.TenantID, c.UserID, archived, after, at, id, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list lists: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (List, error) { return scanList(row) })
	if err != nil {
		return nil, "", fmt.Errorf("list lists: %w", err)
	}
	if len(found) > limit {
		found = found[:limit]
		last := found[limit-1]
		return found, page.Encode(last.CreatedAt, last.ID), nil
	}
	return found, "", nil
}

// lockRole locks a list and returns the caller's role, empty if they aren't on
// it. Holding the lock keeps a list from losing its last owner to two removals,
// and keeps the sweep and a link change from crossing.
func lockRole(ctx context.Context, tx pgx.Tx, c policy.Caller, id string) (string, error) {
	var locked string
	err := tx.QueryRow(ctx, "SELECT id FROM collab.lists WHERE tenant_id = $1 AND id = $2 FOR NO KEY UPDATE", c.TenantID, id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", policy.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lock list: %w", err)
	}
	var role string
	err = tx.QueryRow(ctx, "SELECT role FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND user_id = $3",
		c.TenantID, id, c.UserID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", policy.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read list role: %w", err)
	}
	return role, nil
}

func lockOwned(ctx context.Context, tx pgx.Tx, c policy.Caller, id string) error {
	role, err := lockRole(ctx, tx, c, id)
	if err != nil {
		return err
	}
	if role != RoleOwner {
		return policy.ErrForbidden
	}
	return nil
}

func read(ctx context.Context, q interface {
	policy.Querier
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, tenantID, id string) (List, error) {
	l, err := scanList(q.QueryRow(ctx, "SELECT "+listColumns+" FROM collab.lists l WHERE l.tenant_id = $1 AND l.id = $2", tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return List{}, policy.ErrNotFound
	}
	if err != nil {
		return List{}, fmt.Errorf("read list: %w", err)
	}
	rows, err := q.Query(ctx, "SELECT user_id, role, source FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 ORDER BY user_id", tenantID, id)
	if err != nil {
		return List{}, fmt.Errorf("read list members: %w", err)
	}
	if l.Members, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Member]); err != nil {
		return List{}, fmt.Errorf("read list members: %w", err)
	}
	return l, nil
}

// Read returns a list the caller is on, with its members.
func (s *Service) Read(ctx context.Context, c policy.Caller, id string) (List, error) {
	l, err := read(ctx, s.pool, c.TenantID, id)
	if err != nil {
		return List{}, err
	}
	if !slices.ContainsFunc(l.Members, func(m Member) bool { return m.UserID == c.UserID }) {
		return List{}, policy.ErrNotFound
	}
	return l, nil
}

// LinkPatch tells a field left out from one set to empty, which clears a link.
type LinkPatch struct {
	Set   bool
	Value string
}

// UnmarshalJSON runs only when the key is present.
func (p *LinkPatch) UnmarshalJSON(data []byte) error {
	p.Set = true
	return json.Unmarshal(data, &p.Value)
}

// Patch changes a list. Nil fields stay as they are.
type Patch struct {
	Name      *string   `json:"name"`
	Archived  *bool     `json:"archived"`
	OrgUnitID LinkPatch `json:"org_unit_id"`
}

// Update renames, archives or links a list, for a list owner. Setting a link
// reads the unit first, so a list doesn't wait for the sweep and a bad unit
// fails the request. That read runs before the transaction, so no row stays
// locked while it waits on the network.
func (s *Service) Update(ctx context.Context, c policy.Caller, id string, p Patch) (List, error) {
	if _, err := s.Read(ctx, c, id); err != nil {
		return List{}, err
	}
	var unitMembers []string
	if p.OrgUnitID.Set && p.OrgUnitID.Value != "" {
		var err error
		unitMembers, err = s.org.UnitMembers(ctx, c.TenantID, p.OrgUnitID.Value)
		if errors.Is(err, people.ErrNotFound) {
			return List{}, ErrBadUnit
		}
		if err != nil {
			return List{}, err
		}
	}
	var out List
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwned(ctx, tx, c, id); err != nil {
			return err
		}
		if p.Name != nil {
			name, err := cleanName(*p.Name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE collab.lists SET name = $3 WHERE tenant_id = $1 AND id = $2", c.TenantID, id, name); err != nil {
				return fmt.Errorf("rename list: %w", err)
			}
		}
		if p.Archived != nil {
			if _, err := tx.Exec(ctx,
				"UPDATE collab.lists SET archived_at = CASE WHEN $3 THEN COALESCE(archived_at, now()) END WHERE tenant_id = $1 AND id = $2",
				c.TenantID, id, *p.Archived); err != nil {
				return fmt.Errorf("archive list: %w", err)
			}
		}
		if p.OrgUnitID.Set {
			if err := SetLink(ctx, tx, c, id, p.OrgUnitID.Value, unitMembers); err != nil {
				return err
			}
		}
		var err error
		out, err = read(ctx, tx, c.TenantID, id)
		return err
	})
	if err != nil {
		return List{}, err
	}
	return out, nil
}

// SetLink points a list at a unit, or clears the link if unitID is empty. Members
// of the old link become manual, so nobody loses access by surprise. The caller
// holds the list's lock.
func SetLink(ctx context.Context, tx pgx.Tx, c policy.Caller, listID, unitID string, members []string) error {
	var current string
	if err := tx.QueryRow(ctx, "SELECT COALESCE(org_unit_id, '') FROM collab.lists WHERE tenant_id = $1 AND id = $2",
		c.TenantID, listID).Scan(&current); err != nil {
		return fmt.Errorf("read link: %w", err)
	}
	if current == unitID {
		// Not a change. Going on would make its org members manual, and the
		// sweep could then never remove them.
		return nil
	}
	if _, err := tx.Exec(ctx,
		"UPDATE collab.list_members SET source = 'manual' WHERE tenant_id = $1 AND list_id = $2 AND source = 'org'",
		c.TenantID, listID); err != nil {
		return fmt.Errorf("keep old members: %w", err)
	}
	if _, err := tx.Exec(ctx, "UPDATE collab.lists SET org_unit_id = NULLIF($3, '') WHERE tenant_id = $1 AND id = $2",
		c.TenantID, listID, unitID); err != nil {
		return fmt.Errorf("set link: %w", err)
	}
	if _, err := AddOrgMembers(ctx, tx, c.TenantID, listID, members); err != nil {
		return err
	}
	return activity.Record(ctx, tx, c, activity.Change{
		Kind: "list.linked", Detail: map[string]any{"list_id": listID, "org_unit_id": unitID},
	})
}

// AddOrgMembers adds a unit's members, leaving anyone already on the list as they
// are, and returns how many joined.
func AddOrgMembers(ctx context.Context, tx pgx.Tx, tenantID, listID string, members []string) (int64, error) {
	tag, err := tx.Exec(ctx,
		`INSERT INTO collab.list_members (tenant_id, list_id, user_id, role, source)
		 SELECT $1, $2, u, 'member', 'org' FROM unnest($3::text[]) u
		 ON CONFLICT DO NOTHING`, tenantID, listID, members)
	if err != nil {
		return 0, fmt.Errorf("add org members: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RemoveOrgMembers drops org members who aren't in keep. Manual members are never
// touched.
func RemoveOrgMembers(ctx context.Context, tx pgx.Tx, tenantID, listID string, keep []string) (int64, error) {
	tag, err := tx.Exec(ctx,
		`DELETE FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND source = 'org' AND NOT user_id = ANY($3::text[])`,
		tenantID, listID, keep)
	if err != nil {
		return 0, fmt.Errorf("remove org members: %w", err)
	}
	return tag.RowsAffected(), nil
}

func ownerCount(ctx context.Context, tx pgx.Tx, tenantID, listID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, "SELECT count(*) FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND role = 'owner'", tenantID, listID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count list owners: %w", err)
	}
	return n, nil
}

// PutMember adds someone or changes their role. Doing this to an org member makes
// them manual, so they stay after leaving the unit.
func (s *Service) PutMember(ctx context.Context, c policy.Caller, id, userID, role string) error {
	if role == "" {
		role = RoleMember
	}
	if role != RoleOwner && role != RoleMember {
		return ErrInvalid
	}
	if _, err := s.Read(ctx, c, id); err != nil {
		return err
	}
	member, err := s.org.Member(ctx, c.TenantID, userID)
	if errors.Is(err, people.ErrNotFound) || (err == nil && !member.Active()) {
		return ErrBadPerson
	}
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwned(ctx, tx, c, id); err != nil {
			return err
		}
		var current string
		err := tx.QueryRow(ctx, "SELECT role FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND user_id = $3",
			c.TenantID, id, userID).Scan(&current)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read member: %w", err)
		}
		if current == RoleOwner && role != RoleOwner {
			owners, err := ownerCount(ctx, tx, c.TenantID, id)
			if err != nil {
				return err
			}
			if owners == 1 {
				return ErrLastOwner
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO collab.list_members (tenant_id, list_id, user_id, role, source) VALUES ($1, $2, $3, $4, 'manual')
			 ON CONFLICT (tenant_id, list_id, user_id) DO UPDATE SET role = $4, source = 'manual'`,
			c.TenantID, id, userID, role); err != nil {
			return fmt.Errorf("put member: %w", err)
		}
		return activity.Record(ctx, tx, c, activity.Change{
			Kind: "list.member_set", Detail: map[string]any{"list_id": id, "user_id": userID, "role": role}, Notify: []string{userID},
		})
	})
}

// Reasons a list waits in the admin queue.
const (
	// ReasonNoActiveOwner means nobody active can manage the list.
	ReasonNoActiveOwner = "no_active_owner"
	// ReasonNoActiveMembers means nobody active is on it, and it has open items.
	ReasonNoActiveMembers = "no_active_members"
)

// Queued is what a module admin sees of a list that needs an owner.
type Queued struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Reason   string    `json:"reason"`
	QueuedAt time.Time `json:"queued_at"`
}

// Queue returns one page of a tenant's queued lists, oldest first. The caller
// must already have been confirmed as a module admin.
func (s *Service) Queue(ctx context.Context, tenantID, cursor string, limit int) ([]Queued, string, error) {
	at, id, after, err := page.Decode(cursor)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, queue_reason, queued_at FROM collab.lists
		 WHERE tenant_id = $1 AND queued_at IS NOT NULL AND archived_at IS NULL AND (NOT $2 OR (queued_at, id) > ($3, $4))
		 ORDER BY queued_at, id LIMIT $5`, tenantID, after, at, id, limit+1)
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

// SetOwner gives a queued list a new owner, who can then manage everything else
// on it. Anything not in the queue answers not found. The caller must already
// have been confirmed as a module admin.
func (s *Service) SetOwner(ctx context.Context, c policy.Caller, id, to string) error {
	member, err := s.org.Member(ctx, c.TenantID, to)
	if errors.Is(err, people.ErrNotFound) || (err == nil && !member.Active()) {
		return ErrBadPerson
	}
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			"SELECT 1 FROM collab.lists WHERE tenant_id = $1 AND id = $2 AND queued_at IS NOT NULL AND archived_at IS NULL FOR NO KEY UPDATE", c.TenantID, id)
		if err != nil {
			return fmt.Errorf("lock list: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return policy.ErrNotFound
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO collab.list_members (tenant_id, list_id, user_id, role, source) VALUES ($1, $2, $3, 'owner', 'manual')
			 ON CONFLICT (tenant_id, list_id, user_id) DO UPDATE SET role = 'owner', source = 'manual'`,
			c.TenantID, id, to); err != nil {
			return fmt.Errorf("put owner: %w", err)
		}
		if _, err := tx.Exec(ctx, "UPDATE collab.lists SET queued_at = NULL, queue_reason = NULL WHERE tenant_id = $1 AND id = $2", c.TenantID, id); err != nil {
			return fmt.Errorf("unqueue list: %w", err)
		}
		return activity.Record(ctx, tx, c, activity.Change{
			Kind: "list.owner_assigned", Detail: map[string]any{"list_id": id, "user_id": to}, Notify: []string{to},
		})
	})
}

// RemoveDeparted takes people who have left off a list, and tells its remaining
// owners. The sweep calls it, so a list can end up with no owner, and the queue
// picks that up. The caller holds the list's lock.
func RemoveDeparted(ctx context.Context, tx pgx.Tx, c policy.Caller, id string, gone []string) error {
	tag, err := tx.Exec(ctx, "DELETE FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND user_id = ANY($3::text[])", c.TenantID, id, gone)
	if err != nil {
		return fmt.Errorf("remove departed members: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, "SELECT user_id FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND role = 'owner'", c.TenantID, id)
	if err != nil {
		return fmt.Errorf("read list owners: %w", err)
	}
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read list owners: %w", err)
	}
	return activity.Record(ctx, tx, c, activity.Change{
		Kind: "list.members_departed", Detail: map[string]any{"list_id": id, "user_ids": gone}, Notify: owners,
	})
}

// RemoveMember takes someone off a list. Items don't move, so they keep anything
// they own or collaborate on. Removing nobody is no error, so a retry succeeds.
func (s *Service) RemoveMember(ctx context.Context, c policy.Caller, id, userID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwned(ctx, tx, c, id); err != nil {
			return err
		}
		var role, source string
		err := tx.QueryRow(ctx, "SELECT role, source FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND user_id = $3",
			c.TenantID, id, userID).Scan(&role, &source)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read member: %w", err)
		}
		if source == SourceOrg {
			return ErrManagedByUnit
		}
		if role == RoleOwner {
			owners, err := ownerCount(ctx, tx, c.TenantID, id)
			if err != nil {
				return err
			}
			if owners == 1 {
				return ErrLastOwner
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND user_id = $3",
			c.TenantID, id, userID); err != nil {
			return fmt.Errorf("remove member: %w", err)
		}
		return activity.Record(ctx, tx, c, activity.Change{
			Kind: "list.member_removed", Detail: map[string]any{"list_id": id, "user_id": userID}, Notify: []string{userID},
		})
	})
}
