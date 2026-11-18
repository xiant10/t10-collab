package docsmcp

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Operation is one route of the spec, as docs_api lists it.
type Operation struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	OperationID string `json:"operation_id,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

type spec struct {
	root map[string]any
	ops  []Operation
}

var methods = []string{"get", "put", "post", "patch", "delete", "head", "options"}

func loadSpec(raw []byte) (*spec, error) {
	var parsed any
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("openapi.yaml: %w", err)
	}
	root, ok := stringKeys(parsed).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("openapi.yaml isn't a mapping")
	}
	s := &spec{root: root}
	paths, _ := root["paths"].(map[string]any)
	for p, v := range paths {
		item, _ := v.(map[string]any)
		for _, m := range methods {
			op, ok := item[m].(map[string]any)
			if !ok {
				continue
			}
			id, _ := op["operationId"].(string)
			sum, _ := op["summary"].(string)
			s.ops = append(s.ops, Operation{Method: strings.ToUpper(m), Path: p, OperationID: id, Summary: sum})
		}
	}
	sort.Slice(s.ops, func(i, j int) bool {
		if s.ops[i].Path != s.ops[j].Path {
			return s.ops[i].Path < s.ops[j].Path
		}
		return s.ops[i].Method < s.ops[j].Method
	})
	return s, nil
}

// operation finds one by its operationId or as "METHOD /path", and answers it with
// the path's shared parameters and every local $ref resolved.
func (s *spec) operation(name string) (map[string]any, bool) {
	name = strings.TrimSpace(name)
	for _, o := range s.ops {
		if o.OperationID == name || strings.EqualFold(o.Method+" "+o.Path, name) {
			item := s.root["paths"].(map[string]any)[o.Path].(map[string]any)
			out := map[string]any{"method": o.Method, "path": o.Path}
			for k, v := range item[strings.ToLower(o.Method)].(map[string]any) {
				out[k] = v
			}
			if shared, ok := item["parameters"]; ok {
				out["path_parameters"] = shared
			}
			return s.resolve(out, nil).(map[string]any), true
		}
	}
	return nil, false
}

// resolve replaces each "#/…" $ref with what it points at, leaving a ref it has
// already entered on this branch as it is, so a recursive schema stays finite.
func (s *spec) resolve(v any, seen []string) any {
	switch t := v.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
			for _, r := range seen {
				if r == ref {
					return t
				}
			}
			if target, ok := s.lookup(ref); ok {
				return s.resolve(target, append(seen, ref))
			}
			return t
		}
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = s.resolve(x, seen)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = s.resolve(x, seen)
		}
		return out
	default:
		return v
	}
}

func (s *spec) lookup(ref string) (any, bool) {
	var at any = s.root
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := at.(map[string]any)
		if !ok {
			return nil, false
		}
		if at, ok = m[part]; !ok {
			return nil, false
		}
	}
	return at, true
}

// stringKeys makes every mapping's keys strings, as JSON needs: YAML reads an
// unquoted 200 as a number.
func stringKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = stringKeys(x)
		}
		return t
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[fmt.Sprint(k)] = stringKeys(x)
		}
		return out
	case []any:
		for i, x := range t {
			t[i] = stringKeys(x)
		}
		return t
	default:
		return v
	}
}
