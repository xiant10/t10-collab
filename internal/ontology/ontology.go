// Package ontology is what collab serves through the ontology contract (the
// workspace's contracts/ontology): work items, lists, comments and offers under
// collab's one policy, the tenant's item types as its tenant layer, and actions
// over collab's own services. See docs/ontology.md.
package ontology

import (
	"context"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"
	onto "github.com/xiant10/t10-modkit/ontology"
	"github.com/xiant10/t10-modkit/ontology/proposals"
	"github.com/xiant10/t10-modkit/ontology/undo"

	"t10-collab/internal/offers"
	"t10-collab/internal/policy"
	"t10-collab/internal/types"
	"t10-collab/internal/workitems"
)

// Version is the core's version, raised whenever the core changes.
const Version = 1

// schema holds the drafts and undo entries, beside collab's own tables: the
// db package migrates them.
const schema = "collab"

// Deps are the services the objects and actions read and call.
type Deps struct {
	Pool   *pgxpool.Pool
	Items  *workitems.Service
	Offers *offers.Service
	Types  *types.Store
}

// Ontology is collab's registry and what it reads.
type Ontology struct {
	d   Deps
	reg *onto.Registry[policy.Caller]
}

// New declares collab's ontology. authenticate reads a request's caller, and
// errors maps collab's errors to the contract's, as the routes answer them.
func New(d Deps, authenticate func(*http.Request) (policy.Caller, onto.Token, error), errors func(error) error) *Ontology {
	o := &Ontology{d: d}
	o.reg = onto.New(onto.Options[policy.Caller]{
		Module: "collab", Prefix: "work", Version: Version,
		Authenticate: authenticate,
		Who:          func(c policy.Caller) onto.Who { return onto.Who{Tenant: c.TenantID, User: c.UserID} },
		// Collab's people need no business permission: each record's policy
		// decides, in each action's Available. An agent's grant is checked on
		// its tools.
		Can:       func(policy.Caller, string) bool { return true },
		Tenant:    o.tenant,
		Proposals: proposals.Postgres{Pool: d.Pool, Schema: schema},
		Undo:      undo.Postgres{Pool: d.Pool, Schema: schema},
		Errors:    errors,
	})
	o.declareItems()
	o.declareLists()
	o.declareComments()
	o.declareOffers()
	o.declareActions()
	return o
}

// Registry is the declared ontology, for its routes and tools.
func (o *Ontology) Registry() *onto.Registry[policy.Caller] { return o.reg }

// tenant is the tenant layer: the item types that apply in the tenant, their
// states, their fields as properties, and the steps their items may take.
func (o *Ontology) tenant(ctx context.Context, c policy.Caller) (onto.Tenant, error) {
	revision, err := o.d.Types.Revision(ctx)
	if err != nil {
		return onto.Tenant{}, err
	}
	all, err := o.d.Types.List(ctx, c.TenantID)
	if err != nil {
		return onto.Tenant{}, err
	}
	var typeIDs, states, moves []string
	props := map[string]onto.Property{}
	for _, t := range all {
		if t.ID != types.Task {
			typeIDs = append(typeIDs, t.ID)
		}
		for _, s := range t.StateOrder {
			if !slices.Contains(states, s) {
				states = append(states, s)
			}
		}
		for _, tr := range t.Transitions {
			if !slices.Contains(moves, tr.ID) {
				moves = append(moves, tr.ID)
			}
		}
		for key, f := range t.Fields.Properties {
			p := onto.Property{Type: fieldKind(f), Name: f.Title}
			if p.Name == "" {
				p.Name = key
			}
			for _, v := range f.Enum {
				if s, ok := v.(string); ok && p.Type == onto.Enum {
					p.Values = append(p.Values, s)
				}
			}
			props[fieldProperty(t.ID, key)] = p
		}
	}
	values := map[string][]string{}
	if len(typeIDs) > 0 {
		values["type"] = typeIDs
	}
	if len(states) > 0 {
		values["state"] = states
	}
	out := onto.Tenant{Revision: revision, ObjectTypes: map[string]onto.TenantType{"work_item": {Values: values, Properties: props}}}
	if len(moves) > 0 {
		out.ActionTypes = map[string]onto.TenantAction{"work_item.move": {Values: map[string][]string{"move": moves}}}
	}
	return out, nil
}
