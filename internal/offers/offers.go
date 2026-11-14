// Package offers owns sending, accepting, declining and withdrawing. The service
// works out whether a send is an offer or an assignment from the org, so a caller
// can't choose to skip acceptance.
package offers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xiant10/t10-modkit/ids"
	"github.com/xiant10/t10-modkit/page"
	"github.com/xiant10/t10-modkit/people"

	"t10-collab/internal/activity"
	"t10-collab/internal/db"
	"t10-collab/internal/policy"
	"t10-collab/internal/workitems"
)

// Modes of a send.
const (
	ModeTransfer = "transfer"
	ModeShare    = "share"
)

// States of an offer.
const (
	StatePending  = "pending"
	StateAssigned = "assigned"
)

// Boxes of the offer list.
const (
	BoxInbox = "inbox"
	BoxSent  = "sent"
)

const (
	maxNote = 2000
	// HoldFor is how long an offer can hold a departed owner's item.
	HoldFor = 14 * 24 * time.Hour
	// sendsPerHour limits how fast one person can fill colleagues' inboxes. It's
	// counted in Postgres, so that it holds across replicas.
	sendsPerHour = 100
	// agentSendsPerHour is the same limit for an agent account, lower because a
	// run can send without anyone watching.
	agentSendsPerHour = 60
	// MaxAgentHops is how many times agent accounts may pass an item on with
	// no person acting. The send that would pass it is redirected to the
	// sending agent account's manager instead.
	MaxAgentHops = 3
)

var (
	// ErrInvalid means a value failed validation.
	ErrInvalid = errors.New("invalid value")
	// ErrItemClosed means a done or cancelled item can't be sent.
	ErrItemClosed = errors.New("item is closed")
	// ErrAlreadyOwner means the recipient owns the item.
	ErrAlreadyOwner = errors.New("already an owner")
	// ErrOfferExists means a pending offer already fills this slot.
	ErrOfferExists = errors.New("a pending offer exists")
	// ErrNotPending means someone answered or withdrew the offer first.
	ErrNotPending = errors.New("offer is not pending")
	// ErrRefReused means a client_ref was used for a different send.
	ErrRefReused = errors.New("client_ref was used for another send")
	// ErrRateLimited means the sender has hit the hourly limit.
	ErrRateLimited = errors.New("too many sends")
	// ErrNotAccepting means the recipient is an agent account that doesn't take
	// work from the sender. Its reporting line can still assign it work.
	ErrNotAccepting = errors.New("this agent doesn't take work from you")
	// ErrNoManager means an item agents kept passing on has nobody to go to.
	ErrNoManager = errors.New("agents passed this item on too often, and the sender has no manager to send it to")
)

// Org is the set of reads a send depends on.
type Org interface {
	Member(ctx context.Context, tenantID, userID string) (people.Member, error)
	Reports(ctx context.Context, tenantID, userID string) ([]string, error)
	Units(ctx context.Context, tenantID string) ([]people.Unit, error)
}

// Offer is one send, of any outcome.
type Offer struct {
	ID           string     `json:"id"`
	ItemID       string     `json:"item_id"`
	ItemTitle    string     `json:"item_title"`
	FromUserID   string     `json:"from_user_id"`
	ToUserID     string     `json:"to_user_id"`
	Mode         string     `json:"mode"`
	Note         string     `json:"note"`
	State        string     `json:"state"`
	ResponseNote string     `json:"response_note"`
	RespondedAt  *time.Time `json:"responded_at"`
	CreatedAt    time.Time  `json:"created_at"`
	// ExpiresAt is set only on an offer holding a departed owner's item.
	ExpiresAt *time.Time `json:"expires_at"`
}

// Send describes one send.
type Send struct {
	To   string `json:"to"`
	Mode string `json:"mode"`
	Note string `json:"note"`
	// ClientRef makes a retry return the first send. Without it, a retry after a
	// successful assignment fails, because the sender no longer owns the item.
	ClientRef string `json:"client_ref"`
}

func (s *Send) normalize() error {
	if s.Mode == "" {
		s.Mode = ModeTransfer
	}
	if (s.Mode != ModeTransfer && s.Mode != ModeShare) || utf8.RuneCountInString(s.Note) > maxNote {
		return ErrInvalid
	}
	return nil
}

// Service manages offers.
type Service struct {
	pool  *pgxpool.Pool
	org   Org
	items *workitems.Service
}

// NewService builds a Service.
func NewService(pool *pgxpool.Pool, org Org, items *workitems.Service) *Service {
	return &Service{pool: pool, org: org, items: items}
}

// Plan is what the org says about a send: whether the recipient is valid, and
// whether the sender outranks them. Reads are fresh, and any failure fails the
// send, because a send to the wrong person can't be undone.
type Plan struct {
	assign bool
	// redirectedFrom is the agent account a looping send was meant for, when
	// it went to the sender's manager instead.
	redirectedFrom string
}

// Plan makes the org reads for a send. It runs before any transaction, so no row
// stays locked while it waits on the network.
func (s *Service) Plan(ctx context.Context, c policy.Caller, send *Send) (Plan, error) {
	if err := send.normalize(); err != nil {
		return Plan{}, err
	}
	if send.To == c.UserID {
		return Plan{}, workitems.ErrBadPerson
	}
	// The token is up to 5 minutes old, and an assignment needs no acceptance, so
	// a removed manager must be caught here.
	sender, err := s.org.Member(ctx, c.TenantID, c.UserID)
	if errors.Is(err, people.ErrNotFound) || (err == nil && !sender.Active()) {
		return Plan{}, policy.ErrForbidden
	}
	if err != nil {
		return Plan{}, err
	}
	recipient, err := s.org.Member(ctx, c.TenantID, send.To)
	if errors.Is(err, people.ErrNotFound) || (err == nil && !recipient.Active()) {
		return Plan{}, workitems.ErrBadPerson
	}
	if err != nil {
		return Plan{}, err
	}
	reports, err := s.org.Reports(ctx, c.TenantID, c.UserID)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{assign: slices.Contains(reports, send.To)}
	if recipient.Agent() && !plan.assign {
		if err := s.accepts(ctx, c, sender, send.To); err != nil {
			return Plan{}, err
		}
	}
	return plan, nil
}

// accepts checks an agent account's intake: who it takes offers from. The org
// reads are fresh, as every read a send depends on is.
func (s *Service) accepts(ctx context.Context, c policy.Caller, sender people.Member, to string) error {
	var everyone bool
	var units, lists []string
	err := s.pool.QueryRow(ctx, "SELECT everyone, units, lists FROM collab.agent_intake WHERE tenant_id = $1 AND user_id = $2",
		c.TenantID, to).Scan(&everyone, &units, &lists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotAccepting
	}
	if err != nil {
		return fmt.Errorf("read intake: %w", err)
	}
	if everyone {
		return nil
	}
	if len(lists) > 0 {
		var member bool
		err := s.pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM collab.list_members WHERE tenant_id = $1 AND list_id = ANY($2) AND user_id = $3)",
			c.TenantID, lists, c.UserID).Scan(&member)
		if err != nil {
			return fmt.Errorf("read list members: %w", err)
		}
		if member {
			return nil
		}
	}
	if len(units) > 0 && sender.OrgUnitID != "" {
		tree, err := s.org.Units(ctx, c.TenantID)
		if err != nil {
			return err
		}
		parent := map[string]string{}
		for _, u := range tree {
			parent[u.ID] = u.ParentID
		}
		// The sender's unit or any unit above it, so a listed department takes
		// work from its teams. The bound guards against a malformed tree.
		for unit, n := sender.OrgUnitID, 0; unit != "" && n <= len(tree); unit, n = parent[unit], n+1 {
			if slices.Contains(units, unit) {
				return nil
			}
		}
	}
	return ErrNotAccepting
}

// loop redirects a send between agent accounts that would pass an item on
// MaxAgentHops times with no person acting: it goes to the sending agent
// account's manager instead, who answers for its work.
// It returns the agent account the send was meant for when it redirects one.
func (s *Service) loop(ctx context.Context, c policy.Caller, itemID string, send *Send) (string, error) {
	if !people.IsAgentID(c.UserID) || !people.IsAgentID(send.To) {
		return "", nil
	}
	hops, _, err := activity.AgentHops(ctx, s.pool, c.TenantID, itemID)
	if err != nil {
		return "", err
	}
	if hops+1 < MaxAgentHops {
		return "", nil
	}
	sender, err := s.org.Member(ctx, c.TenantID, c.UserID)
	if err != nil {
		return "", err
	}
	if sender.ManagerUserID == "" {
		return "", ErrNoManager
	}
	meant := send.To
	send.To = sender.ManagerUserID
	return meant, nil
}

// Send sends an existing item.
func (s *Service) Send(ctx context.Context, c policy.Caller, itemID string, send Send) (Offer, error) {
	if send.ClientRef != "" {
		offer, err := s.byRef(ctx, c, send.ClientRef)
		if err == nil {
			if offer.ItemID != itemID || offer.ToUserID != send.To {
				return Offer{}, ErrRefReused
			}
			return offer, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Offer{}, err
		}
	}
	item, relation, err := s.items.Read(ctx, c, itemID)
	if err != nil {
		return Offer{}, err
	}
	if !relation.CanSend() {
		return Offer{}, policy.ErrForbidden
	}
	if item.Closed() {
		return Offer{}, ErrItemClosed
	}
	redirectedFrom, err := s.loop(ctx, c, itemID, &send)
	if err != nil {
		return Offer{}, err
	}
	plan, err := s.Plan(ctx, c, &send)
	if err != nil {
		return Offer{}, err
	}
	plan.redirectedFrom = redirectedFrom
	var offer Offer
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		offer, err = s.SendIn(ctx, tx, c, plan, itemID, send)
		return err
	})
	if err != nil {
		return Offer{}, err
	}
	return offer, nil
}

// SendIn records a planned send inside tx. It rechecks the item under its lock,
// because an owner can change while the org reads run.
func (s *Service) SendIn(ctx context.Context, tx pgx.Tx, c policy.Caller, plan Plan, itemID string, send Send) (Offer, error) {
	item, relation, err := workitems.Lock(ctx, tx, c, itemID)
	if err != nil {
		return Offer{}, err
	}
	if !relation.CanSend() {
		return Offer{}, policy.ErrForbidden
	}
	if item.Closed() {
		return Offer{}, ErrItemClosed
	}
	if slices.Contains(item.Owners, send.To) {
		return Offer{}, ErrAlreadyOwner
	}
	var sent int
	err = tx.QueryRow(ctx,
		"SELECT count(*) FROM collab.offers WHERE tenant_id = $1 AND from_user_id = $2 AND created_at > now() - interval '1 hour'",
		c.TenantID, c.UserID).Scan(&sent)
	if err != nil {
		return Offer{}, fmt.Errorf("count sends: %w", err)
	}
	if sent >= sendsPerHour || (people.IsAgentID(c.UserID) && sent >= agentSendsPerHour) {
		return Offer{}, ErrRateLimited
	}

	offer := Offer{
		ID: ids.New("ofr"), ItemID: itemID, ItemTitle: item.Title, FromUserID: c.UserID, ToUserID: send.To,
		Mode: send.Mode, Note: send.Note, State: StatePending,
	}
	if plan.assign {
		offer.State = StateAssigned
	}
	var ref *string
	if send.ClientRef != "" {
		ref = &send.ClientRef
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO collab.offers (tenant_id, id, item_id, from_user_id, to_user_id, mode, note, state, client_ref)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING created_at`,
		c.TenantID, offer.ID, itemID, c.UserID, send.To, send.Mode, send.Note, offer.State, ref).Scan(&offer.CreatedAt)
	if db.IsUniqueViolation(err, "offers_one_per_recipient") || db.IsUniqueViolation(err, "offers_one_transfer_per_sender") {
		return Offer{}, ErrOfferExists
	}
	if db.IsUniqueViolation(err, "offers_client_ref") {
		return Offer{}, ErrRefReused
	}
	if err != nil {
		return Offer{}, fmt.Errorf("insert offer: %w", err)
	}

	detail := map[string]any{"offer_id": offer.ID, "to": send.To, "mode": send.Mode, "note": send.Note}
	if plan.redirectedFrom != "" {
		detail["redirected_from"], detail["reason"] = plan.redirectedFrom, "agent_loop"
	}
	// A send answers any suggestion, whoever it went to.
	if _, err := tx.Exec(ctx,
		`UPDATE collab.work_items SET suggested_to = NULL, suggested_note = '', suggested_at = NULL
		 WHERE tenant_id = $1 AND id = $2 AND suggested_to IS NOT NULL`, c.TenantID, itemID); err != nil {
		return Offer{}, fmt.Errorf("clear suggestion: %w", err)
	}
	if !plan.assign {
		err := activity.Record(ctx, tx, c, activity.Change{ItemID: itemID, Kind: "offer.sent", Detail: detail, Notify: []string{send.To}})
		return offer, err
	}
	if err := changeOwners(ctx, tx, c, offer); err != nil {
		return Offer{}, err
	}
	notify, err := activity.PeopleOn(ctx, tx, c.TenantID, itemID)
	if err != nil {
		return Offer{}, err
	}
	return offer, activity.Record(ctx, tx, c, activity.Change{ItemID: itemID, Kind: "offer.assigned", Detail: detail, Notify: notify})
}

// Suggest records a send an assistant drafted, for an owner to make with one
// click. An assistant acting for a person never sends. It makes the same org
// checks as a send, so a suggestion that couldn't be sent is refused now, not
// when the person clicks. Nobody is told: the owner sees it on the item.
func (s *Service) Suggest(ctx context.Context, c policy.Caller, itemID string, send Send) (workitems.Item, error) {
	send.Mode = ModeTransfer
	if _, err := s.Plan(ctx, c, &send); err != nil {
		return workitems.Item{}, err
	}
	var item workitems.Item
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		locked, relation, err := workitems.Lock(ctx, tx, c, itemID)
		if err != nil {
			return err
		}
		if !relation.CanSend() {
			return policy.ErrForbidden
		}
		if locked.Closed() {
			return ErrItemClosed
		}
		if slices.Contains(locked.Owners, send.To) {
			return ErrAlreadyOwner
		}
		if _, err := tx.Exec(ctx,
			"UPDATE collab.work_items SET suggested_to = $3, suggested_note = $4, suggested_at = now() WHERE tenant_id = $1 AND id = $2",
			c.TenantID, itemID, send.To, send.Note); err != nil {
			return fmt.Errorf("suggest: %w", err)
		}
		if err := activity.Record(ctx, tx, c, activity.Change{ItemID: itemID, Kind: "item.suggested", Detail: map[string]any{"to": send.To, "note": send.Note}}); err != nil {
			return err
		}
		item, err = workitems.LockRow(ctx, tx, c.TenantID, itemID)
		return err
	})
	return item, err
}

// Pending returns the item's offers that nobody has answered, oldest first, for
// someone who can read it.
func (s *Service) Pending(ctx context.Context, tenantID, itemID string) ([]Offer, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+offerColumns+" FROM collab.offers o WHERE o.tenant_id = $1 AND o.item_id = $2 AND o.state = 'pending' ORDER BY o.created_at, o.id",
		tenantID, itemID)
	if err != nil {
		return nil, fmt.Errorf("list pending offers: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Offer, error) { return scanOffer(row) })
}

// changeOwners moves ownership as an offer says. The caller holds the item's
// lock, and the recipient joins before the sender leaves, so an item without a
// list always keeps an owner. Offers that no longer make sense end here, or one
// could later be accepted and take the item from its new owner.
func changeOwners(ctx context.Context, tx pgx.Tx, c policy.Caller, o Offer) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO collab.work_item_owners (tenant_id, item_id, user_id, added_by) VALUES ($1, $2, $3, $4)
		 ON CONFLICT DO NOTHING`, c.TenantID, o.ItemID, o.ToUserID, o.FromUserID); err != nil {
		return fmt.Errorf("add owner: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM collab.work_item_collaborators WHERE tenant_id = $1 AND item_id = $2 AND user_id = $3",
		c.TenantID, o.ItemID, o.ToUserID); err != nil {
		return fmt.Errorf("promote collaborator: %w", err)
	}
	if err := workitems.WithdrawOffers(ctx, tx, c, o.ItemID, workitems.OfferScope{To: o.ToUserID}, "recipient_owns"); err != nil {
		return err
	}
	if o.Mode != ModeTransfer {
		return workitems.WithdrawOffers(ctx, tx, c, o.ItemID, workitems.OfferScope{NonOwners: true}, "sender_left")
	}
	if _, err := tx.Exec(ctx, "DELETE FROM collab.work_item_owners WHERE tenant_id = $1 AND item_id = $2 AND user_id = $3",
		c.TenantID, o.ItemID, o.FromUserID); err != nil {
		return fmt.Errorf("remove owner: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO collab.work_item_collaborators (tenant_id, item_id, user_id, added_by) VALUES ($1, $2, $3, $4)
		 ON CONFLICT DO NOTHING`, c.TenantID, o.ItemID, o.FromUserID, o.ToUserID); err != nil {
		return fmt.Errorf("keep sender as collaborator: %w", err)
	}
	return workitems.WithdrawOffers(ctx, tx, c, o.ItemID, workitems.OfferScope{NonOwners: true}, "sender_left")
}

const offerColumns = `o.id, o.item_id, o.from_user_id, o.to_user_id, o.mode, o.note, o.state, o.response_note, o.responded_at, o.created_at, o.expires_at`

func scanOffer(row pgx.Row, extra ...any) (Offer, error) {
	var o Offer
	dest := append([]any{&o.ID, &o.ItemID, &o.FromUserID, &o.ToUserID, &o.Mode, &o.Note, &o.State, &o.ResponseNote, &o.RespondedAt, &o.CreatedAt, &o.ExpiresAt}, extra...)
	return o, row.Scan(dest...)
}

func (s *Service) byRef(ctx context.Context, c policy.Caller, ref string) (Offer, error) {
	return scanOffer(s.pool.QueryRow(ctx,
		"SELECT "+offerColumns+" FROM collab.offers o WHERE o.tenant_id = $1 AND o.from_user_id = $2 AND o.client_ref = $3",
		c.TenantID, c.UserID, ref))
}

func read(ctx context.Context, tx pgx.Tx, tenantID, id string) (Offer, error) {
	o, err := scanOffer(tx.QueryRow(ctx, "SELECT "+offerColumns+" FROM collab.offers o WHERE o.tenant_id = $1 AND o.id = $2", tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Offer{}, policy.ErrNotFound
	}
	if err != nil {
		return Offer{}, fmt.Errorf("read offer: %w", err)
	}
	return o, nil
}

// end moves a pending offer to a final state. The state in the WHERE clause is
// what stops accepting and withdrawing at the same moment from both winning.
func end(ctx context.Context, tx pgx.Tx, tenantID string, o *Offer, state, note string) error {
	err := tx.QueryRow(ctx,
		`UPDATE collab.offers SET state = $3, response_note = $4, responded_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND state = 'pending' RETURNING state, response_note, responded_at`,
		tenantID, o.ID, state, note).Scan(&o.State, &o.ResponseNote, &o.RespondedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotPending
	}
	if err != nil {
		return fmt.Errorf("end offer: %w", err)
	}
	return nil
}

// answer loads an offer for its recipient and locks its item. Everyone else gets
// not found, so offer IDs can't be probed.
func answer(ctx context.Context, tx pgx.Tx, c policy.Caller, id string) (Offer, error) {
	o, err := read(ctx, tx, c.TenantID, id)
	if err != nil {
		return Offer{}, err
	}
	if o.ToUserID != c.UserID {
		return Offer{}, policy.ErrNotFound
	}
	if _, err := workitems.LockRow(ctx, tx, c.TenantID, o.ItemID); err != nil {
		return Offer{}, err
	}
	return o, nil
}

// Accept moves ownership to the recipient.
func (s *Service) Accept(ctx context.Context, c policy.Caller, id string) (Offer, error) {
	var o Offer
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if o, err = answer(ctx, tx, c, id); err != nil {
			return err
		}
		if err := end(ctx, tx, c.TenantID, &o, "accepted", ""); err != nil {
			return err
		}
		if err := changeOwners(ctx, tx, c, o); err != nil {
			return err
		}
		notify, err := activity.PeopleOn(ctx, tx, c.TenantID, o.ItemID)
		if err != nil {
			return err
		}
		return activity.Record(ctx, tx, c, activity.Change{
			ItemID: o.ItemID, Kind: "offer.accepted", Detail: map[string]any{"offer_id": o.ID, "mode": o.Mode}, Notify: append(notify, o.FromUserID),
		})
	})
	if err != nil {
		return Offer{}, err
	}
	return o, nil
}

// Decline leaves the owners as they are, and tells the sender why.
func (s *Service) Decline(ctx context.Context, c policy.Caller, id, note string) (Offer, error) {
	if utf8.RuneCountInString(note) > maxNote {
		return Offer{}, ErrInvalid
	}
	var o Offer
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if o, err = answer(ctx, tx, c, id); err != nil {
			return err
		}
		if err := end(ctx, tx, c.TenantID, &o, "declined", note); err != nil {
			return err
		}
		return activity.Record(ctx, tx, c, activity.Change{
			ItemID: o.ItemID, Kind: "offer.declined", Detail: map[string]any{"offer_id": o.ID, "note": note}, Notify: []string{o.FromUserID},
		})
	})
	if err != nil {
		return Offer{}, err
	}
	return o, nil
}

// OfferFromSweep sends a departed owner's item to the next person in line. It comes from
// nobody and may be declined, so it's always a share: the sweep removes the
// departed owner once someone has accepted. The caller holds the item's lock.
func OfferFromSweep(ctx context.Context, tx pgx.Tx, c policy.Caller, itemID, to string) error {
	id := ids.New("ofr")
	if _, err := tx.Exec(ctx,
		`INSERT INTO collab.offers (tenant_id, id, item_id, from_user_id, to_user_id, mode, state, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, 'pending', now() + make_interval(secs => $7))`,
		c.TenantID, id, itemID, policy.System, to, ModeShare, HoldFor.Seconds()); err != nil {
		return fmt.Errorf("insert sweep offer: %w", err)
	}
	return activity.Record(ctx, tx, c, activity.Change{
		ItemID: itemID, Kind: "offer.sent", Detail: map[string]any{"offer_id": id, "to": to, "mode": ModeShare, "reason": "owner_left"}, Notify: []string{to},
	})
}

// Hold starts the clock on pending offers that hold a departed owner's item, and
// reports whether there are any. An offer sent long before the departure still
// gets the full time. The caller holds the item's lock.
func Hold(ctx context.Context, tx pgx.Tx, tenantID, itemID string) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE collab.offers SET expires_at = COALESCE(expires_at, now() + make_interval(secs => $3))
		 WHERE tenant_id = $1 AND item_id = $2 AND state = 'pending'`, tenantID, itemID, HoldFor.Seconds())
	if err != nil {
		return false, fmt.Errorf("hold offers: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// Retire ends offers that can't be answered: those to someone who has left, and
// those past their expiry. The caller holds the item's lock.
func Retire(ctx context.Context, tx pgx.Tx, c policy.Caller, itemID string, gone []string) error {
	if err := retire(ctx, tx, c, itemID, "voided", "to_user_id = ANY($3::text[])", gone); err != nil {
		return err
	}
	return retire(ctx, tx, c, itemID, "expired", "expires_at <= now()")
}

func retire(ctx context.Context, tx pgx.Tx, c policy.Caller, itemID, state, filter string, args ...any) error {
	rows, err := tx.Query(ctx,
		`UPDATE collab.offers SET state = '`+state+`', responded_at = now()
		 WHERE tenant_id = $1 AND item_id = $2 AND state = 'pending' AND `+filter+`
		 RETURNING id, from_user_id, to_user_id`, append([]any{c.TenantID, itemID}, args...)...)
	if err != nil {
		return fmt.Errorf("end offers: %w", err)
	}
	type ended struct{ id, from, to string }
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ended, error) {
		var o ended
		return o, row.Scan(&o.id, &o.from, &o.to)
	})
	if err != nil {
		return fmt.Errorf("end offers: %w", err)
	}
	for _, o := range found {
		err := activity.Record(ctx, tx, c, activity.Change{
			ItemID: itemID, Kind: "offer." + state, Detail: map[string]any{"offer_id": o.id}, Notify: []string{o.from, o.to},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Withdraw cancels a pending offer, for its sender or any owner.
func (s *Service) Withdraw(ctx context.Context, c policy.Caller, id string) (Offer, error) {
	var o Offer
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if o, err = read(ctx, tx, c.TenantID, id); err != nil {
			return err
		}
		_, relation, err := workitems.Lock(ctx, tx, c, o.ItemID)
		if err != nil {
			return err
		}
		if !relation.CanWithdraw(o.FromUserID == c.UserID) {
			return policy.ErrForbidden
		}
		if err := end(ctx, tx, c.TenantID, &o, "withdrawn", ""); err != nil {
			return err
		}
		return activity.Record(ctx, tx, c, activity.Change{
			ItemID: o.ItemID, Kind: "offer.withdrawn", Detail: map[string]any{"offer_id": o.ID, "reason": "withdrawn"}, Notify: []string{o.ToUserID, o.FromUserID},
		})
	})
	if err != nil {
		return Offer{}, err
	}
	return o, nil
}

// List returns one page of the caller's inbox or sent offers, newest first. A
// title shows only while the caller can still see the item.
func (s *Service) List(ctx context.Context, c policy.Caller, box, cursor string, limit int) ([]Offer, string, error) {
	at, id, after, err := page.Decode(cursor)
	if err != nil {
		return nil, "", err
	}
	var filter string
	switch box {
	case BoxInbox:
		filter = "o.to_user_id = $2 AND o.state = 'pending'"
	case BoxSent:
		filter = "o.from_user_id = $2"
	default:
		return nil, "", ErrInvalid
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+offerColumns+", CASE WHEN "+policy.VisibleSQL("$1", "$2")+` THEN i.title ELSE '' END
		 FROM collab.offers o JOIN collab.work_items i ON i.tenant_id = o.tenant_id AND i.id = o.item_id
		 WHERE o.tenant_id = $1 AND `+filter+` AND (NOT $3 OR (o.created_at, o.id) < ($4, $5))
		 ORDER BY o.created_at DESC, o.id DESC LIMIT $6`,
		c.TenantID, c.UserID, after, at, id, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list offers: %w", err)
	}
	offers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Offer, error) {
		var title string
		o, err := scanOffer(row, &title)
		o.ItemTitle = title
		return o, err
	})
	if err != nil {
		return nil, "", fmt.Errorf("list offers: %w", err)
	}
	if len(offers) > limit {
		offers = offers[:limit]
		last := offers[limit-1]
		return offers, page.Encode(last.CreatedAt, last.ID), nil
	}
	return offers, "", nil
}
