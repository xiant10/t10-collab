package ontology

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

var (
	keyPattern        = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	actionKeyPattern  = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	propertyPattern   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	typeRefPattern    = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-z][a-z0-9_]*$`)
	permissionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-z0-9_.]+$`)
)

// CheckCore reports every way a core descriptor breaks the contract's rules, the
// same rules contracts/ontology/descriptor.ts checks.
func CheckCore(d Descriptor) error {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if !keyPattern.MatchString(d.Module) {
		fail("module %q isn't a module ID", d.Module)
	}
	if d.Version < 1 {
		fail("version must be at least 1")
	}
	declared := func(to string) bool {
		module, name, _ := strings.Cut(to, ":")
		_, ok := d.ObjectTypes[name]
		return module != d.Module || ok
	}
	used := map[string]bool{}
	for name, t := range d.ObjectTypes {
		if !keyPattern.MatchString(name) {
			fail("object type %q isn't a key", name)
		}
		if t.Label == "" || t.LabelZh == "" {
			fail("%s: needs label and label_zh", name)
		}
		if _, ok := t.Properties[t.Title]; !ok {
			fail("%s: its title %q isn't one of its properties", name, t.Title)
		}
		for prop, p := range t.Properties {
			checkProperty(fail, name+"."+prop, prop, p, declared)
		}
		for link, l := range t.Links {
			if !keyPattern.MatchString(link) {
				fail("%s.links.%s: isn't a key", name, link)
			}
			if !typeRefPattern.MatchString(l.To) || !declared(l.To) {
				fail("%s.links.%s: %q isn't a declared type", name, link, l.To)
			}
			for prop, p := range l.Properties {
				checkProperty(fail, name+".links."+link+"."+prop, prop, p, declared)
			}
		}
		for _, iface := range t.Implements {
			used[iface] = true
			want, ok := Interfaces[iface]
			if !ok {
				fail("%s: %s isn't an interface the contract defines", name, iface)
				continue
			}
			for prop, w := range want {
				have := t.Properties[prop]
				if have.Type != w.Type || (w.To != "" && have.To != w.To) {
					fail("%s: %s needs %s as %s", name, iface, prop, w.Type)
				}
			}
		}
	}
	for iface := range used {
		if !slices.Contains(d.Interfaces, iface) {
			fail("interfaces: leaves out %s, which a type implements", iface)
		}
	}
	for _, iface := range d.Interfaces {
		if !used[iface] {
			fail("interfaces: lists %s, which no type implements", iface)
		}
	}
	for name, a := range d.ActionTypes {
		if !actionKeyPattern.MatchString(name) {
			fail("action type %q isn't a key", name)
		}
		if a.Label == "" || a.LabelZh == "" {
			fail("%s: needs label and label_zh", name)
		}
		if !permissionPattern.MatchString(a.Permission) {
			fail("%s: permission %q isn't a business permission", name, a.Permission)
		}
		target, ok := d.ObjectTypes[a.On]
		if a.On != "" && !ok {
			fail("%s: acts on %s, which isn't a declared type", name, a.On)
		}
		for param, p := range a.Params {
			checkProperty(fail, name+"."+param, param, Property{Type: p.Type, Values: p.Values, To: p.To, Sensitivity: p.Sensitivity}, declared)
			if target.Properties[param].Derived {
				fail("%s: param %s would set the derived %s.%s", name, param, a.On, param)
			}
		}
		switch a.Agent {
		case Never, Propose:
		case Undoable:
			if !a.Undo || !a.Idempotent {
				fail("%s: an undoable action must be undo and idempotent", name)
			}
		default:
			fail("%s: tier %q isn't never, propose or undoable", name, a.Agent)
		}
		if a.Undo != (a.UndoWindow > 0) {
			fail("%s: undo needs an undo_window, and only undo has one", name)
		}
	}
	for _, name := range d.Restricted {
		a, ok := d.ActionTypes[name]
		if !ok {
			fail("restricted: %s isn't a declared action", name)
		} else if a.Agent != Never {
			fail("restricted: %s has tier %s, not never", name, a.Agent)
		}
	}
	for kind, e := range d.Events {
		if !actionKeyPattern.MatchString(kind) {
			fail("events: %q isn't an event kind", kind)
		}
		if _, ok := d.ObjectTypes[e.On]; !ok {
			fail("events.%s: %s isn't a declared type", kind, e.On)
		}
	}
	return joined(errs)
}

func checkProperty(fail func(string, ...any), where, name string, p Property, declared func(string) bool) {
	if !propertyPattern.MatchString(name) {
		fail("%s: isn't a property name", where)
	}
	if !slices.Contains(kinds, p.Type) {
		fail("%s: type %q isn't one of the contract's", where, p.Type)
	}
	if (p.Type == Ref) != (p.To != "") {
		fail("%s: a ref, and only a ref, names its target", where)
	}
	if p.Type == Ref && (!typeRefPattern.MatchString(p.To) || !declared(p.To)) {
		fail("%s: %q isn't a declared type", where, p.To)
	}
	if p.Type != Enum && len(p.Values) > 0 {
		fail("%s: only an enum has values", where)
	}
	switch p.Sensitivity {
	case "", Contact, Identity, Personal:
	default:
		fail("%s: sensitivity %q isn't contact, identity or personal", where, p.Sensitivity)
	}
	if p.Sources && !p.Derived {
		fail("%s: only a derived property has sources", where)
	}
}

// CheckTenant reports every way a tenant layer does more than add: a type, link
// or action the core doesn't declare, a property that redeclares a core one,
// values on something that isn't an enum, or values on an action parameter that
// isn't a label, unless the action's tier is never.
func CheckTenant(core Descriptor, t Tenant) error {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, "tenant: "+fmt.Sprintf(format, args...)) }
	addValues := func(where string, values map[string][]string, coreProps, added map[string]Property) {
		for prop := range values {
			p, ok := coreProps[prop]
			if !ok {
				p, ok = added[prop]
			}
			if !ok {
				fail("%s.%s: adds values to a property that isn't declared", where, prop)
			} else if p.Type != Enum {
				fail("%s.%s: adds values to a %s, not an enum", where, prop, p.Type)
			}
		}
	}
	addProperties := func(where string, added, coreProps map[string]Property) {
		for prop, p := range added {
			if _, ok := coreProps[prop]; ok {
				fail("%s.%s: redeclares a core property", where, prop)
			}
			checkProperty(func(format string, args ...any) { fail(format, args...) }, where+"."+prop, prop, p, func(string) bool { return true })
		}
	}
	for name, layer := range t.ObjectTypes {
		ot, ok := core.ObjectTypes[name]
		if !ok {
			fail("object_types.%s: isn't a core type; a tenant layer adds no types", name)
			continue
		}
		addProperties(name, layer.Properties, ot.Properties)
		addValues(name, layer.Values, ot.Properties, layer.Properties)
		for link, ll := range layer.Links {
			cl, ok := ot.Links[link]
			if !ok {
				fail("%s.links.%s: isn't a core link", name, link)
				continue
			}
			addProperties(name+".links."+link, ll.Properties, cl.Properties)
			addValues(name+".links."+link, ll.Values, cl.Properties, ll.Properties)
		}
	}
	for name, layer := range t.ActionTypes {
		a, ok := core.ActionTypes[name]
		if !ok {
			fail("action_types.%s: declares an action type; a tenant layer adds none", name)
			continue
		}
		for param := range layer.Values {
			p, ok := a.Params[param]
			switch {
			case !ok:
				fail("%s.%s: adds values to a parameter that isn't declared", name, param)
			case p.Type != Enum:
				fail("%s.%s: adds values to a %s, not an enum", name, param, p.Type)
			case !p.Label && a.Agent != Never:
				fail("%s.%s: adds values to a parameter that isn't \"label\": true, on a %s action", name, param, a.Agent)
			}
		}
	}
	return joined(errs)
}

func joined(errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return errors.New(strings.Join(errs, "; "))
}
