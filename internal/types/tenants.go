package types

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Tenant versions: the owning module tailors a type for one tenant, such as a
// company's own labels and the details it asks for before a send. A tenant
// version tailors one default version and carries its number. Once the module
// pushes a newer default, the tenant falls back to it until the module pushes
// the tenant's version again, so a tailoring never reshapes a lifecycle it
// wasn't written for.

// ErrNoDefault means a tenant version names a default version collab doesn't
// hold: the module pushes the default first.
var ErrNoDefault = errors.New("push the type's default at this version first")

// Tailor checks that a tenant version changes only what a tenant may: labels,
// field titles, what moves require, and new fields. States, phases, who takes
// a move, where it goes, the visibility profile and the departure rule are the
// module's promises to every tenant. It works by putting the default's values
// back into the tenant version: what's left must be the default.
func Tailor(tenant, base Definition) error {
	t := tenant
	t.Label = base.Label
	if !slices.Equal(slices.Sorted(maps.Keys(t.States)), slices.Sorted(maps.Keys(base.States))) {
		return invalid("a tenant version keeps the default's states")
	}
	for name, st := range base.States {
		if tenant.States[name].Phase != st.Phase {
			return invalid("a tenant version keeps state %s's phase", name)
		}
	}
	// Only the labels differ now, and they may.
	t.States = maps.Clone(base.States)
	for key, field := range base.Fields.Properties {
		mine, ok := tenant.Fields.Properties[key]
		if !ok {
			return invalid("a tenant version keeps the default's field %s", key)
		}
		mine.Title = field.Title
		if !equalJSON(Definition{Fields: Fields{Properties: map[string]Field{key: mine}}}, Definition{Fields: Fields{Properties: map[string]Field{key: field}}}) {
			return invalid("a tenant version may change field %s's title only", key)
		}
	}
	t.Fields = base.Fields
	if (t.Offers == nil) != (base.Offers == nil) {
		return invalid("a tenant version keeps the default's offer mappings")
	}
	if t.Offers != nil {
		offers := *t.Offers
		for _, event := range []string{OfferSend, OfferAssign, OfferAccept, OfferDecline, OfferWithdraw} {
			mine, theirs := offers.Offer(event), base.Offers.Offer(event)
			if (mine == nil) != (theirs == nil) {
				return invalid("a tenant version keeps the default's offer mappings")
			}
			if mine != nil {
				copied := *mine
				copied.Requires = theirs.Requires
				setOffer(&offers, event, &copied)
			}
		}
		t.Offers = &offers
	}
	if len(t.Transitions) != len(base.Transitions) {
		return invalid("a tenant version keeps the default's transitions")
	}
	t.Transitions = slices.Clone(t.Transitions)
	for i := range t.Transitions {
		t.Transitions[i].Label, t.Transitions[i].Requires = base.Transitions[i].Label, base.Transitions[i].Requires
	}
	if !equalJSON(t, base) {
		return invalid("a tenant version may change labels, field titles, requires and add fields only")
	}
	return nil
}

func setOffer(o *Offers, event string, m *OfferMove) {
	switch event {
	case OfferSend:
		o.Send = m
	case OfferAssign:
		o.Assign = m
	case OfferAccept:
		o.Accept = m
	case OfferDecline:
		o.Decline = m
	case OfferWithdraw:
		o.Withdraw = m
	}
}

// PutTenant stores a tenant's version of a type, for its owning module only.
func (s *Store) PutTenant(ctx context.Context, service, tenantID string, d Definition) (Type, error) {
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
		// The default is read under a share lock, so it can't move on while
		// the tenant version is checked against it.
		base, err := scanType(tx.QueryRow(ctx,
			"SELECT definition, updated_at FROM collab.types WHERE tenant_id = '' AND id = $1 FOR SHARE", d.ID))
		if errors.Is(err, ErrUnknown) || (err == nil && base.Version != d.Version) {
			return ErrNoDefault
		}
		if err != nil {
			return err
		}
		if err := Tailor(d, base.Definition); err != nil {
			return err
		}
		err = tx.QueryRow(ctx,
			`INSERT INTO collab.types (tenant_id, id, module, version, definition) VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (tenant_id, id) DO UPDATE SET version = $4, definition = $5, updated_at = now()
			 RETURNING updated_at`, tenantID, d.ID, d.OwnerModule, d.Version, raw).Scan(&t.UpdatedAt)
		if err != nil {
			return fmt.Errorf("write tenant type: %w", err)
		}
		t.Definition = d
		return bump(ctx, tx)
	})
	return t, err
}

// DeleteTenant puts a tenant back on a type's default. Deleting what isn't
// there is no error, so a retry succeeds.
func (s *Store) DeleteTenant(ctx context.Context, service, tenantID, id string) error {
	module := Module(id)
	if module == "" || module == "collab" {
		return invalid("id must be <module>.<name>")
	}
	if service != "t10-"+module {
		return ErrNotOwner
	}
	if _, err := s.pool.Exec(ctx,
		`WITH gone AS (DELETE FROM collab.types WHERE tenant_id = $1 AND id = $2 RETURNING 1)
		 UPDATE collab.type_revision SET revision = revision + 1 WHERE EXISTS (SELECT 1 FROM gone)`, tenantID, id); err != nil {
		return fmt.Errorf("delete tenant type: %w", err)
	}
	return nil
}
