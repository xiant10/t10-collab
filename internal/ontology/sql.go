package ontology

import (
	"fmt"
	"strings"
	"time"

	onto "github.com/xiant10/t10-modkit/ontology"
)

// A search compiled to SQL: the filter on whitelisted columns, the order, and
// the cursor, each over placeholders the query adds as it goes.

// column is a property as SQL: expr for filters, and sort (expr when empty)
// for ordering. null is the literal a missing value sorts as, so that a cursor
// on one can be compared; cast types a placeholder, such as ::numeric.
type column struct {
	expr, sort, null, cast string
	kind                   onto.Kind
	// unsorted refuses a sort on it.
	unsorted bool
}

func (c column) sortExpr() string {
	if c.sort != "" {
		return c.sort
	}
	return c.expr
}

// args numbers placeholders from a first one.
type args struct {
	first  int
	values []any
}

func (a *args) add(v any) string {
	a.values = append(a.values, v)
	return fmt.Sprintf("$%d", a.first+len(a.values)-1)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)

// where compiles a filter. A property with no column answers
// filter_not_allowed, as the contract lets a module refuse one.
func where(f *onto.Filter, cols map[string]column, a *args) (string, error) {
	if f == nil {
		return "true", nil
	}
	if f.And != nil {
		parts := make([]string, len(f.And))
		for i := range f.And {
			part, err := where(&f.And[i], cols, a)
			if err != nil {
				return "", err
			}
			parts[i] = part
		}
		return "(" + strings.Join(parts, " AND ") + ")", nil
	}
	col, ok := cols[f.Property]
	if !ok {
		return "", onto.Fail(400, onto.ErrFilterNotAllowed.Code, "this module doesn't filter on "+f.Property)
	}
	switch f.Op {
	case onto.Eq:
		return fmt.Sprintf("(%s = %s%s)", col.expr, a.add(f.Value), col.cast), nil
	case onto.Lt:
		return fmt.Sprintf("(%s < %s%s)", col.expr, a.add(f.Value), col.cast), nil
	case onto.Gt:
		return fmt.Sprintf("(%s > %s%s)", col.expr, a.add(f.Value), col.cast), nil
	case onto.Contains:
		s, _ := f.Value.(string)
		return fmt.Sprintf("(%s ILIKE '%%' || %s || '%%')", col.expr, a.add(likeEscaper.Replace(s))), nil
	case onto.In:
		values, _ := f.Value.([]any)
		return fmt.Sprintf("(%s = ANY(%s%s))", col.expr, a.add(array(values)), arrayCast(col)), nil
	}
	return "", onto.Fail(400, "invalid_request", "unknown op")
}

// array is a list of filter values as a slice Postgres takes.
func array(values []any) any {
	if len(values) == 0 {
		return []string{}
	}
	switch values[0].(type) {
	case int64:
		out := make([]int64, len(values))
		for i, v := range values {
			out[i], _ = v.(int64)
		}
		return out
	case bool:
		out := make([]bool, len(values))
		for i, v := range values {
			out[i], _ = v.(bool)
		}
		return out
	case time.Time:
		out := make([]time.Time, len(values))
		for i, v := range values {
			out[i], _ = v.(time.Time)
		}
		return out
	}
	out := make([]string, len(values))
	for i, v := range values {
		out[i], _ = v.(string)
	}
	return out
}

func arrayCast(col column) string {
	if col.cast != "" {
		return col.cast + "[]"
	}
	return ""
}

// order is the ORDER BY for a sort, ending with the ID, and the condition for
// the page after a position. A sort on a column that refuses one answers
// filter_not_allowed.
func order(s onto.Sort, after *onto.Position, cols map[string]column, a *args) (orderBy, cursor string, err error) {
	if s.Property == "" {
		if after != nil {
			return "i.id", "i.id > " + a.add(after.ID), nil
		}
		return "i.id", "true", nil
	}
	col, ok := cols[s.Property]
	if !ok || col.unsorted {
		return "", "", onto.Fail(400, onto.ErrFilterNotAllowed.Code, "this module doesn't sort on "+s.Property)
	}
	dir, cmp := "ASC", ">"
	if s.Desc {
		dir, cmp = "DESC", "<"
	}
	orderBy = fmt.Sprintf("%s %s, i.id %s", col.sortExpr(), dir, dir)
	if after == nil {
		return orderBy, "true", nil
	}
	value := ""
	if after.Value == nil && col.null != "" {
		value = col.null
	} else {
		value = a.add(after.Value) + col.cast
	}
	return orderBy, fmt.Sprintf("((%s, i.id) %s (%s, %s))", col.sortExpr(), cmp, value, a.add(after.ID)), nil
}

// quote is a SQL string literal, for names checked against a pattern already.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
