package ontology

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// Token says whose token a request carries.
type Token int

const (
	// PersonToken is a person's own token.
	PersonToken Token = iota
	// DelegatedToken is an agent's, acting for a person (an act or grant claim).
	DelegatedToken
)

// Who names the person behind a caller.
type Who struct {
	Tenant, User string
}

// Options configure a module's registry. C is the module's own caller type.
type Options[C any] struct {
	// Module is the module's ID, and Prefix its manifest's action_prefix, which
	// names its tools and grant actions.
	Module, Prefix string
	// Version is the core's version, raised whenever the core changes.
	Version int
	// Restricted lists the actions that post or change money, consent or scope.
	Restricted []string
	// Authenticate reads a request's caller and whose token it carries. Its
	// error answers as it is when it's an *Error, and 401 otherwise.
	Authenticate func(r *http.Request) (C, Token, error)
	// Who names the person a caller is.
	Who func(c C) Who
	// Can reports whether the person holds a business permission.
	Can func(c C, permission string) bool
	// Tenant returns the caller's tenant layer, if the module has one.
	Tenant func(ctx context.Context, c C) (Tenant, error)
	// Proposals and Undo store drafts, undo entries and idempotent answers, in
	// the module's own database: ontology/proposals and ontology/undo have them.
	Proposals ProposalStore
	Undo      UndoStore
	// Errors maps the module's own errors, such as its store's not-found, to the
	// contract's *Error. Without it, or when it answers another error, an error
	// that isn't an *Error answers 500 unavailable.
	Errors func(error) error
	// Now is the clock, time.Now by default.
	Now func() time.Time
}

// Registry is one module's ontology.
type Registry[C any] struct {
	opts        Options[C]
	types       map[string]*objectEntry[C]
	actions     map[string]*actionEntry[C]
	actionOrder []string
	events      map[string]Event
	errs        []string
}

// New starts a registry. Declare types and actions with AddObject and AddAction,
// then call Check.
func New[C any](o Options[C]) *Registry[C] {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Registry[C]{opts: o, types: map[string]*objectEntry[C]{}, actions: map[string]*actionEntry[C]{}, events: map[string]Event{}}
}

// Object is one record as a caller may see it, from the module's View.
type Object struct {
	ID       string
	Title    string
	Revision int64
	// Properties hold only what the caller may see, masked as the module's own
	// routes mask them, including the tenant layer's properties.
	Properties map[string]any
}

// Page is a page of a link: up to Limit rows after the row with ID After.
type Page struct {
	Limit int
	After string
}

// LinkRow is one linked object: the ID of a local object, which is then read
// through its type's Get and View, or the Ref of another module's, which is
// answered as only a reference.
type LinkRow struct {
	ID   string
	Ref  string
	Link map[string]any
}

// ObjectType declares one object type over the module's own reads. R is the
// module's record type.
type ObjectType[C, R any] struct {
	Name, Label, LabelZh, Description string
	// Title names the property that is the object's title.
	Title      string
	Implements []string
	Properties map[string]Property
	Links      map[string]Link
	// DefaultSort orders a search with no sort. Without one, objects come by ID.
	DefaultSort Sort

	// Search answers up to q.Limit+1 records in q.Sort's order after q.After,
	// with the caller's scope applied before sorting and paging. It may refuse
	// a filter it won't run with ErrFilterNotAllowed.
	Search func(ctx context.Context, c C, q Query) ([]R, error)
	// Get answers one record, and ErrNotFound for one the caller can't reach.
	Get func(ctx context.Context, c C, id string) (R, error)
	// View is the record as the caller may see it.
	View func(ctx context.Context, c C, r R) Object
	// Follow answers up to p.Limit+1 rows of a link, in a stable order, after
	// p.After.
	Follow func(ctx context.Context, c C, r R, link string, p Page) ([]LinkRow, error)
	// Sources answer the references a derived property's value came from,
	// newest first, limited to records the caller may see.
	Sources func(ctx context.Context, c C, r R, property string) ([]string, error)
}

// Input is what an action is applied with.
type Input struct {
	// Params are decoded to their types: string for string, text, enum, ref,
	// date and decimal; int64 for integer and money_fen; bool; time.Time.
	Params   map[string]any
	Revision int64
	// Via is person, agent or proposal, for the audit record.
	Via string
	// Agent names the agent for via agent or proposal, and ProposalID the
	// proposal confirmed.
	Agent, ProposalID string
	// IdempotencyKey is the request's key. An idempotent action needs one, and
	// any action given one replays its answer for it.
	IdempotencyKey string
	// AutoApply is set when an agent applied a propose action directly, because
	// the person let it act without asking, for the audit record.
	AutoApply bool
}

// Result is what an applied action changed or created.
type Result struct {
	// Type and ID name the object to answer, by default the one acted on.
	Type, ID string
	// Undo is what the action's Revert needs, for an undo action.
	Undo any
}

// Undoing is what Revert gets: the undo entry being undone.
type Undoing struct {
	ID      string
	Object  string
	Payload json.RawMessage
}

// ActionType declares one action over the module's command path. R is the
// record type of the object type it acts on; a create uses struct{}.
type ActionType[C, R any] struct {
	Name, Label, LabelZh, Description string
	// On is the object type acted on, empty for a create.
	On         string
	Params     map[string]Param
	Permission string
	Idempotent bool
	// Undo actions answer an undo ID, usable for UndoWindow.
	Undo       bool
	UndoWindow time.Duration
	Agent      Tier

	// Available answers "" when the caller may take the action on r now, or a
	// stable reason code, such as wrong_state, when not. The permission is
	// checked before it.
	Available func(ctx context.Context, c C, r R) string
	// Validate adds the module's own parameter checks to the typed ones.
	Validate func(ctx context.Context, c C, r R, in Input) []FieldError
	// Apply runs the module's command: permission, scope and revision checked
	// again, the change, its audit and side effects in one transaction.
	Apply func(ctx context.Context, c C, r R, in Input) (Result, error)
	// Revert writes the compensating change for an undo, naming the original,
	// and ErrUndoConflict when a later change depends on it. It writes it at most
	// once per Undoing.ID, and answers ErrUndone when it already has, because an
	// undo that was interrupted runs it again.
	Revert func(ctx context.Context, c C, u Undoing) (Result, error)
}

type objectEntry[C any] struct {
	info        ObjectTypeInfo
	defaultSort Sort
	search      func(context.Context, C, Query) ([]any, error)
	get         func(context.Context, C, string) (any, error)
	view        func(context.Context, C, any) Object
	follow      func(context.Context, C, any, string, Page) ([]LinkRow, error)
	sources     func(context.Context, C, any, string) ([]string, error)
	record      string // the record type's name, for AddAction's check
}

type actionEntry[C any] struct {
	info      ActionTypeInfo
	available func(context.Context, C, any) string
	validate  func(context.Context, C, any, Input) []FieldError
	apply     func(context.Context, C, any, Input) (Result, error)
	revert    func(context.Context, C, Undoing) (Result, error)
	window    time.Duration
}

func (r *Registry[C]) fail(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// AddObject declares an object type.
func AddObject[C, R any](reg *Registry[C], t ObjectType[C, R]) {
	if _, ok := reg.types[t.Name]; ok {
		reg.fail("object type %s is declared twice", t.Name)
		return
	}
	if t.Search == nil || t.Get == nil || t.View == nil {
		reg.fail("%s: needs Search, Get and View", t.Name)
		return
	}
	if len(t.Links) > 0 && t.Follow == nil {
		reg.fail("%s: declares links but no Follow", t.Name)
	}
	// A page's cursor carries the last object's sort value, so a default sort
	// on a sensitive property would hand its values to an agent.
	if s := t.DefaultSort.Property; s != "" {
		if p, ok := t.Properties[s]; !ok {
			reg.fail("%s: its default sort %q isn't one of its properties", t.Name, s)
		} else if p.Sensitivity != "" {
			reg.fail("%s: its default sort %q is sensitive, and a page's cursor carries its value", t.Name, s)
		}
	}
	for name, p := range t.Properties {
		if p.Sources && t.Sources == nil {
			reg.fail("%s.%s: has sources but the type has no Sources", t.Name, name)
		}
	}
	e := &objectEntry[C]{
		info: ObjectTypeInfo{Label: t.Label, LabelZh: t.LabelZh, Description: t.Description, Title: t.Title,
			Implements: nonNil(t.Implements), Properties: nonNilMap(t.Properties), Links: nonNilMap(t.Links)},
		defaultSort: t.DefaultSort,
		record:      fmt.Sprintf("%T", *new(R)),
		search: func(ctx context.Context, c C, q Query) ([]any, error) {
			rows, err := t.Search(ctx, c, q)
			out := make([]any, len(rows))
			for i, row := range rows {
				out[i] = row
			}
			return out, err
		},
		get:  func(ctx context.Context, c C, id string) (any, error) { return t.Get(ctx, c, id) },
		view: func(ctx context.Context, c C, rec any) Object { return t.View(ctx, c, rec.(R)) },
	}
	if t.Follow != nil {
		e.follow = func(ctx context.Context, c C, rec any, link string, p Page) ([]LinkRow, error) {
			return t.Follow(ctx, c, rec.(R), link, p)
		}
	}
	if t.Sources != nil {
		e.sources = func(ctx context.Context, c C, rec any, prop string) ([]string, error) {
			return t.Sources(ctx, c, rec.(R), prop)
		}
	}
	reg.types[t.Name] = e
}

// AddAction declares an action type. Its R must be the record type of the
// object type it acts on, declared first.
func AddAction[C, R any](reg *Registry[C], a ActionType[C, R]) {
	if _, ok := reg.actions[a.Name]; ok {
		reg.fail("action type %s is declared twice", a.Name)
		return
	}
	if a.On != "" {
		target, ok := reg.types[a.On]
		if !ok {
			reg.fail("%s: acts on %s, which must be declared first", a.Name, a.On)
			return
		}
		if record := fmt.Sprintf("%T", *new(R)); record != target.record {
			reg.fail("%s: its record type %s isn't %s's, %s", a.Name, record, a.On, target.record)
			return
		}
	}
	if a.Apply == nil {
		reg.fail("%s: needs Apply", a.Name)
		return
	}
	if a.Undo && a.Revert == nil {
		reg.fail("%s: is undo but has no Revert", a.Name)
	}
	e := &actionEntry[C]{
		info: ActionTypeInfo{Label: a.Label, LabelZh: a.LabelZh, Description: a.Description, On: a.On, Params: nonNilMap(a.Params),
			Permission: a.Permission, Idempotent: a.Idempotent, Undo: a.Undo, UndoWindow: int(a.UndoWindow / time.Second), Agent: a.Agent},
		window: a.UndoWindow,
		apply: func(ctx context.Context, c C, rec any, in Input) (Result, error) {
			typed, _ := rec.(R)
			return a.Apply(ctx, c, typed, in)
		},
		revert: a.Revert,
	}
	if a.Available != nil {
		e.available = func(ctx context.Context, c C, rec any) string { typed, _ := rec.(R); return a.Available(ctx, c, typed) }
	}
	if a.Validate != nil {
		e.validate = func(ctx context.Context, c C, rec any, in Input) []FieldError {
			typed, _ := rec.(R)
			return a.Validate(ctx, c, typed, in)
		}
	}
	reg.actions[a.Name] = e
	reg.actionOrder = append(reg.actionOrder, a.Name)
	sort.Strings(reg.actionOrder)
}

// AddEvent declares an event kind the module posts, on an object type.
func (r *Registry[C]) AddEvent(kind, on string) { r.events[kind] = Event{On: on} }

// Core is the descriptor's core, identical for every tenant.
func (r *Registry[C]) Core() Descriptor {
	d := Descriptor{Module: r.opts.Module, Version: r.opts.Version, ObjectTypes: map[string]ObjectTypeInfo{}, ActionTypes: map[string]ActionTypeInfo{},
		Restricted: nonNil(slices.Clone(r.opts.Restricted)), Interfaces: []string{}, Events: map[string]Event{}}
	for name, t := range r.types {
		d.ObjectTypes[name] = t.info
		for _, iface := range t.info.Implements {
			if !slices.Contains(d.Interfaces, iface) {
				d.Interfaces = append(d.Interfaces, iface)
			}
		}
	}
	sort.Strings(d.Interfaces)
	for name, a := range r.actions {
		d.ActionTypes[name] = a.info
	}
	for kind, e := range r.events {
		d.Events[kind] = e
	}
	return d
}

// Check reports every mistake in the declarations and the core they make. A
// module calls it at startup and in a test, so a broken ontology never serves.
func (r *Registry[C]) Check() error {
	var errs []string
	errs = append(errs, r.errs...)
	if r.opts.Prefix == "" || r.opts.Authenticate == nil || r.opts.Who == nil || r.opts.Can == nil {
		errs = append(errs, "options need Prefix, Authenticate, Who and Can")
	}
	if err := CheckCore(r.Core()); err != nil {
		errs = append(errs, err.Error())
	}
	for name, a := range r.actions {
		if a.info.Agent != Never && r.opts.Proposals == nil {
			errs = append(errs, name+": an agent may propose it, so options need Proposals")
		}
		if (a.info.Undo || a.info.Idempotent || a.info.Agent != Never) && r.opts.Undo == nil {
			errs = append(errs, name+": undo, idempotent and agent actions need options' Undo store, which keeps keys and what agents did")
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("ontology %s: %s", r.opts.Module, strings.Join(errs, "; "))
}

// Describe is the descriptor for the caller's tenant: the core and its layer,
// which is refused when it does more than add.
func (r *Registry[C]) Describe(ctx context.Context, c C) (Descriptor, error) {
	d := r.Core()
	if r.opts.Tenant == nil {
		return d, nil
	}
	t, err := r.opts.Tenant(ctx, c)
	if err != nil {
		return Descriptor{}, err
	}
	if err := CheckTenant(d, t); err != nil {
		return Descriptor{}, fmt.Errorf("tenant layer refused: %w", err)
	}
	d.Tenant = t
	return d, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func nonNilMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return map[K]V{}
	}
	return m
}
