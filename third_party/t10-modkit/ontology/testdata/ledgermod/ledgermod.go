// Package ledgermod is a small, in-memory test module shaped like a sales
// module made from t10-template: each rep's rows are private, a per-tenant pack
// is its tenant layer (stages, roles, kinds of record and person fields, some
// sensitive), and an append-only ledger of records derives each deal's stage,
// score and band, which can name their records. record.add is undoable;
// stage changes, new people and tickets wait for the rep.
//
// It names no customer and holds no real pack: tests give it made-up ones. The
// ontology package's tests run the specs' sales scenarios against it, and the
// workspace's conformance suite runs against it over HTTP (see stub/).
package ledgermod

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xiant10/t10-modkit/ontology"
	"github.com/xiant10/t10-modkit/ontology/proposals"
	"github.com/xiant10/t10-modkit/ontology/undo"
)

// Field is one person field a pack defines.
type Field struct {
	Key, Name, NameZh string
	Sensitive         bool
}

// Pack is one tenant's configuration, versioned.
type Pack struct {
	Version     int64
	Stages      []string
	Roles       []string
	Kinds       []string
	Fields      []Field
	ManagerSees string // "bands": a manager sees bands, never scores or notes
}

// Caller is a person in a tenant: a rep, or a manager who sees reps' deals as
// the pack allows.
type Caller struct {
	Tenant, User string
	Manager      bool
	Perms        []string
}

// Rep and Manager build callers with the permissions a pack's roles give.
func Rep(tenant, user string) Caller {
	return Caller{Tenant: tenant, User: user, Perms: []string{"sales:record.add", "sales:deal.update", "sales:stakeholder.create", "sales:ticket.send"}}
}

func Manager(tenant, user string) Caller {
	return Caller{Tenant: tenant, User: user, Manager: true, Perms: []string{"sales:deal.discount"}}
}

type Account struct {
	ID, Tenant, Owner, Name string
	Revision                int64
}

type Stakeholder struct {
	ID, Tenant, Owner, Account, Name, Phone string
	Fields                                  map[string]string
	Revision                                int64
}

type Deal struct {
	ID, Tenant, Owner, Account, Name, Notes string
	Amount                                  int64
	Revision                                int64
}

// Record is one ledger fact. Kind "stage" moves a deal's stage, and kind
// "undo" compensates the record it Undoes. Records are never edited.
type Record struct {
	ID, Tenant, Owner, Deal, Kind, Summary string
	Stage, Because, Undoes, Via            string
	At                                     time.Time
}

type Item struct {
	ID, Tenant, Owner, Deal, Title string
	Deadline                       time.Time
}

type Ticket struct {
	ID, Tenant, Owner, Deal, Title string
}

type stake struct{ deal, stakeholder, role string }

// Module is the test module's data and its registry.
type Module struct {
	mu           sync.Mutex
	seq          int
	packs        map[string]Pack
	accounts     map[string]*Account
	stakeholders map[string]*Stakeholder
	deals        map[string]*Deal
	records      []*Record
	items        []*Item
	tickets      []*Ticket
	stakes       []stake
	keys         map[string]string // idempotency keys already applied, to the record written
	now          func() time.Time

	Registry *ontology.Registry[Caller]
}

// New builds the module with no data, keeping drafts and undo entries in
// memory. now is its clock.
func New(now func() time.Time) *Module {
	return NewWith(now, &proposals.Memory{}, &undo.Memory{})
}

// NewWith builds the module over other stores, such as Postgres ones.
func NewWith(now func() time.Time, ps ontology.ProposalStore, us ontology.UndoStore) *Module {
	m := &Module{packs: map[string]Pack{}, accounts: map[string]*Account{}, stakeholders: map[string]*Stakeholder{}, deals: map[string]*Deal{},
		keys: map[string]string{}, now: now}
	m.Registry = ontology.New(ontology.Options[Caller]{
		Module: "sales", Prefix: "sales", Version: 1,
		Restricted:   []string{"deal.set_discount"},
		Authenticate: Authenticate,
		Who:          func(c Caller) ontology.Who { return ontology.Who{Tenant: c.Tenant, User: c.User} },
		Can:          func(c Caller, p string) bool { return slices.Contains(c.Perms, p) },
		Tenant:       m.tenant,
		Proposals:    ps, Undo: us, Now: now,
	})
	m.declare()
	return m
}

// Authenticate reads the stub's test tokens: "Bearer person:<tenant>:<user>"
// for a rep, "person:<tenant>:<user>:manager" for a manager, and
// "agent:<tenant>:<user>" for a delegated token.
func Authenticate(r *http.Request) (Caller, ontology.Token, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(raw, ":")
	if !ok || len(parts) < 3 || (parts[0] != "person" && parts[0] != "agent") {
		return Caller{}, 0, fmt.Errorf("no token")
	}
	c := Rep(parts[1], parts[2])
	if len(parts) == 4 && parts[3] == "manager" {
		c = Manager(parts[1], parts[2])
	}
	if parts[0] == "agent" {
		return c, ontology.DelegatedToken, nil
	}
	return c, ontology.PersonToken, nil
}

func (m *Module) id(prefix string) string {
	m.seq++
	return fmt.Sprintf("%s_%04d", prefix, m.seq)
}

// SetPack publishes a tenant's pack.
func (m *Module) SetPack(tenant string, p Pack) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.packs[tenant] = p
}

// AddAccount, AddDeal, AddStakeholder, AddItem and Link seed a rep's book.
func (m *Module) AddAccount(c Caller, name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := &Account{ID: m.id("acc"), Tenant: c.Tenant, Owner: c.User, Name: name, Revision: 1}
	m.accounts[a.ID] = a
	return a.ID
}

func (m *Module) AddDeal(c Caller, account, name string, amount int64, notes string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := &Deal{ID: m.id("deal"), Tenant: c.Tenant, Owner: c.User, Account: account, Name: name, Amount: amount, Notes: notes, Revision: 1}
	m.deals[d.ID] = d
	return d.ID
}

func (m *Module) AddStakeholder(c Caller, account, name, phone string, fields map[string]string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &Stakeholder{ID: m.id("sh"), Tenant: c.Tenant, Owner: c.User, Account: account, Name: name, Phone: phone, Fields: fields, Revision: 1}
	m.stakeholders[s.ID] = s
	return s.ID
}

func (m *Module) AddItem(c Caller, deal, title string, deadline time.Time) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := &Item{ID: m.id("item"), Tenant: c.Tenant, Owner: c.User, Deal: deal, Title: title, Deadline: deadline}
	m.items = append(m.items, it)
	return it.ID
}

func (m *Module) Link(deal, stakeholder, role string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stakes = append(m.stakes, stake{deal, stakeholder, role})
}

// Reassign moves a deal to another rep, as a manager's reassignment would.
func (m *Module) Reassign(deal, user string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deals[deal].Owner = user
}

// Records are the ledger, in the order written, for tests.
func (m *Module) Records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Record, len(m.records))
	for i, r := range m.records {
		out[i] = *r
	}
	return out
}

func (m *Module) tenant(_ context.Context, c Caller) (ontology.Tenant, error) {
	m.mu.Lock()
	p, ok := m.packs[c.Tenant]
	m.mu.Unlock()
	if !ok {
		return ontology.Tenant{}, nil
	}
	fields := map[string]ontology.Property{}
	for _, f := range p.Fields {
		prop := ontology.Property{Type: ontology.Text, Name: f.Name, NameZh: f.NameZh}
		if f.Sensitive {
			prop.Sensitivity = ontology.Personal
		}
		fields[f.Key] = prop
	}
	return ontology.Tenant{
		Revision: p.Version,
		ObjectTypes: map[string]ontology.TenantType{
			"deal": {Values: map[string][]string{"stage": p.Stages},
				Links: map[string]ontology.TenantLink{"stakeholders": {Values: map[string][]string{"role": p.Roles}}}},
			"record":      {Values: map[string][]string{"kind": p.Kinds}},
			"stakeholder": {Properties: fields},
		},
		ActionTypes: map[string]ontology.TenantAction{"record.add": {Values: map[string][]string{"kind": p.Kinds}}},
	}, nil
}

func (m *Module) pack(tenant string) Pack { return m.packs[tenant] }

// active reports whether a record still counts: not an undo, and not undone.
func (m *Module) active(r *Record) bool {
	if r.Kind == "undo" {
		return false
	}
	for _, o := range m.records {
		if o.Kind == "undo" && o.Undoes == r.ID {
			return false
		}
	}
	return true
}

// derived computes a deal's stage, score and band from its ledger, and the
// records behind each, newest first.
func (m *Module) derived(d *Deal) (stage string, score int64, band string, stageSrc, scoreSrc []string) {
	p := m.pack(d.Tenant)
	if len(p.Stages) > 0 {
		stage = p.Stages[0]
	}
	for _, r := range slices.Backward(m.records) {
		if r.Deal != d.ID || !m.active(r) {
			continue
		}
		if r.Kind == "stage" {
			if stageSrc == nil {
				stage, stageSrc = r.Stage, []string{"sales:record:" + r.ID}
			}
			continue
		}
		score += 10
		scoreSrc = append(scoreSrc, "sales:record:"+r.ID)
	}
	switch {
	case score >= 30:
		band = "A"
	case score >= 10:
		band = "B"
	default:
		band = "C"
	}
	return stage, score, band, stageSrc, scoreSrc
}

func (m *Module) mine(c Caller, tenant, owner string) bool {
	return c.Tenant == tenant && c.User == owner
}

// dealVisible: a rep sees their own deals, a manager every deal in the tenant.
func (m *Module) dealVisible(c Caller, d *Deal) bool {
	return m.mine(c, d.Tenant, d.Owner) || (c.Manager && c.Tenant == d.Tenant)
}

func (m *Module) dealFacts(d *Deal) map[string]any {
	stage, score, band, _, _ := m.derived(d)
	return map[string]any{"name": d.Name, "account": "sales:account:" + d.Account, "amount": d.Amount, "stage": stage, "score": score, "band": band, "notes": d.Notes}
}

func (m *Module) dealView(c Caller, d *Deal) ontology.Object {
	props := m.dealFacts(d)
	if !m.mine(c, d.Tenant, d.Owner) {
		// A manager sees what the pack allows: bands, never scores or notes.
		props = map[string]any{"name": props["name"], "account": props["account"], "stage": props["stage"], "band": props["band"]}
	}
	return ontology.Object{ID: d.ID, Title: d.Name, Revision: d.Revision, Properties: props}
}

func (m *Module) stakeholderFacts(s *Stakeholder) map[string]any {
	props := map[string]any{"name": s.Name, "phone": s.Phone, "account": "sales:account:" + s.Account}
	for _, f := range m.pack(s.Tenant).Fields {
		if v, ok := s.Fields[f.Key]; ok {
			props[f.Key] = v
		}
	}
	return props
}

func (m *Module) recordFacts(r *Record) map[string]any {
	props := map[string]any{"summary": r.Summary, "kind": r.Kind, "deal": "sales:deal:" + r.Deal, "at": r.At.UTC().Format(time.RFC3339)}
	if r.Undoes != "" {
		props["undoes"] = "sales:record:" + r.Undoes
	}
	return props
}

// search runs a query over rows in memory: the caller's scope first, then the
// filter, then sort and page.
func search[R any](rows []R, id func(R) string, facts func(R) map[string]any, q ontology.Query) []R {
	var out []R
	for _, r := range rows {
		if q.Filter == nil || match(*q.Filter, facts(r)) {
			out = append(out, r)
		}
	}
	key := func(r R) any {
		if q.Sort.Property == "" {
			return nil
		}
		return facts(r)[q.Sort.Property]
	}
	order := func(a, b R) int {
		c := compare(key(a), key(b))
		if c == 0 {
			c = cmp.Compare(id(a), id(b))
		}
		if q.Sort.Desc {
			c = -c
		}
		return c
	}
	slices.SortFunc(out, order)
	if q.After != nil {
		i := 0
		for i < len(out) {
			c := compare(key(out[i]), q.After.Value)
			if c == 0 {
				c = cmp.Compare(id(out[i]), q.After.ID)
			}
			if q.Sort.Desc {
				c = -c
			}
			if c > 0 {
				break
			}
			i++
		}
		out = out[i:]
	}
	if len(out) > q.Limit+1 {
		out = out[:q.Limit+1]
	}
	return out
}

func match(f ontology.Filter, facts map[string]any) bool {
	if f.And != nil {
		for _, sub := range f.And {
			if !match(sub, facts) {
				return false
			}
		}
		return true
	}
	v := facts[f.Property]
	switch f.Op {
	case ontology.Eq:
		return compare(v, f.Value) == 0
	case ontology.In:
		return slices.ContainsFunc(f.Value.([]any), func(x any) bool { return compare(v, x) == 0 })
	case ontology.Lt:
		return v != nil && compare(v, f.Value) < 0
	case ontology.Gt:
		return v != nil && compare(v, f.Value) > 0
	case ontology.Contains:
		s, _ := v.(string)
		return strings.Contains(s, f.Value.(string))
	}
	return false
}

func compare(a, b any) int {
	switch x := a.(type) {
	case int64:
		y, _ := b.(int64)
		return cmp.Compare(x, y)
	case time.Time:
		y, _ := b.(time.Time)
		return x.Compare(y)
	case string:
		if y, ok := b.(time.Time); ok {
			t, _ := time.Parse(time.RFC3339, x)
			return t.Compare(y)
		}
		y, _ := b.(string)
		return cmp.Compare(x, y)
	case bool:
		y, _ := b.(bool)
		if x == y {
			return 0
		}
		if !x {
			return -1
		}
		return 1
	case nil:
		if b == nil {
			return 0
		}
		return -1
	}
	return 0
}

// page cuts a link's rows after the ID p.After, keeping one more than p.Limit.
func page(rows []ontology.LinkRow, p ontology.Page) []ontology.LinkRow {
	if p.After != "" {
		i := slices.IndexFunc(rows, func(r ontology.LinkRow) bool { return r.ID == p.After })
		rows = rows[i+1:]
	}
	if len(rows) > p.Limit+1 {
		rows = rows[:p.Limit+1]
	}
	return rows
}

func (m *Module) locked() func() { m.mu.Lock(); return m.mu.Unlock }

func (m *Module) declare() {
	reg := m.Registry
	ontology.AddObject(reg, ontology.ObjectType[Caller, *Account]{
		Name: "account", Label: "Account", LabelZh: "客户单位", Description: "An organisation a rep sells to. Private to its rep.",
		Title: "name", Properties: map[string]ontology.Property{"name": {Type: ontology.String, Name: "Name", NameZh: "名称"}},
		Links: map[string]ontology.Link{"deals": {To: "sales:deal", Many: true}},
		Search: func(_ context.Context, c Caller, q ontology.Query) ([]*Account, error) {
			defer m.locked()()
			var rows []*Account
			for _, a := range m.accounts {
				if m.mine(c, a.Tenant, a.Owner) {
					rows = append(rows, a)
				}
			}
			return search(rows, func(a *Account) string { return a.ID }, func(a *Account) map[string]any { return map[string]any{"name": a.Name} }, q), nil
		},
		Get: func(_ context.Context, c Caller, id string) (*Account, error) {
			defer m.locked()()
			a, ok := m.accounts[id]
			if !ok || !m.mine(c, a.Tenant, a.Owner) {
				return nil, ontology.ErrNotFound
			}
			return a, nil
		},
		View: func(_ context.Context, _ Caller, a *Account) ontology.Object {
			return ontology.Object{ID: a.ID, Title: a.Name, Revision: a.Revision, Properties: map[string]any{"name": a.Name}}
		},
		Follow: func(_ context.Context, c Caller, a *Account, _ string, p ontology.Page) ([]ontology.LinkRow, error) {
			defer m.locked()()
			var rows []ontology.LinkRow
			for _, d := range m.deals {
				if d.Account == a.ID {
					rows = append(rows, ontology.LinkRow{ID: d.ID})
				}
			}
			slices.SortFunc(rows, func(x, y ontology.LinkRow) int { return cmp.Compare(x.ID, y.ID) })
			return page(rows, p), nil
		},
	})
	ontology.AddObject(reg, ontology.ObjectType[Caller, *Stakeholder]{
		Name: "stakeholder", Label: "Stakeholder", LabelZh: "关键人", Description: "A person at an account who matters to a deal.",
		Title: "name", Implements: []string{"t10:Person"},
		Properties: map[string]ontology.Property{
			"name":    {Type: ontology.String, Name: "Name", NameZh: "姓名"},
			"phone":   {Type: ontology.String, Sensitivity: ontology.Contact},
			"account": {Type: ontology.Ref, To: "sales:account"},
		},
		Search: func(_ context.Context, c Caller, q ontology.Query) ([]*Stakeholder, error) {
			defer m.locked()()
			var rows []*Stakeholder
			for _, s := range m.stakeholders {
				if m.mine(c, s.Tenant, s.Owner) {
					rows = append(rows, s)
				}
			}
			return search(rows, func(s *Stakeholder) string { return s.ID }, m.stakeholderFacts, q), nil
		},
		Get: func(_ context.Context, c Caller, id string) (*Stakeholder, error) {
			defer m.locked()()
			s, ok := m.stakeholders[id]
			if !ok || !m.mine(c, s.Tenant, s.Owner) {
				return nil, ontology.ErrNotFound
			}
			return s, nil
		},
		View: func(_ context.Context, _ Caller, s *Stakeholder) ontology.Object {
			defer m.locked()()
			return ontology.Object{ID: s.ID, Title: s.Name, Revision: s.Revision, Properties: m.stakeholderFacts(s)}
		},
	})
	ontology.AddObject(reg, ontology.ObjectType[Caller, *Deal]{
		Name: "deal", Label: "Deal", LabelZh: "商机项目", Description: "One opportunity at an account, scored from its ledger.",
		Title: "name",
		Properties: map[string]ontology.Property{
			"name":    {Type: ontology.String},
			"account": {Type: ontology.Ref, To: "sales:account"},
			"amount":  {Type: ontology.MoneyFen},
			"stage":   {Type: ontology.Enum, Derived: true, Sources: true},
			"score":   {Type: ontology.Integer, Derived: true, Sources: true},
			"band":    {Type: ontology.Enum, Values: []string{"A", "B", "C"}, Derived: true, Sources: true},
			"notes":   {Type: ontology.Text},
		},
		Links: map[string]ontology.Link{
			"stakeholders": {To: "sales:stakeholder", Many: true, Properties: map[string]ontology.Property{"role": {Type: ontology.Enum}}},
			"records":      {To: "sales:record", Many: true},
			"items":        {To: "sales:item", Many: true},
			"tickets":      {To: "sales:ticket", Many: true},
		},
		Search: func(_ context.Context, c Caller, q ontology.Query) ([]*Deal, error) {
			defer m.locked()()
			if c.Manager && q.Filter != nil && touches(*q.Filter, "score", "notes", "amount") || c.Manager && slices.Contains([]string{"score", "notes", "amount"}, q.Sort.Property) {
				return nil, ontology.ErrFilterNotAllowed
			}
			var rows []*Deal
			for _, d := range m.deals {
				if m.dealVisible(c, d) {
					rows = append(rows, d)
				}
			}
			return search(rows, func(d *Deal) string { return d.ID }, m.dealFacts, q), nil
		},
		Get: func(_ context.Context, c Caller, id string) (*Deal, error) {
			defer m.locked()()
			d, ok := m.deals[id]
			if !ok || !m.dealVisible(c, d) {
				return nil, ontology.ErrNotFound
			}
			return d, nil
		},
		View: func(_ context.Context, c Caller, d *Deal) ontology.Object {
			defer m.locked()()
			return m.dealView(c, d)
		},
		Follow: func(_ context.Context, c Caller, d *Deal, link string, p ontology.Page) ([]ontology.LinkRow, error) {
			defer m.locked()()
			var rows []ontology.LinkRow
			switch link {
			case "stakeholders":
				for _, s := range m.stakes {
					if s.deal == d.ID {
						rows = append(rows, ontology.LinkRow{ID: s.stakeholder, Link: map[string]any{"role": s.role}})
					}
				}
			case "records":
				for _, r := range m.records {
					if r.Deal == d.ID {
						rows = append(rows, ontology.LinkRow{ID: r.ID})
					}
				}
			case "items":
				for _, it := range m.items {
					if it.Deal == d.ID {
						rows = append(rows, ontology.LinkRow{ID: it.ID})
					}
				}
			case "tickets":
				for _, t := range m.tickets {
					if t.Deal == d.ID {
						rows = append(rows, ontology.LinkRow{ID: t.ID})
					}
				}
			}
			return page(rows, p), nil
		},
		Sources: func(_ context.Context, c Caller, d *Deal, prop string) ([]string, error) {
			defer m.locked()()
			if !m.mine(c, d.Tenant, d.Owner) {
				// A manager sees bands, but not the rep's records behind them.
				return []string{}, nil
			}
			_, _, _, stageSrc, scoreSrc := m.derived(d)
			if prop == "stage" {
				return stageSrc, nil
			}
			return scoreSrc, nil
		},
	})
	ontology.AddObject(reg, ontology.ObjectType[Caller, *Record]{
		Name: "record", Label: "Record", LabelZh: "行动记录", Description: "One fact in the ledger. Never edited; an undo writes a compensating record.",
		Title: "summary",
		Properties: map[string]ontology.Property{
			"summary": {Type: ontology.Text},
			"kind":    {Type: ontology.Enum, Values: []string{"stage", "undo"}},
			"deal":    {Type: ontology.Ref, To: "sales:deal"},
			"at":      {Type: ontology.Timestamp},
			"undoes":  {Type: ontology.Ref, To: "sales:record"},
		},
		DefaultSort: ontology.Sort{Property: "at", Desc: true},
		Search: func(_ context.Context, c Caller, q ontology.Query) ([]*Record, error) {
			defer m.locked()()
			var rows []*Record
			for _, r := range m.records {
				if m.mine(c, r.Tenant, r.Owner) {
					rows = append(rows, r)
				}
			}
			return search(rows, func(r *Record) string { return r.ID }, func(r *Record) map[string]any {
				f := m.recordFacts(r)
				f["at"] = r.At
				return f
			}, q), nil
		},
		Get: func(_ context.Context, c Caller, id string) (*Record, error) {
			defer m.locked()()
			for _, r := range m.records {
				if r.ID == id && m.mine(c, r.Tenant, r.Owner) {
					return r, nil
				}
			}
			return nil, ontology.ErrNotFound
		},
		View: func(_ context.Context, _ Caller, r *Record) ontology.Object {
			defer m.locked()()
			return ontology.Object{ID: r.ID, Title: r.Summary, Revision: 1, Properties: m.recordFacts(r)}
		},
	})
	ontology.AddObject(reg, ontology.ObjectType[Caller, *Item]{
		Name: "item", Label: "Meeting or to-do", LabelZh: "日程待办", Title: "title", Implements: []string{"t10:HasDeadline"},
		Properties: map[string]ontology.Property{"title": {Type: ontology.String}, "deadline": {Type: ontology.Timestamp}},
		Search: func(_ context.Context, c Caller, q ontology.Query) ([]*Item, error) {
			defer m.locked()()
			var rows []*Item
			for _, it := range m.items {
				if m.mine(c, it.Tenant, it.Owner) {
					rows = append(rows, it)
				}
			}
			return search(rows, func(it *Item) string { return it.ID }, func(it *Item) map[string]any {
				return map[string]any{"title": it.Title, "deadline": it.Deadline}
			}, q), nil
		},
		Get: func(_ context.Context, c Caller, id string) (*Item, error) {
			defer m.locked()()
			for _, it := range m.items {
				if it.ID == id && m.mine(c, it.Tenant, it.Owner) {
					return it, nil
				}
			}
			return nil, ontology.ErrNotFound
		},
		View: func(_ context.Context, _ Caller, it *Item) ontology.Object {
			return ontology.Object{ID: it.ID, Title: it.Title, Revision: 1, Properties: map[string]any{"title": it.Title, "deadline": it.Deadline.UTC().Format(time.RFC3339)}}
		},
	})
	ontology.AddObject(reg, ontology.ObjectType[Caller, *Ticket]{
		Name: "ticket", Label: "Ticket", LabelZh: "工单", Title: "title",
		Properties: map[string]ontology.Property{"title": {Type: ontology.String}, "work_item": {Type: ontology.Ref, To: "collab:work_item"}},
		Search: func(_ context.Context, c Caller, q ontology.Query) ([]*Ticket, error) {
			defer m.locked()()
			var rows []*Ticket
			for _, t := range m.tickets {
				if m.mine(c, t.Tenant, t.Owner) {
					rows = append(rows, t)
				}
			}
			return search(rows, func(t *Ticket) string { return t.ID }, func(t *Ticket) map[string]any { return map[string]any{"title": t.Title} }, q), nil
		},
		Get: func(_ context.Context, c Caller, id string) (*Ticket, error) {
			defer m.locked()()
			for _, t := range m.tickets {
				if t.ID == id && m.mine(c, t.Tenant, t.Owner) {
					return t, nil
				}
			}
			return nil, ontology.ErrNotFound
		},
		View: func(_ context.Context, _ Caller, t *Ticket) ontology.Object {
			return ontology.Object{ID: t.ID, Title: t.Title, Revision: 1, Properties: map[string]any{"title": t.Title}}
		},
	})
	m.declareActions()
	reg.AddEvent("deal.stage_changed", "deal")
}

func touches(f ontology.Filter, props ...string) bool {
	if f.And != nil {
		return slices.ContainsFunc(f.And, func(sub ontology.Filter) bool { return touches(sub, props...) })
	}
	return slices.Contains(props, f.Property)
}

// own is the rep's own deal, which only they may write to.
func (m *Module) own(c Caller, d *Deal) string {
	if !m.mine(c, d.Tenant, d.Owner) {
		return "forbidden"
	}
	return ""
}

func (m *Module) fresh(d *Deal, revision int64) error {
	if revision != d.Revision {
		return ontology.ErrStaleRevision
	}
	return nil
}

func (m *Module) declareActions() {
	reg := m.Registry
	ontology.AddAction(reg, ontology.ActionType[Caller, *Deal]{
		Name: "record.add", Label: "Add a record", LabelZh: "记录", On: "deal",
		Description: "Write one safe fact to the deal's ledger, such as a meeting held or something learned. kind is one of the company's kinds of record.",
		Params: map[string]ontology.Param{
			"kind":    {Type: ontology.Enum, Required: true, Label: true},
			"summary": {Type: ontology.Text, Required: true},
		},
		Permission: "sales:record.add", Idempotent: true, Undo: true, UndoWindow: 24 * time.Hour, Agent: ontology.Undoable,
		Available: func(_ context.Context, c Caller, d *Deal) string { return m.own(c, d) },
		Apply: func(_ context.Context, c Caller, d *Deal, in ontology.Input) (ontology.Result, error) {
			defer m.locked()()
			if id, ok := m.keys[c.Tenant+"|"+c.User+"|"+in.IdempotencyKey]; ok {
				return ontology.Result{ID: d.ID, Undo: map[string]string{"record": id}}, nil
			}
			if err := m.fresh(d, in.Revision); err != nil {
				return ontology.Result{}, err
			}
			r := &Record{ID: m.id("rec"), Tenant: d.Tenant, Owner: d.Owner, Deal: d.ID, Kind: in.Params["kind"].(string), Summary: in.Params["summary"].(string), Via: in.Via, At: m.now()}
			m.records = append(m.records, r)
			m.keys[c.Tenant+"|"+c.User+"|"+in.IdempotencyKey] = r.ID
			d.Revision++
			return ontology.Result{ID: d.ID, Undo: map[string]string{"record": r.ID}}, nil
		},
		Revert: func(_ context.Context, c Caller, u ontology.Undoing) (ontology.Result, error) {
			defer m.locked()()
			var payload struct {
				Record string `json:"record"`
			}
			if err := json.Unmarshal(u.Payload, &payload); err != nil {
				return ontology.Result{}, err
			}
			i := slices.IndexFunc(m.records, func(r *Record) bool { return r.ID == payload.Record })
			if i < 0 {
				return ontology.Result{}, ontology.ErrNotFound
			}
			orig := m.records[i]
			for _, r := range m.records {
				if r.Because == orig.ID && m.active(r) {
					return ontology.Result{}, ontology.ErrUndoConflict
				}
			}
			m.records = append(m.records, &Record{ID: m.id("rec"), Tenant: orig.Tenant, Owner: orig.Owner, Deal: orig.Deal, Kind: "undo",
				Summary: "Undid " + orig.ID, Undoes: orig.ID, Via: "person", At: m.now()})
			m.deals[orig.Deal].Revision++
			return ontology.Result{Type: "deal", ID: orig.Deal}, nil
		},
	})
	ontology.AddAction(reg, ontology.ActionType[Caller, *Deal]{
		Name: "deal.set_stage", Label: "Change stage", LabelZh: "推进阶段", On: "deal",
		Description: "Move the deal to another of the company's stages, citing the record that justifies it. Writes a stage record; the stage is recomputed from it.",
		Params: map[string]ontology.Param{
			"to":      {Type: ontology.Enum, Required: true},
			"because": {Type: ontology.Ref, To: "sales:record"},
		},
		Permission: "sales:deal.update", Agent: ontology.Propose,
		Available: func(_ context.Context, c Caller, d *Deal) string {
			if reason := m.own(c, d); reason != "" {
				return reason
			}
			defer m.locked()()
			stages := m.pack(d.Tenant).Stages
			if stage, _, _, _, _ := m.derived(d); len(stages) > 0 && stage == stages[len(stages)-1] {
				return "wrong_state"
			}
			return ""
		},
		Validate: func(_ context.Context, _ Caller, d *Deal, in ontology.Input) []ontology.FieldError {
			defer m.locked()()
			if !slices.Contains(m.pack(d.Tenant).Stages, in.Params["to"].(string)) {
				return []ontology.FieldError{{Param: "to", Code: "not_allowed"}}
			}
			return nil
		},
		Apply: func(_ context.Context, c Caller, d *Deal, in ontology.Input) (ontology.Result, error) {
			defer m.locked()()
			if err := m.fresh(d, in.Revision); err != nil {
				return ontology.Result{}, err
			}
			because, _ := in.Params["because"].(string)
			_, _, because, _ = ontology.SplitRef(because)
			m.records = append(m.records, &Record{ID: m.id("rec"), Tenant: d.Tenant, Owner: d.Owner, Deal: d.ID, Kind: "stage", Stage: in.Params["to"].(string),
				Summary: "Stage: " + in.Params["to"].(string), Because: because, Via: in.Via, At: m.now()})
			d.Revision++
			return ontology.Result{ID: d.ID}, nil
		},
	})
	ontology.AddAction(reg, ontology.ActionType[Caller, struct{}]{
		Name: "stakeholder.create", Label: "Add a person", LabelZh: "新增关键人",
		Params: map[string]ontology.Param{
			"name":    {Type: ontology.String, Required: true},
			"account": {Type: ontology.Ref, To: "sales:account", Required: true},
		},
		Permission: "sales:stakeholder.create", Agent: ontology.Propose,
		Validate: func(_ context.Context, c Caller, _ struct{}, in ontology.Input) []ontology.FieldError {
			defer m.locked()()
			_, _, id, _ := ontology.SplitRef(in.Params["account"].(string))
			if a, ok := m.accounts[id]; !ok || !m.mine(c, a.Tenant, a.Owner) {
				return []ontology.FieldError{{Param: "account", Code: "not_found"}}
			}
			return nil
		},
		Apply: func(_ context.Context, c Caller, _ struct{}, in ontology.Input) (ontology.Result, error) {
			defer m.locked()()
			_, _, account, _ := ontology.SplitRef(in.Params["account"].(string))
			s := &Stakeholder{ID: m.id("sh"), Tenant: c.Tenant, Owner: c.User, Account: account, Name: in.Params["name"].(string), Revision: 1}
			m.stakeholders[s.ID] = s
			return ontology.Result{Type: "stakeholder", ID: s.ID}, nil
		},
	})
	ontology.AddAction(reg, ontology.ActionType[Caller, *Deal]{
		Name: "ticket.send", Label: "Send a ticket", LabelZh: "发起工单", On: "deal",
		Params:     map[string]ontology.Param{"title": {Type: ontology.String, Required: true}},
		Permission: "sales:ticket.send", Agent: ontology.Propose,
		Available: func(_ context.Context, c Caller, d *Deal) string { return m.own(c, d) },
		Apply: func(_ context.Context, c Caller, d *Deal, in ontology.Input) (ontology.Result, error) {
			defer m.locked()()
			if err := m.fresh(d, in.Revision); err != nil {
				return ontology.Result{}, err
			}
			m.tickets = append(m.tickets, &Ticket{ID: m.id("tk"), Tenant: d.Tenant, Owner: d.Owner, Deal: d.ID, Title: in.Params["title"].(string)})
			d.Revision++
			return ontology.Result{ID: d.ID}, nil
		},
	})
	ontology.AddAction(reg, ontology.ActionType[Caller, *Deal]{
		Name: "deal.set_discount", Label: "Set the discount", LabelZh: "设置折扣", On: "deal",
		Params:     map[string]ontology.Param{"percent": {Type: ontology.Decimal, Required: true}},
		Permission: "sales:deal.discount", Agent: ontology.Never,
		Apply: func(_ context.Context, _ Caller, d *Deal, in ontology.Input) (ontology.Result, error) {
			defer m.locked()()
			if err := m.fresh(d, in.Revision); err != nil {
				return ontology.Result{}, err
			}
			d.Notes += " discount " + in.Params["percent"].(string) + "%"
			d.Revision++
			return ontology.Result{ID: d.ID}, nil
		},
	})
}
