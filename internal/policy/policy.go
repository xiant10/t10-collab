// Package policy holds every access rule, so that a route and any later agent
// tool can't disagree. It decides from facts it's given, and from the one SQL
// filter for "items this user can see", which lists apply before pagination.
package policy

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Actions a route or agent tool declares. A person's rights are judged record by
// record below, and an agent also needs the action on every grant of its token.
const (
	ActionRead   = "work:read"
	ActionWrite  = "work:write"
	ActionDelete = "work:delete"
	ActionSend   = "work:send"
)

var (
	// ErrNotFound means the item doesn't exist or the caller can't see it. The
	// two answer the same, so IDs can't be probed.
	ErrNotFound = errors.New("not found")
	// ErrForbidden means the caller can see the item, but not do this to it.
	ErrForbidden = errors.New("not permitted")
)

// System is the actor and offer sender for what the sweep does. It's nobody, so
// it's never told anything.
const System = "system:sweep"

// Caller is a verified person. TenantID comes from the token, never a request.
type Caller struct {
	TenantID string
	UserID   string
	// GrantID is set when an agent is acting for the person, and goes on the record.
	GrantID string
}

// Relation is everything about a caller that access to one item depends on.
type Relation struct {
	Creator, Owner, Collaborator, Recipient bool
	// ListMember means the caller is on the item's list.
	ListMember bool
	// Unowned means the item has no owner at all, which only a list item can.
	Unowned bool
}

// Values of the tenant's manager_visibility setting.
const (
	VisibilityNone   = "none"
	VisibilityDirect = "direct_reports"
	VisibilityAll    = "all_reports"
)

// ValidVisibility reports whether value is a setting a tenant can choose.
func ValidVisibility(value string) bool {
	return value == VisibilityNone || value == VisibilityDirect || value == VisibilityAll
}

// ManagerVisibility reads the tenant's setting. No row means none.
func ManagerVisibility(ctx context.Context, q Querier, tenantID string) (string, error) {
	value := VisibilityNone
	err := q.QueryRow(ctx, "SELECT manager_visibility FROM collab.tenant_settings WHERE tenant_id = $1", tenantID).Scan(&value)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("read settings: %w", err)
	}
	return value, nil
}

// ReportsSQL is the filter for a manager's read-only view, on work_items alias i:
// items owned by anyone in the reports placeholder. Offers, and items a report
// merely created or collaborates on, stay out, so a declined item is never exposed.
func ReportsSQL(tenant, reports string) string {
	return fmt.Sprintf(`(i.tenant_id = %[1]s AND i.deleted_at IS NULL AND EXISTS (
		SELECT 1 FROM collab.work_item_owners o WHERE o.tenant_id = i.tenant_id AND o.item_id = i.id AND o.user_id = ANY(%[2]s)
	))`, tenant, reports)
}

// VisibleSQL is the filter for items the caller can read, on work_items alias i.
// tenant and user are the placeholders holding the caller's tenant and user ID.
// It's the SQL form of CanRead, so the two must change together.
func VisibleSQL(tenant, user string) string {
	return fmt.Sprintf(`(i.tenant_id = %[1]s AND i.deleted_at IS NULL AND (
		i.creator_id = %[2]s
		OR EXISTS (SELECT 1 FROM collab.work_item_owners o WHERE o.tenant_id = i.tenant_id AND o.item_id = i.id AND o.user_id = %[2]s)
		OR EXISTS (SELECT 1 FROM collab.work_item_collaborators c WHERE c.tenant_id = i.tenant_id AND c.item_id = i.id AND c.user_id = %[2]s)
		OR EXISTS (SELECT 1 FROM collab.offers f WHERE f.tenant_id = i.tenant_id AND f.item_id = i.id AND f.to_user_id = %[2]s AND f.state = 'pending')
		OR EXISTS (SELECT 1 FROM collab.list_members m WHERE m.tenant_id = i.tenant_id AND m.list_id = i.list_id AND m.user_id = %[2]s)
	))`, tenant, user)
}

// Querier reads from the pool or from inside a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RelationTo reads how the caller stands to an item.
func RelationTo(ctx context.Context, q Querier, c Caller, itemID, creatorID, listID string) (Relation, error) {
	r := Relation{Creator: creatorID == c.UserID}
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM collab.work_item_owners WHERE tenant_id = $1 AND item_id = $2 AND user_id = $3),
		        EXISTS (SELECT 1 FROM collab.work_item_collaborators WHERE tenant_id = $1 AND item_id = $2 AND user_id = $3),
		        EXISTS (SELECT 1 FROM collab.offers WHERE tenant_id = $1 AND item_id = $2 AND to_user_id = $3 AND state = 'pending'),
		        EXISTS (SELECT 1 FROM collab.list_members WHERE tenant_id = $1 AND list_id = $4 AND user_id = $3),
		        NOT EXISTS (SELECT 1 FROM collab.work_item_owners WHERE tenant_id = $1 AND item_id = $2)`,
		c.TenantID, itemID, c.UserID, listID).Scan(&r.Owner, &r.Collaborator, &r.Recipient, &r.ListMember, &r.Unowned)
	if err != nil {
		return Relation{}, fmt.Errorf("read relation: %w", err)
	}
	return r, nil
}

// CanRead covers reading and commenting. Declining ends access, because a
// recipient is a Recipient only while the offer is pending.
func (r Relation) CanRead() bool {
	return r.Creator || r.Owner || r.Collaborator || r.Recipient || r.ListMember
}

// CanEdit covers fields and moving between todo, doing and done.
func (r Relation) CanEdit() bool { return r.Creator || r.Owner || r.Collaborator || r.ListMember }

// CanSend is any owner. For an unowned list item, it's any list member.
func (r Relation) CanSend() bool { return r.Owner || (r.Unowned && r.ListMember) }

// CanClaim is any list member, while the item has no owners.
func (r Relation) CanClaim() bool { return r.Unowned && r.ListMember }

// CanManagePeople covers adding and removing collaborators.
func (r Relation) CanManagePeople() bool { return r.Creator || r.Owner }

// CanCancel also covers reopening a cancelled item, so that a collaborator can't
// undo an owner's decision.
func (r Relation) CanCancel() bool { return r.Creator || r.Owner }

// CanDelete is any owner, not the creator: a delete hides the item from everyone,
// so it mustn't reach work the creator has handed to someone else. An unowned
// list item is nobody else's work, so its creator can delete it.
func (r Relation) CanDelete() bool { return r.Owner || (r.Unowned && r.Creator) }

// Can is what a caller may do to one item, so that a screen offers only what will
// work. The routes still decide each request.
type Can struct {
	Edit         bool `json:"edit"`
	Cancel       bool `json:"cancel"`
	Claim        bool `json:"claim"`
	Send         bool `json:"send"`
	Delete       bool `json:"delete"`
	ManagePeople bool `json:"manage_people"`
}

// Can reads the relation as actions. A closed item can't be claimed.
func (r Relation) Can(closed bool) Can {
	return Can{
		Edit: r.CanEdit(), Cancel: r.CanCancel(), Claim: r.CanClaim() && !closed, Send: r.CanSend(),
		Delete: r.CanDelete(), ManagePeople: r.CanManagePeople(),
	}
}

// CanWithdraw is an offer's sender, or any owner.
func (r Relation) CanWithdraw(sender bool) bool { return sender || r.Owner }
