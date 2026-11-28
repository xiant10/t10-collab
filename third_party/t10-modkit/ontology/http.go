package ontology

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/xiant10/t10-modkit/httpjson"
	"github.com/xiant10/t10-modkit/telemetry"
)

// Route is one of the contract's routes, for a module to mount on its mux.
type Route struct {
	Pattern string
	Handler http.Handler
}

// maxBody bounds every request body these routes read.
const maxBody = 1 << 20

// Routes are the descriptor, object, action, undo and proposal routes, each
// traced by its pattern. Every one but the descriptor refuses a delegated token.
func (r *Registry[C]) Routes() []Route {
	routes := []Route{
		{"GET /v1/ontology", r.withCaller(true, r.serveDescriptor)},
		{"GET /v1/objects/{type}/{id}", r.withCaller(false, r.serveGet)},
		{"POST /v1/objects/{type}/search", r.withCaller(false, r.serveSearch)},
		{"GET /v1/objects/{type}/{id}/links/{link}", r.withCaller(false, r.serveFollow)},
		{"POST /v1/objects/resolve", r.withCaller(false, r.serveResolve)},
		{"POST /v1/actions/{action}/validate", r.withCaller(false, r.serveValidate)},
		{"POST /v1/actions/{action}/apply", r.withCaller(false, r.serveApply)},
		{"GET /v1/undo", r.withCaller(false, r.serveUndos)},
		{"GET /v1/done", r.withCaller(false, r.serveDone)},
		{"POST /v1/undo/{id}", r.withCaller(false, r.serveUndo)},
	}
	return append(routes, r.ProposalRoutes()...)
}

// ProposalRoutes are only the proposal routes, for a module that mounts them
// apart from the others.
func (r *Registry[C]) ProposalRoutes() []Route {
	return []Route{
		{"GET /v1/proposals", r.withCaller(false, r.serveProposals)},
		{"POST /v1/proposals/{id}/confirm", r.withCaller(false, r.serveConfirm)},
		{"POST /v1/proposals/{id}/dismiss", r.withCaller(false, r.serveDismiss)},
	}
}

// Mount adds routes to a mux, each traced by its pattern.
func Mount(mux *http.ServeMux, routes []Route) {
	for _, route := range routes {
		mux.Handle(route.Pattern, telemetry.Wrap(route.Pattern, route.Handler))
	}
}

// WriteError answers err as the contract's error body.
func WriteError(w http.ResponseWriter, err error) {
	e := asError(err)
	if len(e.Errors) > 0 {
		httpjson.Write(w, e.Status, map[string]any{"error": e.Code, "message": e.Message, "errors": e.Errors})
		return
	}
	httpjson.Error(w, e.Status, e.Code, e.Message)
}

func (r *Registry[C]) withCaller(delegatedOK bool, serve func(http.ResponseWriter, *http.Request, C)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, token, err := r.opts.Authenticate(req)
		if err != nil {
			var e *Error
			if errors.As(err, &e) {
				WriteError(w, e)
			} else {
				httpjson.Error(w, http.StatusUnauthorized, "unauthenticated", "")
			}
			return
		}
		if token == DelegatedToken && !delegatedOK {
			WriteError(w, ErrPersonTokenNeeded)
			return
		}
		serve(w, req, c)
	})
}

func readBody(w http.ResponseWriter, req *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody))
	if err != nil {
		httpjson.Error(w, http.StatusRequestEntityTooLarge, "too_large", "the body is larger than this route takes")
		return nil, false
	}
	return body, true
}

func (r *Registry[C]) serveDescriptor(w http.ResponseWriter, req *http.Request, c C) {
	d, err := r.Describe(req.Context(), c)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, d)
}

func (r *Registry[C]) serveGet(w http.ResponseWriter, req *http.Request, c C) {
	out, err := r.GetObject(req.Context(), c, req.PathValue("type"), req.PathValue("id"), req.URL.Query().Get("sources"))
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (r *Registry[C]) serveSearch(w http.ResponseWriter, req *http.Request, c C) {
	body, ok := readBody(w, req)
	if !ok {
		return
	}
	out, err := r.Search(req.Context(), c, req.PathValue("type"), body)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (r *Registry[C]) serveFollow(w http.ResponseWriter, req *http.Request, c C) {
	limit := 0
	if raw := req.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			WriteError(w, invalid("limit is a number"))
			return
		}
		limit = n
	}
	out, err := r.Follow(req.Context(), c, req.PathValue("type"), req.PathValue("id"), req.PathValue("link"), limit, req.URL.Query().Get("cursor"))
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (r *Registry[C]) serveResolve(w http.ResponseWriter, req *http.Request, c C) {
	var body struct {
		Refs []string `json:"refs"`
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxBody)
	if !httpjson.Decode(w, req, &body) {
		return
	}
	out, err := r.Resolve(req.Context(), c, body.Refs)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"objects": out})
}

func decodeAction(w http.ResponseWriter, req *http.Request) (ActionRequest, bool) {
	var body ActionRequest
	req.Body = http.MaxBytesReader(w, req.Body, maxBody)
	return body, httpjson.Decode(w, req, &body)
}

func (r *Registry[C]) serveValidate(w http.ResponseWriter, req *http.Request, c C) {
	body, ok := decodeAction(w, req)
	if !ok {
		return
	}
	errs, err := r.Validate(req.Context(), c, req.PathValue("action"), body)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"valid": len(errs) == 0, "errors": errs})
}

func (r *Registry[C]) serveApply(w http.ResponseWriter, req *http.Request, c C) {
	body, ok := decodeAction(w, req)
	if !ok {
		return
	}
	out, err := r.Apply(req.Context(), c, req.PathValue("action"), body, Input{Via: "person", IdempotencyKey: req.Header.Get("Idempotency-Key")})
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (r *Registry[C]) serveUndo(w http.ResponseWriter, req *http.Request, c C) {
	out, err := r.Undo(req.Context(), c, req.PathValue("id"))
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (r *Registry[C]) serveUndos(w http.ResponseWriter, req *http.Request, c C) {
	out, err := r.Undos(req.Context(), c)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"undo": out})
}

func (r *Registry[C]) serveDone(w http.ResponseWriter, req *http.Request, c C) {
	out, err := r.Done(req.Context(), c)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"done": out})
}

func (r *Registry[C]) serveProposals(w http.ResponseWriter, req *http.Request, c C) {
	out, err := r.Proposals(req.Context(), c)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"proposals": out})
}

func (r *Registry[C]) serveConfirm(w http.ResponseWriter, req *http.Request, c C) {
	var body ConfirmRequest
	raw, ok := readBody(w, req)
	if !ok {
		return
	}
	if len(raw) > 0 && json.Unmarshal(raw, &body) != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "the body isn't valid JSON for this route")
		return
	}
	out, err := r.Confirm(req.Context(), c, req.PathValue("id"), body)
	if err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (r *Registry[C]) serveDismiss(w http.ResponseWriter, req *http.Request, c C) {
	id := req.PathValue("id")
	if err := r.Dismiss(req.Context(), c, id); err != nil {
		WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]string{"id": id, "status": "dismissed"})
}
