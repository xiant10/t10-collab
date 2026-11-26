package ontology

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"time"
)

// Op is a filter condition's operator.
type Op string

const (
	Eq       Op = "eq"
	In       Op = "in"
	Lt       Op = "lt"
	Gt       Op = "gt"
	Contains Op = "contains"
)

// Filter is a typed filter tree: either And, every one of which holds, or one
// condition. Value is decoded to the property's type, as Input.Params are, and
// is a []any for In.
type Filter struct {
	And      []Filter
	Property string
	Op       Op
	Value    any
}

// Sort orders a search by one property, or by ID when Property is empty.
type Sort struct {
	Property string
	Desc     bool
}

// Position is where a page starts: after the object with this sort value and ID.
type Position struct {
	Value any
	ID    string
}

// Query is a search, validated against the type's properties. The module turns
// it into its own query, scope first.
type Query struct {
	Filter *Filter
	Sort   Sort
	Limit  int
	After  *Position
	// Agent is set for an agent's search, through the tools.
	Agent bool
}

// MaxLimit is the most objects one page holds, and DefaultLimit the number without a limit.
const (
	MaxLimit     = 100
	DefaultLimit = 50
)

type searchBody struct {
	Filter json.RawMessage `json:"filter"`
	Sort   *struct {
		Property string `json:"property"`
		Desc     bool   `json:"desc"`
	} `json:"sort"`
	Limit  *int   `json:"limit"`
	Cursor string `json:"cursor"`
}

func invalid(format string, args ...any) *Error {
	return Fail(http.StatusBadRequest, "invalid_request", fmt.Sprintf(format, args...))
}

// DecodeSearch validates a search body against a type's properties (its
// core's and tenant layer's) and returns the query. For an agent, a filter or
// sort on a property with a sensitivity is ErrFilterNotAllowed, whatever the
// person's own view, so an agent can't learn a masked value from what matches.
func DecodeSearch(raw []byte, props map[string]Property, defaultSort Sort, agent bool) (Query, *Error) {
	var body searchBody
	if len(bytes.TrimSpace(raw)) > 0 {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		d.UseNumber()
		if err := d.Decode(&body); err != nil {
			return Query{}, invalid("the search body isn't valid")
		}
	}
	q := Query{Limit: DefaultLimit, Sort: defaultSort, Agent: agent}
	if body.Limit != nil {
		if *body.Limit < 1 || *body.Limit > MaxLimit {
			return Query{}, invalid("limit is from 1 to %d", MaxLimit)
		}
		q.Limit = *body.Limit
	}
	if body.Sort != nil {
		p, ok := props[body.Sort.Property]
		if !ok {
			return Query{}, invalid("sort: %q isn't a property", body.Sort.Property)
		}
		if agent && p.Sensitivity != "" {
			return Query{}, Fail(http.StatusBadRequest, ErrFilterNotAllowed.Code, "an agent can't sort on a sensitive property")
		}
		q.Sort = Sort{Property: body.Sort.Property, Desc: body.Sort.Desc}
	}
	if len(body.Filter) > 0 && string(body.Filter) != "null" {
		f, err := decodeFilter(body.Filter, props, agent, 0)
		if err != nil {
			return Query{}, err
		}
		q.Filter = &f
	}
	if body.Cursor != "" {
		pos, err := decodeCursor(body.Cursor, q.Sort, props)
		if err != nil {
			return Query{}, err
		}
		q.After = &pos
	}
	return q, nil
}

func decodeFilter(raw json.RawMessage, props map[string]Property, agent bool, depth int) (Filter, *Error) {
	if depth > 4 {
		return Filter{}, invalid("the filter nests too deep")
	}
	var node struct {
		And      []json.RawMessage `json:"and"`
		Property string            `json:"property"`
		Op       Op                `json:"op"`
		Value    json.RawMessage   `json:"value"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&node); err != nil {
		return Filter{}, invalid("a filter is a condition or {and: [...]}")
	}
	if node.And != nil {
		if node.Property != "" || node.Op != "" || len(node.And) == 0 || len(node.And) > 20 {
			return Filter{}, invalid("and takes 1 to 20 conditions, and nothing beside them")
		}
		out := Filter{And: make([]Filter, 0, len(node.And))}
		for _, sub := range node.And {
			f, err := decodeFilter(sub, props, agent, depth+1)
			if err != nil {
				return Filter{}, err
			}
			out.And = append(out.And, f)
		}
		return out, nil
	}
	p, ok := props[node.Property]
	if !ok {
		return Filter{}, invalid("filter: %q isn't a property", node.Property)
	}
	if agent && p.Sensitivity != "" {
		return Filter{}, Fail(http.StatusBadRequest, ErrFilterNotAllowed.Code, "an agent can't filter on a sensitive property")
	}
	where := "filter." + node.Property
	switch node.Op {
	case Eq:
	case In:
		var items []json.RawMessage
		if json.Unmarshal(node.Value, &items) != nil || len(items) == 0 || len(items) > MaxLimit {
			return Filter{}, invalid("%s: in takes a list of 1 to %d values", where, MaxLimit)
		}
		values := make([]any, len(items))
		for i, item := range items {
			v, code := decodeValue(item, Param{Type: p.Type, Values: p.Values})
			if code != "" {
				return Filter{}, invalid("%s: %s", where, code)
			}
			values[i] = v
		}
		return Filter{Property: node.Property, Op: In, Value: values}, nil
	case Lt, Gt:
		if p.Type == Boolean || p.Type == Enum || p.Type == Ref || p.Type == Text {
			return Filter{}, invalid("%s: %s doesn't compare a %s", where, node.Op, p.Type)
		}
	case Contains:
		if p.Type != String && p.Type != Text {
			return Filter{}, invalid("%s: contains is for string and text", where)
		}
	default:
		return Filter{}, invalid("%s: op is eq, in, lt, gt or contains", where)
	}
	v, code := decodeValue(node.Value, Param{Type: p.Type, Values: p.Values})
	if code != "" {
		return Filter{}, invalid("%s: %s", where, code)
	}
	return Filter{Property: node.Property, Op: node.Op, Value: v}, nil
}

var (
	decimalPattern = regexp.MustCompile(`^-?[0-9]{1,30}(\.[0-9]{1,10})?$`)
	refPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-z][a-z0-9_]*:[A-Za-z0-9_.-]{1,200}$`)
)

// decodeValue reads one JSON value as a parameter's type, and answers the
// error code when it doesn't fit: wrong_type or not_allowed.
func decodeValue(raw json.RawMessage, p Param) (any, string) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil || v == nil {
		return nil, "wrong_type"
	}
	switch p.Type {
	case String, Text:
		s, ok := v.(string)
		if !ok || (p.Type == String && len(s) > 1000) || len(s) > 100_000 {
			return nil, "wrong_type"
		}
		return s, ""
	case Enum:
		s, ok := v.(string)
		if !ok {
			return nil, "wrong_type"
		}
		if len(p.Values) > 0 && !slices.Contains(p.Values, s) {
			return nil, "not_allowed"
		}
		return s, ""
	case Ref:
		s, ok := v.(string)
		if !ok || !refPattern.MatchString(s) || (p.To != "" && !hasPrefix(s, p.To+":")) {
			return nil, "wrong_type"
		}
		return s, ""
	case Integer, MoneyFen:
		n, ok := v.(json.Number)
		if !ok {
			return nil, "wrong_type"
		}
		i, err := n.Int64()
		if err != nil {
			return nil, "wrong_type"
		}
		return i, ""
	case Decimal:
		s, ok := v.(string)
		if !ok || !decimalPattern.MatchString(s) {
			return nil, "wrong_type"
		}
		return s, ""
	case Boolean:
		b, ok := v.(bool)
		if !ok {
			return nil, "wrong_type"
		}
		return b, ""
	case Date:
		s, ok := v.(string)
		if _, err := time.Parse(time.DateOnly, s); !ok || err != nil {
			return nil, "wrong_type"
		}
		return s, ""
	case Timestamp:
		s, ok := v.(string)
		t, err := time.Parse(time.RFC3339, s)
		if !ok || err != nil {
			return nil, "wrong_type"
		}
		return t, ""
	}
	return nil, "wrong_type"
}

func hasPrefix(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }

type cursor struct {
	Sort  string          `json:"s"`
	Desc  bool            `json:"d,omitempty"`
	Value json.RawMessage `json:"v,omitempty"`
	ID    string          `json:"id"`
}

// encodeCursor is the cursor after the last object of a page.
func encodeCursor(s Sort, value any, id string) string {
	c := cursor{Sort: s.Property, Desc: s.Desc, ID: id}
	if s.Property != "" && value != nil {
		if t, ok := value.(time.Time); ok {
			value = t.UTC().Format(time.RFC3339Nano)
		}
		c.Value, _ = json.Marshal(value)
	}
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string, sort Sort, props map[string]Property) (Position, *Error) {
	bad := invalid("the cursor isn't one this module issued for this search")
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Position{}, bad
	}
	var c cursor
	if json.Unmarshal(raw, &c) != nil || c.ID == "" || c.Sort != sort.Property || c.Desc != sort.Desc {
		return Position{}, bad
	}
	pos := Position{ID: c.ID}
	if sort.Property != "" && len(c.Value) > 0 {
		p := props[sort.Property]
		if p.Type == Timestamp {
			var text string
			if json.Unmarshal(c.Value, &text) != nil {
				return Position{}, bad
			}
			t, err := time.Parse(time.RFC3339Nano, text)
			if err != nil {
				return Position{}, bad
			}
			pos.Value = t
			return pos, nil
		}
		v, code := decodeValue(c.Value, Param{Type: p.Type})
		if code != "" {
			return Position{}, bad
		}
		pos.Value = v
	}
	return pos, nil
}

// decodeParams reads an action's params against its declared (and tenant
// layer's) parameters: unknown ones, missing required ones, wrong types and
// values outside an enum are each a FieldError.
func decodeParams(raw json.RawMessage, params map[string]Param) (map[string]any, []FieldError) {
	in := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &in) != nil {
			return nil, []FieldError{{Param: "params", Code: "wrong_type"}}
		}
	}
	out := map[string]any{}
	var errs []FieldError
	for name := range in {
		if _, ok := params[name]; !ok {
			errs = append(errs, FieldError{Param: name, Code: "unknown"})
		}
	}
	for name, p := range params {
		v, ok := in[name]
		if !ok || string(v) == "null" {
			if p.Required {
				errs = append(errs, FieldError{Param: name, Code: "required"})
			}
			continue
		}
		value, code := decodeValue(v, p)
		if code != "" {
			errs = append(errs, FieldError{Param: name, Code: code})
			continue
		}
		if s, ok := value.(string); ok && p.Required && s == "" {
			errs = append(errs, FieldError{Param: name, Code: "required"})
			continue
		}
		out[name] = value
	}
	slices.SortFunc(errs, func(a, b FieldError) int {
		if a.Param < b.Param {
			return -1
		}
		if a.Param > b.Param {
			return 1
		}
		return 0
	})
	return out, errs
}
