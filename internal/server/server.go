// Package server is the HTTP API. Each route declares the action it needs, and
// the guard refuses anything but a person's token before a handler runs.
package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"t10-collab/internal/activity"
	"t10-collab/internal/lists"
	"t10-collab/internal/offers"
	"t10-collab/internal/page"
	"t10-collab/internal/people"
	"t10-collab/internal/policy"
	"t10-collab/internal/telemetry"
	"t10-collab/internal/workitems"
	"t10-collab/verifier"
)

const (
	maxBody      = 1 << 20
	defaultLimit = 50
	maxLimit     = 200
	// notifyService is the only service allowed to read attention counts.
	notifyService  = "t10-notify"
	attentionRoute = "/internal/v1/attention"
	// attentionWindow is how far back counts look when a caller names no time.
	attentionWindow = 24 * time.Hour
)

// manifest tells a client this module is here and what it can rely on. Routes
// and fields can only be added within an api_version.
var manifest = map[string]any{
	"id": "collab", "api_version": 1, "action_prefix": "work", "enabled": true, "features": []string{"attention"},
}

// Verifier checks a bearer token.
type Verifier interface {
	Verify(ctx context.Context, raw string) (*verifier.Identity, error)
}

// Org is the org reads routes make directly.
type Org interface {
	Member(ctx context.Context, tenantID, userID string) (people.Member, error)
	Search(ctx context.Context, tenantID, q, unitID, cursor string, limit int) ([]people.Member, string, error)
	Units(ctx context.Context, tenantID string) ([]people.Unit, error)
}

// Deps is what the server needs.
type Deps struct {
	Pool     *pgxpool.Pool
	Verifier Verifier
	Org      Org
	Items    *workitems.Service
	Offers   *offers.Service
	Lists    *lists.Service
	Activity *activity.Service
}

// Server handles the API.
type Server struct {
	Deps
}

// New builds a Server.
func New(d Deps) *Server { return &Server{Deps: d} }

type handler func(w http.ResponseWriter, r *http.Request, c policy.Caller)

type route struct {
	method, pattern string
	// action names what the route does. Agents don't reach routes: see guard.
	action  string
	handler handler
}

func (s *Server) routes() []route {
	return []route{
		{"GET", "/v1/module", policy.ActionRead, s.handleManifest},

		{"POST", "/v1/work-items", policy.ActionWrite, s.handleCreateItem},
		{"GET", "/v1/work-items", policy.ActionRead, s.handleListItems},
		{"GET", "/v1/work-items/{id}", policy.ActionRead, s.handleGetItem},
		{"PATCH", "/v1/work-items/{id}", policy.ActionWrite, s.handleUpdateItem},
		{"DELETE", "/v1/work-items/{id}", policy.ActionDelete, s.handleDeleteItem},
		{"GET", "/v1/work-items/{id}/events", policy.ActionRead, s.handleItemEvents},
		{"POST", "/v1/work-items/{id}/claim", policy.ActionWrite, s.handleClaim},
		{"DELETE", "/v1/work-items/{id}/owners/me", policy.ActionWrite, s.handleStopOwning},
		{"PUT", "/v1/work-items/{id}/collaborators/{user}", policy.ActionWrite, s.handleAddCollaborator},
		{"DELETE", "/v1/work-items/{id}/collaborators/{user}", policy.ActionWrite, s.handleRemoveCollaborator},
		{"GET", "/v1/work-items/{id}/comments", policy.ActionRead, s.handleListComments},
		{"POST", "/v1/work-items/{id}/comments", policy.ActionWrite, s.handleAddComment},
		{"POST", "/v1/work-items/{id}/send", policy.ActionSend, s.handleSend},

		{"GET", "/v1/offers", policy.ActionRead, s.handleListOffers},
		{"POST", "/v1/offers/{id}/accept", policy.ActionSend, s.handleAccept},
		{"POST", "/v1/offers/{id}/decline", policy.ActionSend, s.handleDecline},
		{"POST", "/v1/offers/{id}/withdraw", policy.ActionSend, s.handleWithdraw},

		{"GET", "/v1/lists", policy.ActionRead, s.handleListLists},
		{"POST", "/v1/lists", policy.ActionWrite, s.handleCreateList},
		{"GET", "/v1/lists/{id}", policy.ActionRead, s.handleGetList},
		{"PATCH", "/v1/lists/{id}", policy.ActionWrite, s.handleUpdateList},
		{"PUT", "/v1/lists/{id}/members/{user}", policy.ActionWrite, s.handlePutListMember},
		{"DELETE", "/v1/lists/{id}/members/{user}", policy.ActionWrite, s.handleRemoveListMember},

		{"GET", "/v1/people", policy.ActionRead, s.handlePeople},
		{"GET", "/v1/units", policy.ActionRead, s.handleUnits},

		{"GET", "/v1/notifications", policy.ActionRead, s.handleNotifications},
		{"POST", "/v1/notifications/read", policy.ActionWrite, s.handleMarkRead},

		{"GET", "/v1/settings", policy.ActionRead, s.handleGetSettings},
		{"PUT", "/v1/admin/settings", policy.ActionWrite, s.handlePutSettings},
		{"GET", "/v1/admin/departures", policy.ActionRead, s.handleItemQueue},
		{"POST", "/v1/admin/departures/{item}/reassign", policy.ActionWrite, s.handleReassign},
		{"GET", "/v1/admin/departures/lists", policy.ActionRead, s.handleListQueue},
		{"POST", "/v1/admin/departures/lists/{id}/owner", policy.ActionWrite, s.handleSetListOwner},
	}
}

// Handler returns the routed API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Pool.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	for _, rt := range s.routes() {
		key := rt.method + " " + rt.pattern
		mux.Handle(key, telemetry.Wrap(key, s.guard(rt)))
	}
	key := "GET " + attentionRoute
	mux.Handle(key, telemetry.Wrap(key, http.HandlerFunc(s.handleAttention)))
	key = "POST " + mcpRoute
	mux.Handle(key, telemetry.Wrap(key, http.HandlerFunc(s.handleMCP)))
	return logged(mux)
}

// guard admits a person's token only. The tenant comes from the token, and a
// token with none, like a platform admin's, gets no path into business records.
// Agents use /mcp instead: an action can't tell a route's admin half from the
// rest, so work:write here would reach tenant settings for an admin's agent.
func (s *Server) guard(rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			unauthorized(w)
			return
		}
		who, err := s.Verifier.Verify(r.Context(), raw)
		if err != nil {
			unauthorized(w)
			return
		}
		if who.UserID == "" || who.TenantID == "" || who.Agent != nil {
			writeError(w, http.StatusForbidden, "forbidden", "this token can't use this route")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		rt.handler(w, r, policy.Caller{TenantID: who.TenantID, UserID: who.UserID})
	})
}

func (s *Server) handleManifest(w http.ResponseWriter, _ *http.Request, _ policy.Caller) {
	writeJSON(w, http.StatusOK, manifest)
}

// handleAttention answers t10-notify, the one route open to a service's token. A
// service token can read any tenant, so tenant and user come from the query and
// are checked like any other ID.
func (s *Server) handleAttention(w http.ResponseWriter, r *http.Request) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		unauthorized(w)
		return
	}
	who, err := s.Verifier.Verify(r.Context(), raw)
	if err != nil {
		unauthorized(w)
		return
	}
	if who.ServiceID != notifyService {
		writeError(w, http.StatusForbidden, "forbidden", "this token can't use this route")
		return
	}
	query := r.URL.Query()
	caller := policy.Caller{TenantID: query.Get("tenant"), UserID: query.Get("user")}
	if !people.ValidID(caller.TenantID) || !people.ValidID(caller.UserID) {
		writeError(w, http.StatusBadRequest, "invalid_request", "name a tenant and a user")
		return
	}
	since := time.Now().Add(-attentionWindow)
	if value := query.Get("since"); value != "" {
		if since, err = time.Parse(time.RFC3339, value); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "since must be an RFC 3339 time")
			return
		}
	}
	counts, err := s.Activity.Attention(r.Context(), caller, since)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "invalid_token", "missing, invalid or expired bearer token")
}

// logged records the route, never the query or body: those hold other people's
// words, and the Authorization header stays out too.
func logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
			slog.Info("request", "method", r.Method, "route", r.Pattern, "status", rec.status, "duration_ms", time.Since(start).Milliseconds())
		}
	})
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("encode response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "the body isn't valid JSON for this route")
		return false
	}
	return true
}

// failure maps a domain error to a response. Anything unknown is logged and
// answers 500, with no detail for the caller.
func failure(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := classify(err)
	if status == http.StatusInternalServerError {
		slog.Error("request failed", "route", r.Pattern, "error", err)
	}
	writeError(w, status, code, message)
}

// classify names a domain error for a caller. Messages are fixed text, so they
// never carry a record's words.
func classify(err error) (status int, code, message string) {
	for _, m := range []struct {
		target error
		status int
		code   string
	}{
		{policy.ErrNotFound, http.StatusNotFound, "not_found"},
		{policy.ErrForbidden, http.StatusForbidden, "forbidden"},
		{errBadArguments, http.StatusBadRequest, "invalid_request"},
		{page.ErrBadCursor, http.StatusBadRequest, "invalid_request"},
		{workitems.ErrInvalid, http.StatusBadRequest, "invalid_request"},
		{offers.ErrInvalid, http.StatusBadRequest, "invalid_request"},
		{workitems.ErrBadPerson, http.StatusUnprocessableEntity, "invalid_recipient"},
		{workitems.ErrVersionConflict, http.StatusConflict, "version_conflict"},
		{workitems.ErrLastOwner, http.StatusConflict, "last_owner"},
		{workitems.ErrAlreadyOwner, http.StatusConflict, "already_owner"},
		{workitems.ErrAlreadyOwned, http.StatusConflict, "already_owned"},
		{workitems.ErrClosed, http.StatusConflict, "item_closed"},
		{workitems.ErrListArchived, http.StatusConflict, "list_archived"},
		{lists.ErrInvalid, http.StatusBadRequest, "invalid_request"},
		{lists.ErrBadPerson, http.StatusUnprocessableEntity, "invalid_recipient"},
		{lists.ErrBadUnit, http.StatusUnprocessableEntity, "invalid_unit"},
		{lists.ErrLastOwner, http.StatusConflict, "last_list_owner"},
		{lists.ErrManagedByUnit, http.StatusConflict, "managed_by_unit"},
		{offers.ErrAlreadyOwner, http.StatusConflict, "already_owner"},
		{offers.ErrItemClosed, http.StatusConflict, "item_closed"},
		{offers.ErrOfferExists, http.StatusConflict, "offer_exists"},
		{offers.ErrNotPending, http.StatusConflict, "offer_not_pending"},
		{offers.ErrRefReused, http.StatusConflict, "client_ref_reused"},
		{offers.ErrRateLimited, http.StatusTooManyRequests, "rate_limited"},
		{people.ErrUnavailable, http.StatusServiceUnavailable, "org_unavailable"},
		{people.ErrNotFound, http.StatusNotFound, "not_found"},
	} {
		if errors.Is(err, m.target) {
			return m.status, m.code, m.target.Error()
		}
	}
	return http.StatusInternalServerError, "internal_error", "something went wrong"
}

// limit reads a page size. A bad value answers 400 and returns false.
func limit(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(cmp.Or(r.URL.Query().Get("limit"), strconv.Itoa(defaultLimit)))
	if err != nil || n < 1 || n > maxLimit {
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 200")
		return 0, false
	}
	return n, true
}

func (s *Server) handleCreateItem(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	var req struct {
		workitems.NewItem
		Send *offers.Send `json:"send"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	var plan offers.Plan
	var after func(pgx.Tx, workitems.Item) error
	if req.Send != nil {
		var err error
		if plan, err = s.Offers.Plan(r.Context(), c, req.Send); err != nil {
			failure(w, r, err)
			return
		}
		after = func(tx pgx.Tx, item workitems.Item) error {
			_, err := s.Offers.SendIn(r.Context(), tx, c, plan, item.ID, *req.Send)
			return err
		}
	}
	item, err := s.Items.Create(r.Context(), c, req.NewItem, after)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) handleListItems(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	n, ok := limit(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	items, next, err := s.Items.List(r.Context(), c, workitems.Filter{
		View: q.Get("view"), Status: q.Get("status"), ListID: q.Get("list"), Cursor: q.Get("cursor"), Limit: n,
	})
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (s *Server) handleGetItem(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	item, err := s.Items.View(r.Context(), c, r.PathValue("id"))
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleUpdateItem(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	var patch workitems.Patch
	if !decodeJSON(w, r, &patch) {
		return
	}
	item, err := s.Items.Update(r.Context(), c, r.PathValue("id"), patch)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleDeleteItem(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	if err := s.Items.Delete(r.Context(), c, r.PathValue("id")); err != nil {
		failure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleItemEvents(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	n, ok := limit(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := s.Items.View(r.Context(), c, id); err != nil {
		failure(w, r, err)
		return
	}
	events, next, err := s.Activity.History(r.Context(), c.TenantID, id, r.URL.Query().Get("cursor"), n)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "next_cursor": next})
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	item, err := s.Items.Claim(r.Context(), c, r.PathValue("id"))
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleStopOwning(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	if err := s.Items.StopOwning(r.Context(), c, r.PathValue("id")); err != nil {
		failure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddCollaborator(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	if err := s.Items.AddCollaborator(r.Context(), c, r.PathValue("id"), r.PathValue("user")); err != nil {
		failure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveCollaborator(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	if err := s.Items.RemoveCollaborator(r.Context(), c, r.PathValue("id"), r.PathValue("user")); err != nil {
		failure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListComments(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	n, ok := limit(w, r)
	if !ok {
		return
	}
	comments, next, err := s.Items.Comments(r.Context(), c, r.PathValue("id"), r.URL.Query().Get("cursor"), n)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"comments": comments, "next_cursor": next})
}

func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	var req struct {
		Body string `json:"body"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	comment, err := s.Items.AddComment(r.Context(), c, r.PathValue("id"), req.Body)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, comment)
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	var send offers.Send
	if !decodeJSON(w, r, &send) {
		return
	}
	offer, err := s.Offers.Send(r.Context(), c, r.PathValue("id"), send)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, offer)
}

func (s *Server) handleListOffers(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	n, ok := limit(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, next, err := s.Offers.List(r.Context(), c, q.Get("box"), q.Get("cursor"), n)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"offers": list, "next_cursor": next})
}

func (s *Server) handleAccept(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	offer, err := s.Offers.Accept(r.Context(), c, r.PathValue("id"))
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, offer)
}

func (s *Server) handleDecline(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	var req struct {
		Note string `json:"note"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	offer, err := s.Offers.Decline(r.Context(), c, r.PathValue("id"), req.Note)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, offer)
}

func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	offer, err := s.Offers.Withdraw(r.Context(), c, r.PathValue("id"))
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, offer)
}

func (s *Server) handleListLists(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	n, ok := limit(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	found, next, err := s.Lists.Mine(r.Context(), c, q.Get("archived") == "true", q.Get("cursor"), n)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lists": found, "next_cursor": next})
}

func (s *Server) handleCreateList(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	list, err := s.Lists.Create(r.Context(), c, req.Name)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, list)
}

func (s *Server) handleGetList(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	list, err := s.Lists.Read(r.Context(), c, r.PathValue("id"))
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleUpdateList(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	var patch lists.Patch
	if !decodeJSON(w, r, &patch) {
		return
	}
	list, err := s.Lists.Update(r.Context(), c, r.PathValue("id"), patch)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handlePutListMember(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	var req struct {
		Role string `json:"role"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	if err := s.Lists.PutMember(r.Context(), c, r.PathValue("id"), r.PathValue("user"), req.Role); err != nil {
		failure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveListMember(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	if err := s.Lists.RemoveMember(r.Context(), c, r.PathValue("id"), r.PathValue("user")); err != nil {
		failure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	settings, err := s.Items.Settings(r.Context(), c.TenantID)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

// admin confirms the admin role with the auth service on each request, and
// answers for the caller if it can't. A failed read is never a yes.
func (s *Server) admin(w http.ResponseWriter, r *http.Request, c policy.Caller) bool {
	member, err := s.Org.Member(r.Context(), c.TenantID, c.UserID)
	if errors.Is(err, people.ErrNotFound) || (err == nil && !(member.Active() && member.Role == "admin")) {
		failure(w, r, policy.ErrForbidden)
		return false
	}
	if err != nil {
		failure(w, r, err)
		return false
	}
	return true
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	var next workitems.Settings
	if !decodeJSON(w, r, &next) {
		return
	}
	if !s.admin(w, r, c) {
		return
	}
	if err := s.Items.SetSettings(r.Context(), c, next); err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, next)
}

func (s *Server) handleItemQueue(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	n, ok := limit(w, r)
	if !ok || !s.admin(w, r, c) {
		return
	}
	queued, next, err := s.Items.Queue(r.Context(), c.TenantID, r.URL.Query().Get("cursor"), n)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": queued, "next_cursor": next})
}

func (s *Server) handleReassign(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	var req struct {
		To string `json:"to"`
	}
	if !decodeJSON(w, r, &req) || !s.admin(w, r, c) {
		return
	}
	item, err := s.Items.Reassign(r.Context(), c, r.PathValue("item"), req.To)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": item.ID, "title": item.Title, "owners": item.Owners})
}

func (s *Server) handleListQueue(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	n, ok := limit(w, r)
	if !ok || !s.admin(w, r, c) {
		return
	}
	queued, next, err := s.Lists.Queue(r.Context(), c.TenantID, r.URL.Query().Get("cursor"), n)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lists": queued, "next_cursor": next})
}

func (s *Server) handleSetListOwner(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	var req struct {
		To string `json:"to"`
	}
	if !decodeJSON(w, r, &req) || !s.admin(w, r, c) {
		return
	}
	if err := s.Lists.SetOwner(r.Context(), c, r.PathValue("id"), req.To); err != nil {
		failure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	n, ok := limit(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	members, next, err := s.Org.Search(r.Context(), c.TenantID, q.Get("q"), q.Get("unit"), q.Get("cursor"), n)
	if err != nil {
		failure(w, r, err)
		return
	}
	found := make([]person, len(members))
	for i, m := range members {
		found[i] = personOf(m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"people": found, "next_cursor": next})
}

// person is what one member may read about another.
type person struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	OrgUnitID   string `json:"org_unit_id,omitempty"`
}

func personOf(m people.Member) person {
	return person{UserID: m.UserID, DisplayName: m.DisplayName, OrgUnitID: m.OrgUnitID}
}

func (s *Server) handleUnits(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	units, err := s.Org.Units(r.Context(), c.TenantID)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"units": units})
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	n, ok := limit(w, r)
	if !ok {
		return
	}
	list, next, err := s.Activity.Since(r.Context(), c, r.URL.Query().Get("after"), n)
	if err != nil {
		failure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": list, "cursor": next})
}

func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request, c policy.Caller) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.Activity.MarkRead(r.Context(), c, req.IDs); err != nil {
		failure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
