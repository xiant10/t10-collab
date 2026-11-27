package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	onto "github.com/xiant10/t10-modkit/ontology"

	"t10-collab/internal/ontology"
	"t10-collab/internal/policy"
	"t10-collab/internal/telemetry"
	"t10-collab/internal/workitems"
)

// The ontology's routes and tools (internal/ontology), with callers read and
// errors answered as collab's own routes read and answer them.

func (s *Server) newOntology() *ontology.Ontology {
	o := ontology.New(ontology.Deps{Pool: s.Pool, Items: s.Items, Offers: s.Offers, Types: s.Types}, s.ontologyCaller, ontologyError)
	if err := o.Registry().Check(); err != nil {
		panic(err)
	}
	return o
}

// ontologyCaller admits a person's token, and an agent's acting for a person,
// which the ontology's routes refuse but for the descriptor. A token with no
// tenant, such as a platform admin's or a service's, reads nothing.
func (s *Server) ontologyCaller(r *http.Request) (policy.Caller, onto.Token, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return policy.Caller{}, 0, onto.Fail(http.StatusUnauthorized, "invalid_token", "missing, invalid or expired bearer token")
	}
	who, err := s.Verifier.Verify(r.Context(), raw)
	if err != nil {
		return policy.Caller{}, 0, onto.Fail(http.StatusUnauthorized, "invalid_token", "missing, invalid or expired bearer token")
	}
	if who.UserID == "" || who.TenantID == "" {
		return policy.Caller{}, 0, onto.Fail(http.StatusForbidden, "forbidden", "this token can't use this route")
	}
	c := policy.Caller{TenantID: who.TenantID, UserID: who.UserID}
	if who.Agent != nil {
		c.GrantID, c.GrantChain = who.Agent.GrantID, grantIDs(who.Agent.Chain)
		return c, onto.DelegatedToken, nil
	}
	return c, onto.PersonToken, nil
}

// ontologyError answers collab's errors as the contract's: a stale version is
// stale_revision, an item someone else is on can't be undone, and the rest keep
// the codes collab's routes answer.
func ontologyError(err error) error {
	switch {
	case errors.Is(err, workitems.ErrVersionConflict):
		return onto.ErrStaleRevision
	case errors.Is(err, workitems.ErrInUse):
		return onto.ErrUndoConflict
	case errors.Is(err, workitems.ErrGone):
		return onto.ErrUndone
	case errors.Is(err, policy.ErrNotFound):
		return onto.ErrNotFound
	case errors.Is(err, policy.ErrForbidden):
		return onto.ErrForbidden
	}
	status, code, message := classify(err)
	if status == http.StatusInternalServerError {
		slog.Error("ontology", "error", err)
		return err
	}
	if code == "invalid_request" {
		code = "invalid_params"
	}
	return onto.Fail(status, code, message)
}

// ontologyRoutes mounts the descriptor, object, action, undo and proposal routes.
func (s *Server) ontologyRoutes(mux *http.ServeMux) {
	for _, route := range s.ontology.Registry().Routes() {
		mux.Handle(route.Pattern, telemetry.Wrap(route.Pattern, route.Handler))
	}
}
