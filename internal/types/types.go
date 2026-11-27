// Package types holds item types: what a module declares about the items it
// asks collab to keep, which collab checks and never runs code for. A type
// gives an item fields, the kinds of record it can be about, who may read it,
// and what happens when its owners leave. An untyped item is Task, whose rules
// are collab's own. See docs/types.md.
package types

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Task is the built-in type of every item that names none.
const Task = "collab.task"

// Visibility profiles. scoped comes with scoped entitlements.
const (
	// VisibilityTeam is collab's own read rule, managers included.
	VisibilityTeam = "team"
	// VisibilityParticipants is the creator, owners and the recipient of a
	// pending offer only: no lists, collaborators or managers.
	VisibilityParticipants = "participants"
)

// What happens to an item whose owners have all left.
const (
	// DepartureReassign offers it to its creator, then each departed owner's manager.
	DepartureReassign = "reassign"
	// DepartureCreator offers it to its creator only, and cancels it if they've left too.
	DepartureCreator = "creator"
	// DepartureCancel cancels it and tells the creator.
	DepartureCancel = "cancel"
)

// Field value kinds, as JSON Schema names them.
const (
	KindString  = "string"
	KindInteger = "integer"
	KindNumber  = "number"
	KindBoolean = "boolean"
)

const (
	maxFields      = 50
	maxFieldsBytes = 32 * 1024
	maxLabel       = 100
	maxOptions     = 100
	maxSubjects    = 20
	maxSubjectRef  = 300
	// defaultMaxLength caps a string field that names no maxLength.
	defaultMaxLength = 2000
	maxMaxLength     = 20000
)

var (
	// ErrInvalid means a definition or a value failed validation.
	ErrInvalid = errors.New("invalid value")
	// ErrUnknown means no such type is declared.
	ErrUnknown = errors.New("unknown type")
	// ErrNotOwner means the service doesn't own the type's module.
	ErrNotOwner = errors.New("this service doesn't own the type")
	// ErrStale means the type already has a later version, or this version
	// with a different definition.
	ErrStale = errors.New("version is not newer")
	// ErrFixed means a new version changed what a type promises for good.
	ErrFixed = errors.New("visibility and departure can't change")
)

var (
	typeID    = regexp.MustCompile(`^([a-z][a-z0-9]{0,30})\.[a-z][a-z0-9_]{0,62}$`)
	fieldKey  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	refKind   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}:[a-z][a-z0-9_]{0,62}$`)
	subjectID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,199}$`)
)

// Field describes one field: a subset of JSON Schema that any client can draw.
type Field struct {
	Type      string `json:"type"`
	Title     string `json:"title,omitempty"`
	MaxLength int    `json:"maxLength,omitempty"`
	Enum      []any  `json:"enum,omitempty"`
}

// Fields is a type's fields, in the order clients show them.
type Fields struct {
	Type       string           `json:"type"`
	Properties map[string]Field `json:"properties"`
	// Order lists the keys in the order they were declared.
	Order []string `json:"order"`
}

// UnmarshalJSON keeps the order the keys were declared in, since JSON objects
// have none and clients show fields in a type's order.
func (f *Fields) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
		Order      []string        `json:"order"`
	}
	if err := strictly(data, &raw); err != nil {
		return err
	}
	f.Type, f.Order, f.Properties = raw.Type, raw.Order, map[string]Field{}
	if len(raw.Properties) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw.Properties))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return fmt.Errorf("%w: properties must be an object", ErrInvalid)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key := tok.(string)
		var field Field
		var one json.RawMessage
		if err := dec.Decode(&one); err != nil {
			return err
		}
		if err := strictly(one, &field); err != nil {
			return fmt.Errorf("field %s: %w", key, err)
		}
		if _, dup := f.Properties[key]; dup {
			return fmt.Errorf("%w: field %s is declared twice", ErrInvalid, key)
		}
		f.Properties[key] = field
		keys = append(keys, key)
	}
	if f.Order == nil {
		f.Order = keys
	}
	return nil
}

// Definition is a type as its module declares it.
type Definition struct {
	ID          string `json:"id"`
	OwnerModule string `json:"owner_module"`
	Version     int    `json:"version"`
	Label       string `json:"label"`
	Fields      Fields `json:"fields"`
	// Subject lists the kinds of record, <module>:<type>, an item can be about.
	Subject    []string `json:"subject"`
	Visibility string   `json:"visibility"`
	Departure  string   `json:"departure"`
	// States, when declared, give the type its own lifecycle; see states.go.
	States map[string]State `json:"states,omitempty"`
	// StateOrder lists the states in the order they were declared.
	StateOrder []string `json:"state_order,omitempty"`
	Initial    string   `json:"initial,omitempty"`
	// Cancelled is the state collab moves an item to when it cancels it, as
	// when its owners leave.
	Cancelled   string       `json:"cancelled,omitempty"`
	Offers      *Offers      `json:"offers,omitempty"`
	Transitions []Transition `json:"transitions,omitempty"`
}

// Type is a definition as collab holds it.
type Type struct {
	Definition
	UpdatedAt time.Time `json:"updated_at"`
}

// TaskType is the built-in type. Its subject can be any record, because a
// reference grants nothing.
var TaskType = Type{Definition: Definition{
	ID: Task, OwnerModule: "collab", Version: 1, Label: "Task",
	Fields:     Fields{Type: "object", Properties: map[string]Field{}, Order: []string{}},
	Subject:    []string{},
	Visibility: VisibilityTeam, Departure: DepartureReassign,
}}

// strictly decodes JSON and refuses keys it doesn't know, so a module learns at
// once that a part of its type isn't supported.
func strictly(data []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

// Parse reads and checks a definition.
func Parse(data []byte) (Definition, error) {
	var d Definition
	if err := json.Unmarshal(data, &d); err != nil {
		return Definition{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return d, d.Validate()
}

// Module is the module a type ID belongs to, or empty for a malformed ID.
func Module(id string) string {
	m := typeID.FindStringSubmatch(id)
	if m == nil {
		return ""
	}
	return m[1]
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

func validLabel(s string) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= maxLabel
}

// Validate checks a definition on its own.
func (d *Definition) Validate() error {
	module := Module(d.ID)
	switch {
	case module == "" || module == "collab":
		return invalid("id must be <module>.<name>, and not collab's")
	case d.OwnerModule != module:
		return invalid("owner_module must be the id's module")
	case d.Version < 1:
		return invalid("version must be 1 or more")
	case !validLabel(d.Label):
		return invalid("label must be 1 to %d characters", maxLabel)
	}
	if d.Visibility == "" {
		d.Visibility = VisibilityTeam
	}
	if d.Departure == "" {
		d.Departure = DepartureReassign
		if d.Visibility == VisibilityParticipants {
			d.Departure = DepartureCreator
		}
	}
	switch {
	case d.Visibility != VisibilityTeam && d.Visibility != VisibilityParticipants:
		return invalid("visibility must be team or participants")
	case !slices.Contains([]string{DepartureReassign, DepartureCreator, DepartureCancel}, d.Departure):
		return invalid("departure must be reassign, creator or cancel")
	case d.Departure == DepartureReassign && d.Visibility == VisibilityParticipants:
		// Offering the item to a departed owner's manager would show it to them.
		return invalid("a participants type can't reassign to managers")
	}
	if d.Subject == nil {
		d.Subject = []string{}
	}
	if len(d.Subject) > maxSubjects {
		return invalid("at most %d subject kinds", maxSubjects)
	}
	for _, kind := range d.Subject {
		if !refKind.MatchString(kind) {
			return invalid("subject kind %q must be <module>:<type>", kind)
		}
	}
	if err := d.Fields.validate(); err != nil {
		return err
	}
	return d.validateStates()
}

func (f *Fields) validate() error {
	if f.Type == "" {
		f.Type = "object"
	}
	if f.Properties == nil {
		f.Properties = map[string]Field{}
	}
	if f.Order == nil {
		f.Order = slices.Sorted(maps.Keys(f.Properties))
	}
	if f.Type != "object" {
		return invalid("fields must be an object")
	}
	if len(f.Properties) > maxFields {
		return invalid("at most %d fields", maxFields)
	}
	order := slices.Clone(f.Order)
	slices.Sort(order)
	if !slices.Equal(order, slices.Sorted(maps.Keys(f.Properties))) {
		return invalid("order must name each field once")
	}
	for key, field := range f.Properties {
		if !fieldKey.MatchString(key) {
			return invalid("field key %q must be lowercase letters, digits and _", key)
		}
		if field.Title != "" && !validLabel(field.Title) {
			return invalid("field %s: title must be 1 to %d characters", key, maxLabel)
		}
		if err := field.validate(); err != nil {
			return fmt.Errorf("field %s: %w", key, err)
		}
	}
	return nil
}

func (f Field) validate() error {
	switch f.Type {
	case KindString:
		if f.MaxLength < 0 || f.MaxLength > maxMaxLength {
			return invalid("maxLength must be at most %d", maxMaxLength)
		}
	case KindInteger, KindNumber, KindBoolean:
		if f.MaxLength != 0 {
			return invalid("maxLength is for strings")
		}
	default:
		return invalid("type must be string, integer, number or boolean")
	}
	if len(f.Enum) > maxOptions {
		return invalid("at most %d options", maxOptions)
	}
	for _, option := range f.Enum {
		if err := f.check(option); err != nil {
			return invalid("option %v doesn't fit the field", option)
		}
	}
	return nil
}

// check reports whether a value fits a field. JSON numbers arrive as float64.
func (f Field) check(v any) error {
	switch f.Type {
	case KindString:
		s, ok := v.(string)
		limit := f.MaxLength
		if limit == 0 {
			limit = defaultMaxLength
		}
		if !ok || utf8.RuneCountInString(s) > limit {
			return ErrInvalid
		}
	case KindInteger:
		n, ok := v.(float64)
		if !ok || n != float64(int64(n)) {
			return ErrInvalid
		}
	case KindNumber:
		if _, ok := v.(float64); !ok {
			return ErrInvalid
		}
	case KindBoolean:
		if _, ok := v.(bool); !ok {
			return ErrInvalid
		}
	}
	if len(f.Enum) > 0 && !slices.Contains(f.Enum, v) {
		return ErrInvalid
	}
	return nil
}

// Apply merges a change into an item's fields: a key set to null is removed.
// Only fields the type declares now can be written; others an item kept from
// an earlier version are shown, never changed. It returns the keys changed.
func (t Type) Apply(current, change map[string]any) (map[string]any, []string, error) {
	next := maps.Clone(current)
	if next == nil {
		next = map[string]any{}
	}
	changed := []string{}
	for _, key := range slices.Sorted(maps.Keys(change)) {
		field, ok := t.Fields.Properties[key]
		if !ok {
			return nil, nil, invalid("%s isn't a field of %s", key, t.ID)
		}
		value := change[key]
		if value == nil {
			if _, had := next[key]; had {
				delete(next, key)
				changed = append(changed, key)
			}
			continue
		}
		if err := field.check(value); err != nil {
			return nil, nil, invalid("%s doesn't fit its field", key)
		}
		if old, had := next[key]; !had || !reflect.DeepEqual(old, value) {
			next[key] = value
			changed = append(changed, key)
		}
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > maxFieldsBytes {
		return nil, nil, invalid("fields are over %d KB", maxFieldsBytes/1024)
	}
	return next, changed, nil
}

// CheckSubject checks a reference to the record an item is about, in the
// module contract's form <module>:<type>:<id>, against the kinds the type takes.
// The built-in task takes any kind.
func (t Type) CheckSubject(ref string) error {
	if ref == "" {
		return nil
	}
	kind, id, ok := cutLast(ref)
	if !ok || len(ref) > maxSubjectRef || !refKind.MatchString(kind) || !subjectID.MatchString(id) {
		return invalid("subject_ref must be <module>:<type>:<id>")
	}
	if t.ID != Task && !slices.Contains(t.Subject, kind) {
		return invalid("%s items can't be about a %s", t.ID, kind)
	}
	return nil
}

func cutLast(ref string) (string, string, bool) {
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// Store reads and writes types.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds a Store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Querier reads from the pool or from inside a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Resolve reads a type as it applies in a tenant.
func Resolve(ctx context.Context, q Querier, tenantID, id string) (Type, error) {
	if id == "" || id == Task {
		return TaskType, nil
	}
	return scanType(q.QueryRow(ctx,
		`SELECT t.definition, t.updated_at FROM collab.types t WHERE t.id = $2 AND `+applies+`
		 ORDER BY t.tenant_id DESC LIMIT 1`, tenantID, id))
}

// applies is the filter for the type rows that apply in tenant $1: the
// defaults, and a tenant version only while it tailors the current default.
const applies = `(t.tenant_id = '' OR (t.tenant_id = $1 AND t.version =
	(SELECT d.version FROM collab.types d WHERE d.tenant_id = '' AND d.id = t.id)))`

func scanType(row pgx.Row) (Type, error) {
	var t Type
	var raw []byte
	err := row.Scan(&raw, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Type{}, ErrUnknown
	}
	if err != nil {
		return Type{}, fmt.Errorf("read type: %w", err)
	}
	if err := json.Unmarshal(raw, &t.Definition); err != nil {
		return Type{}, fmt.Errorf("decode type: %w", err)
	}
	return t, nil
}

// Resolve reads a type as it applies in a tenant.
func (s *Store) Resolve(ctx context.Context, tenantID, id string) (Type, error) {
	return Resolve(ctx, s.pool, tenantID, id)
}

// List returns every type that applies in a tenant, the built-in task first.
func (s *Store) List(ctx context.Context, tenantID string) ([]Type, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (t.id) t.definition, t.updated_at FROM collab.types t WHERE `+applies+`
		 ORDER BY t.id, t.tenant_id DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list types: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Type, error) { return scanType(row) })
	if err != nil {
		return nil, fmt.Errorf("list types: %w", err)
	}
	return append([]Type{TaskType}, found...), nil
}

// Put stores a type's default for its owning module, whose service must be
// t10-<module>. The same version with the same definition is a no-op, so every
// replica of a module can push at start.
func (s *Store) Put(ctx context.Context, service string, d Definition) (Type, error) {
	if err := d.Validate(); err != nil {
		return Type{}, err
	}
	if service != "t10-"+d.OwnerModule {
		return Type{}, ErrNotOwner
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return Type{}, err
	}
	var t Type
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The row lock serializes two replicas pushing at once. A first push
		// has no row to lock, so the insert's conflict check does that.
		current, err := scanType(tx.QueryRow(ctx,
			"SELECT definition, updated_at FROM collab.types WHERE tenant_id = '' AND id = $1 FOR UPDATE", d.ID))
		switch {
		case errors.Is(err, ErrUnknown):
		case err != nil:
			return err
		case current.Visibility != d.Visibility || current.Departure != d.Departure:
			return ErrFixed
		case d.Version < current.Version:
			return ErrStale
		case d.Version == current.Version:
			if !equalJSON(current.Definition, d) {
				return ErrStale
			}
			t = current
			return nil
		}
		err = tx.QueryRow(ctx,
			`INSERT INTO collab.types (tenant_id, id, module, version, definition) VALUES ('', $1, $2, $3, $4)
			 ON CONFLICT (tenant_id, id) DO UPDATE SET version = $3, definition = $4, updated_at = now()
			   WHERE collab.types.version < $3
			 RETURNING updated_at`, d.ID, d.OwnerModule, d.Version, raw).Scan(&t.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStale
		}
		if err != nil {
			return fmt.Errorf("write type: %w", err)
		}
		t.Definition = d
		return bump(ctx, tx)
	})
	return t, err
}

// bump counts a change to a type or a tenant's version of one, in its
// transaction. The count is the ontology's tenant-layer revision (Revision).
func bump(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "UPDATE collab.type_revision SET revision = revision + 1"); err != nil {
		return fmt.Errorf("count type change: %w", err)
	}
	return nil
}

// Revision is the count of changes to every type and tenant version. It only
// grows, so a client that cached the types it read knows when to read again.
func (s *Store) Revision(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, "SELECT revision FROM collab.type_revision").Scan(&n); err != nil {
		return 0, fmt.Errorf("read type revision: %w", err)
	}
	return n, nil
}

func equalJSON(a, b Definition) bool {
	x, errX := json.Marshal(a)
	y, errY := json.Marshal(b)
	return errX == nil && errY == nil && bytes.Equal(x, y)
}
