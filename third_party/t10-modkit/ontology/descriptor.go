// Package ontology lets a business module describe its records, links and actions
// as data and serve them through the generic routes and agent tools of the
// workspace's contracts/ontology. A module declares object and action types over
// code it already has; the package builds the descriptor, the routes, filter and
// cursor decoding, the actions lists, proposals, undo and the MCP tools from those
// declarations, so they can't drift apart.
//
// It owns plumbing and never truth: it opens no database connection and decides
// no authority. Every read and write goes through functions the module supplies,
// which apply its own scope, field rules and command path.
package ontology

import "slices"

// Kind is a property's type, one of the contract's set.
type Kind string

const (
	String    Kind = "string"
	Text      Kind = "text"
	Integer   Kind = "integer"
	Decimal   Kind = "decimal"
	MoneyFen  Kind = "money_fen"
	Boolean   Kind = "boolean"
	Date      Kind = "date"
	Timestamp Kind = "timestamp"
	Enum      Kind = "enum"
	Ref       Kind = "ref"
)

var kinds = []Kind{String, Text, Integer, Decimal, MoneyFen, Boolean, Date, Timestamp, Enum, Ref}

// Sensitivity marks personal data, which every agent sees masked.
type Sensitivity string

const (
	Contact  Sensitivity = "contact"
	Identity Sensitivity = "identity"
	Personal Sensitivity = "personal"
)

// Tier is what an agent may do with an action.
type Tier string

const (
	// Never: only people apply it.
	Never Tier = "never"
	// Propose: an agent drafts it, and the person confirms.
	Propose Tier = "propose"
	// Undoable: an agent applies it for the person, who can undo it.
	Undoable Tier = "undoable"
)

// Property is one property of an object type or a link.
type Property struct {
	Type        Kind        `json:"type"`
	Name        string      `json:"name,omitempty"`
	NameZh      string      `json:"name_zh,omitempty"`
	Values      []string    `json:"values,omitempty"`
	To          string      `json:"to,omitempty"`
	Sensitivity Sensitivity `json:"sensitivity,omitempty"`
	Derived     bool        `json:"derived,omitempty"`
	Sources     bool        `json:"sources,omitempty"`
}

// Param is one parameter of an action type.
type Param struct {
	Type        Kind        `json:"type"`
	Name        string      `json:"name,omitempty"`
	NameZh      string      `json:"name_zh,omitempty"`
	Values      []string    `json:"values,omitempty"`
	To          string      `json:"to,omitempty"`
	Sensitivity Sensitivity `json:"sensitivity,omitempty"`
	Required    bool        `json:"required,omitempty"`
	// Label promises that the action's effect doesn't depend on the value, so a
	// tenant layer may add values to it. The module's own tests keep the promise.
	Label bool `json:"label,omitempty"`
}

// Link is a named link from an object type to another.
type Link struct {
	To         string              `json:"to"`
	Many       bool                `json:"many"`
	Name       string              `json:"name,omitempty"`
	NameZh     string              `json:"name_zh,omitempty"`
	Properties map[string]Property `json:"properties,omitempty"`
}

// ObjectTypeInfo is an object type as the descriptor shows it.
type ObjectTypeInfo struct {
	Label       string              `json:"label"`
	LabelZh     string              `json:"label_zh"`
	Description string              `json:"description,omitempty"`
	Title       string              `json:"title"`
	Implements  []string            `json:"implements"`
	Properties  map[string]Property `json:"properties"`
	Links       map[string]Link     `json:"links"`
}

// ActionTypeInfo is an action type as the descriptor shows it.
type ActionTypeInfo struct {
	Label       string           `json:"label"`
	LabelZh     string           `json:"label_zh"`
	Description string           `json:"description,omitempty"`
	On          string           `json:"on,omitempty"`
	Params      map[string]Param `json:"params"`
	Permission  string           `json:"permission"`
	Idempotent  bool             `json:"idempotent"`
	Undo        bool             `json:"undo"`
	// UndoWindow is in seconds.
	UndoWindow int  `json:"undo_window,omitempty"`
	Agent      Tier `json:"agent"`
}

// Event is an event kind the module posts, and the object type it concerns.
type Event struct {
	On string `json:"on"`
}

// Descriptor is what GET /v1/ontology answers.
type Descriptor struct {
	Module      string                    `json:"module"`
	Version     int                       `json:"version"`
	ObjectTypes map[string]ObjectTypeInfo `json:"object_types"`
	ActionTypes map[string]ActionTypeInfo `json:"action_types"`
	Restricted  []string                  `json:"restricted"`
	Interfaces  []string                  `json:"interfaces"`
	Events      map[string]Event          `json:"events"`
	Tenant      Tenant                    `json:"tenant"`
}

// Tenant is one tenant's layer over the core: values and properties only.
type Tenant struct {
	Revision    int64                   `json:"revision"`
	ObjectTypes map[string]TenantType   `json:"object_types,omitempty"`
	ActionTypes map[string]TenantAction `json:"action_types,omitempty"`
}

// TenantType adds enum values and properties to a core object type and its links.
type TenantType struct {
	Values     map[string][]string   `json:"values,omitempty"`
	Properties map[string]Property   `json:"properties,omitempty"`
	Links      map[string]TenantLink `json:"links,omitempty"`
}

// TenantLink adds enum values and properties to a core link.
type TenantLink struct {
	Values     map[string][]string `json:"values,omitempty"`
	Properties map[string]Property `json:"properties,omitempty"`
}

// TenantAction adds enum values to a core action's label parameters.
type TenantAction struct {
	Values map[string][]string `json:"values"`
}

// Interfaces are the contract's, from contracts/ontology/interfaces.json. A
// module never defines its own under the t10: prefix.
var Interfaces = map[string]map[string]Property{
	"t10:Assignable":  {"assignee": {Type: Ref, To: "auth:user"}},
	"t10:HasDeadline": {"deadline": {Type: Timestamp}},
	"t10:Person":      {"name": {Type: String}},
	"t10:Commentable": {"comment_count": {Type: Integer}},
}

// Properties are a type's core properties with its tenant layer's: the added
// properties, and the added values on enums.
func (d Descriptor) Properties(objectType string) map[string]Property {
	t, ok := d.ObjectTypes[objectType]
	if !ok {
		return nil
	}
	layer := d.Tenant.ObjectTypes[objectType]
	return merge(t.Properties, layer.Properties, layer.Values)
}

// LinkProperties are a link's core properties with its tenant layer's.
func (d Descriptor) LinkProperties(objectType, link string) map[string]Property {
	l := d.ObjectTypes[objectType].Links[link]
	layer := d.Tenant.ObjectTypes[objectType].Links[link]
	return merge(l.Properties, layer.Properties, layer.Values)
}

// Params are an action's parameters with the values its tenant layer adds.
func (d Descriptor) Params(action string) map[string]Param {
	a := d.ActionTypes[action]
	out := make(map[string]Param, len(a.Params))
	for name, p := range a.Params {
		if added := d.Tenant.ActionTypes[action].Values[name]; len(added) > 0 {
			p.Values = appendNew(p.Values, added)
		}
		out[name] = p
	}
	return out
}

func merge(core, added map[string]Property, values map[string][]string) map[string]Property {
	out := make(map[string]Property, len(core)+len(added))
	for name, p := range core {
		out[name] = p
	}
	for name, p := range added {
		out[name] = p
	}
	for name, v := range values {
		if p, ok := out[name]; ok {
			p.Values = appendNew(p.Values, v)
			out[name] = p
		}
	}
	return out
}

func appendNew(have, more []string) []string {
	out := slices.Clone(have)
	for _, v := range more {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
