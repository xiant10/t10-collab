package ontology

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/xiant10/t10-modkit/ids"
)

type agentKey struct{}

// IsAgent reports whether a call came through the agent tools, so a module's
// View or Search may treat it as an agent's.
func IsAgent(ctx context.Context) bool { v, _ := ctx.Value(agentKey{}).(bool); return v }

// Masked is what an agent sees in place of a sensitive value.
const Masked = "***"

// ActionState is one entry of an object's actions list.
type ActionState struct {
	Action  string `json:"action"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// ObjectAnswer is one object as the routes and tools answer it.
type ObjectAnswer struct {
	Ref        string              `json:"ref"`
	Type       string              `json:"type"`
	Title      string              `json:"title"`
	Revision   int64               `json:"revision"`
	Properties map[string]any      `json:"properties"`
	Actions    []ActionState       `json:"actions"`
	Link       map[string]any      `json:"link,omitempty"`
	Sources    map[string][]string `json:"sources,omitempty"`
}

// Resolved is one object in a resolve answer.
type Resolved struct {
	Ref     string        `json:"ref"`
	Type    string        `json:"type"`
	Title   string        `json:"title"`
	Actions []ActionState `json:"actions"`
}

// SearchAnswer is a page of objects.
type SearchAnswer struct {
	Objects    []ObjectAnswer `json:"objects"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// ActionRequest is the body of validate and apply.
type ActionRequest struct {
	Object   string          `json:"object,omitempty"`
	Revision int64           `json:"revision,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"`
}

// UndoRef is how an applied action is undone.
type UndoRef struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ApplyAnswer is what apply, confirm and undo answer.
type ApplyAnswer struct {
	Object *ObjectAnswer `json:"object"`
	Undo   *UndoRef      `json:"undo,omitempty"`
}

// UndoAnswer is an applied action as the person's list of what they can still
// undo shows it. Agent is set when an agent applied it through its _act tool.
type UndoAnswer struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	Object    string    `json:"object,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DoneAnswer is one thing an agent did for the person, as GET /v1/done shows
// it. UndoID and UndoExpiresAt are there while it can still be undone.
type DoneAnswer struct {
	ID            string     `json:"id"`
	Action        string     `json:"action"`
	Object        string     `json:"object,omitempty"`
	Agent         string     `json:"agent"`
	CreatedAt     time.Time  `json:"created_at"`
	UndoID        string     `json:"undo_id,omitempty"`
	UndoExpiresAt *time.Time `json:"undo_expires_at,omitempty"`
}

// ProposalAnswer is a draft as the person's list shows it.
type ProposalAnswer struct {
	ID        string          `json:"id"`
	Action    string          `json:"action"`
	Object    string          `json:"object,omitempty"`
	Revision  int64           `json:"revision,omitempty"`
	Params    json.RawMessage `json:"params"`
	Agent     string          `json:"agent"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// SplitRef splits <module>:<type>:<id>.
func SplitRef(ref string) (module, objectType, id string, ok bool) {
	parts := strings.SplitN(ref, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func (r *Registry[C]) ref(objectType, id string) string {
	return r.opts.Module + ":" + objectType + ":" + id
}

func (r *Registry[C]) objectType(name string) (*objectEntry[C], *Error) {
	t, ok := r.types[name]
	if !ok {
		return nil, Fail(http.StatusNotFound, "unknown_type", "the descriptor declares no type "+name)
	}
	return t, nil
}

func (r *Registry[C]) states(ctx context.Context, c C, objectType string, rec any) []ActionState {
	out := []ActionState{}
	for _, name := range r.actionOrder {
		a := r.actions[name]
		if a.info.On != objectType {
			continue
		}
		out = append(out, r.state(ctx, c, name, a, rec))
	}
	return out
}

func (r *Registry[C]) state(ctx context.Context, c C, name string, a *actionEntry[C], rec any) ActionState {
	if !r.opts.Can(c, a.info.Permission) {
		return ActionState{Action: name, Reason: "forbidden"}
	}
	if a.available != nil {
		if reason := a.available(ctx, c, rec); reason != "" {
			return ActionState{Action: name, Reason: reason}
		}
	}
	return ActionState{Action: name, Allowed: true}
}

// maskTitle is a title as an agent sees it: masked when its property is sensitive,
// such as a person's name.
func maskTitle(title string, p Property) string {
	if p.Sensitivity != "" && title != "" {
		return Masked
	}
	return title
}

// asError is a module's error as the contract's, through the module's own Errors.
func (r *Registry[C]) asError(err error) *Error {
	var e *Error
	if err != nil && !errors.As(err, &e) && r.opts.Errors != nil {
		err = r.opts.Errors(err)
	}
	return asError(err)
}

// mask replaces every sensitive value with Masked, for an agent.
func mask(values map[string]any, props map[string]Property) map[string]any {
	out := make(map[string]any, len(values))
	for k, v := range values {
		if props[k].Sensitivity != "" && v != nil {
			v = Masked
		}
		out[k] = v
	}
	return out
}

func (r *Registry[C]) answer(ctx context.Context, c C, d Descriptor, objectType string, rec any) ObjectAnswer {
	t := r.types[objectType]
	o := t.view(ctx, c, rec)
	props := o.Properties
	if props == nil {
		props = map[string]any{}
	}
	title := o.Title
	if IsAgent(ctx) {
		all := d.Properties(objectType)
		props = mask(props, all)
		title = maskTitle(title, all[t.info.Title])
	}
	return ObjectAnswer{Ref: r.ref(objectType, o.ID), Type: objectType, Title: title, Revision: o.Revision, Properties: props, Actions: r.states(ctx, c, objectType, rec)}
}

func (r *Registry[C]) describe(ctx context.Context, c C) (Descriptor, *Error) {
	d, err := r.Describe(ctx, c)
	if err != nil {
		return Descriptor{}, r.asError(err)
	}
	return d, nil
}

// GetObject answers one object, with the sources of a derived property when
// sources names one.
func (r *Registry[C]) GetObject(ctx context.Context, c C, objectType, id, sources string) (ObjectAnswer, error) {
	t, e := r.objectType(objectType)
	if e != nil {
		return ObjectAnswer{}, e
	}
	if sources != "" && !t.info.Properties[sources].Sources {
		return ObjectAnswer{}, invalid("%s.%s has no sources", objectType, sources)
	}
	d, e := r.describe(ctx, c)
	if e != nil {
		return ObjectAnswer{}, e
	}
	rec, err := t.get(ctx, c, id)
	if err != nil {
		return ObjectAnswer{}, r.asError(err)
	}
	out := r.answer(ctx, c, d, objectType, rec)
	if sources != "" {
		refs, err := t.sources(ctx, c, rec, sources)
		if err != nil {
			return ObjectAnswer{}, r.asError(err)
		}
		out.Sources = map[string][]string{sources: nonNil(refs)}
	}
	return out, nil
}

// Search answers a page of objects for a search body.
func (r *Registry[C]) Search(ctx context.Context, c C, objectType string, body []byte) (SearchAnswer, error) {
	t, e := r.objectType(objectType)
	if e != nil {
		return SearchAnswer{}, e
	}
	d, e := r.describe(ctx, c)
	if e != nil {
		return SearchAnswer{}, e
	}
	props := d.Properties(objectType)
	q, e := DecodeSearch(body, props, t.defaultSort, IsAgent(ctx))
	if e != nil {
		return SearchAnswer{}, e
	}
	rows, err := t.search(ctx, c, q)
	if err != nil {
		return SearchAnswer{}, r.asError(err)
	}
	out := SearchAnswer{Objects: []ObjectAnswer{}}
	more := len(rows) > q.Limit
	if more {
		rows = rows[:q.Limit]
	}
	var last Object
	for _, rec := range rows {
		out.Objects = append(out.Objects, r.answer(ctx, c, d, objectType, rec))
		last = t.view(ctx, c, rec)
	}
	if more {
		var value any
		if q.Sort.Property != "" {
			value = last.Properties[q.Sort.Property]
		}
		out.NextCursor = encodeCursor(q.Sort, value, last.ID)
	}
	return out, nil
}

type linkCursor struct {
	Link string `json:"l"`
	ID   string `json:"id"`
}

// Follow answers a page of a link's objects, under the same scope and masking.
func (r *Registry[C]) Follow(ctx context.Context, c C, objectType, id, link string, limit int, cursor string) (SearchAnswer, error) {
	t, e := r.objectType(objectType)
	if e != nil {
		return SearchAnswer{}, e
	}
	l, ok := t.info.Links[link]
	if !ok {
		return SearchAnswer{}, Fail(http.StatusNotFound, "unknown_link", objectType+" declares no link "+link)
	}
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 1 || limit > MaxLimit {
		return SearchAnswer{}, invalid("limit is from 1 to %d", MaxLimit)
	}
	p := Page{Limit: limit}
	if cursor != "" {
		raw, err := decodeBase64JSON[linkCursor](cursor)
		if err != nil || raw.Link != link || raw.ID == "" {
			return SearchAnswer{}, invalid("the cursor isn't one this module issued for this link")
		}
		p.After = raw.ID
	}
	d, e := r.describe(ctx, c)
	if e != nil {
		return SearchAnswer{}, e
	}
	rec, err := t.get(ctx, c, id)
	if err != nil {
		return SearchAnswer{}, r.asError(err)
	}
	rows, err := t.follow(ctx, c, rec, link, p)
	if err != nil {
		return SearchAnswer{}, r.asError(err)
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	linkProps := d.LinkProperties(objectType, link)
	module, target, _ := strings.Cut(l.To, ":")
	out := SearchAnswer{Objects: []ObjectAnswer{}}
	for _, row := range rows {
		values := row.Link
		if IsAgent(ctx) {
			values = mask(values, linkProps)
		}
		if module != r.opts.Module {
			out.Objects = append(out.Objects, ObjectAnswer{Ref: row.Ref, Type: target, Properties: map[string]any{}, Actions: []ActionState{}, Link: values})
			continue
		}
		linked, err := r.types[target].get(ctx, c, row.ID)
		if err != nil {
			if e := r.asError(err); !errors.Is(e, ErrNotFound) {
				return SearchAnswer{}, e
			}
			continue
		}
		o := r.answer(ctx, c, d, target, linked)
		o.Link = values
		out.Objects = append(out.Objects, o)
	}
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		after := last.ID
		if after == "" {
			after = last.Ref
		}
		out.NextCursor = encodeBase64JSON(linkCursor{Link: link, ID: after})
	}
	return out, nil
}

// Resolve answers the references the caller can see, in the order given, and
// leaves out the rest with no error.
func (r *Registry[C]) Resolve(ctx context.Context, c C, refs []string) ([]Resolved, error) {
	if len(refs) > MaxLimit {
		return nil, invalid("resolve takes at most %d references", MaxLimit)
	}
	out := []Resolved{}
	d, e := r.describe(ctx, c)
	if e != nil {
		return nil, e
	}
	for _, ref := range refs {
		module, objectType, id, ok := SplitRef(ref)
		t, known := r.types[objectType]
		if !ok || module != r.opts.Module || !known {
			continue
		}
		rec, err := t.get(ctx, c, id)
		if err != nil {
			if e := r.asError(err); !errors.Is(e, ErrNotFound) {
				return nil, e
			}
			continue
		}
		o := t.view(ctx, c, rec)
		if IsAgent(ctx) {
			o.Title = maskTitle(o.Title, d.Properties(objectType)[t.info.Title])
		}
		out = append(out, Resolved{Ref: ref, Type: objectType, Title: o.Title, Actions: r.states(ctx, c, objectType, rec)})
	}
	return out, nil
}

type prepared[C any] struct {
	name    string
	action  *actionEntry[C]
	rec     any
	current int64
	params  map[string]any
}

// prepare checks an action as validate and apply do. A refusal of the whole
// request (unknown action, invisible object) is the *Error; the rest are field
// errors, with Param "" for forbidden, wrong_state and stale_revision.
func (r *Registry[C]) prepare(ctx context.Context, c C, name string, req ActionRequest, needRevision bool) (*prepared[C], []FieldError, *Error) {
	a, ok := r.actions[name]
	if !ok {
		return nil, nil, Fail(http.StatusNotFound, "unknown_action", "the descriptor declares no action "+name)
	}
	p := &prepared[C]{name: name, action: a}
	if a.info.On != "" {
		module, objectType, id, ok := SplitRef(req.Object)
		if !ok || module != r.opts.Module || objectType != a.info.On {
			return nil, nil, invalid("object must be a %s:%s reference", r.opts.Module, a.info.On)
		}
		rec, err := r.types[objectType].get(ctx, c, id)
		if err != nil {
			return nil, nil, r.asError(err)
		}
		p.rec = rec
		p.current = r.types[objectType].view(ctx, c, rec).Revision
	} else if req.Object != "" {
		return nil, nil, invalid("%s creates an object, so it takes no object", name)
	}
	// A stale revision is told first: the caller re-reads before anything else.
	if a.info.On != "" && req.Revision != 0 && req.Revision != p.current {
		return p, []FieldError{{Code: ErrStaleRevision.Code}}, nil
	}
	if s := r.state(ctx, c, name, a, p.rec); !s.Allowed {
		return p, []FieldError{{Code: s.Reason}}, nil
	}
	d, e := r.describe(ctx, c)
	if e != nil {
		return nil, nil, e
	}
	params, errs := decodeParams(req.Params, d.Params(name))
	p.params = params
	if a.info.On != "" {
		if req.Revision == 0 && needRevision {
			errs = append(errs, FieldError{Param: "revision", Code: "required"})
		}
	}
	if len(errs) == 0 && a.validate != nil {
		errs = append(errs, a.validate(ctx, c, p.rec, Input{Params: params, Revision: req.Revision})...)
	}
	return p, errs, nil
}

// refusal turns prepare's field errors into apply's answer.
func refusal(errs []FieldError) *Error {
	if len(errs) == 1 && errs[0].Param == "" {
		return reasonError(errs[0].Code)
	}
	e := Fail(http.StatusBadRequest, "invalid_params", "")
	e.Errors = errs
	return e
}

// Validate checks an action and changes nothing.
func (r *Registry[C]) Validate(ctx context.Context, c C, name string, req ActionRequest) ([]FieldError, error) {
	_, errs, e := r.prepare(ctx, c, name, req, false)
	if e != nil {
		return nil, e
	}
	return nonNil(errs), nil
}

func requestHash(name string, req ActionRequest, params map[string]any) string {
	canonical, _ := json.Marshal(struct {
		Action   string         `json:"a"`
		Object   string         `json:"o"`
		Revision int64          `json:"r"`
		Params   map[string]any `json:"p"`
	}{name, req.Object, req.Revision, params})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// replayScope is the action every key is kept under: a key covers the module,
// not one action, so the same key on another action is idempotency_conflict.
const replayScope = "*"

// Apply runs an action for the caller. via is person, agent or proposal.
func (r *Registry[C]) Apply(ctx context.Context, c C, name string, req ActionRequest, in Input) (ApplyAnswer, error) {
	a, ok := r.actions[name]
	if ok && a.info.Idempotent && in.IdempotencyKey == "" {
		return ApplyAnswer{}, Fail(http.StatusBadRequest, "idempotency_key_required", name+" needs an Idempotency-Key")
	}
	who := r.opts.Who(c)
	// Any action given a key replays its answer for it, so an agent's retry of
	// a non-idempotent action never applies it twice.
	keyed := ok && in.IdempotencyKey != "" && r.opts.Undo != nil
	if keyed {
		kept, found, err := r.opts.Undo.GetReplay(ctx, who.Tenant, who.User, replayScope, in.IdempotencyKey)
		if err != nil {
			return ApplyAnswer{}, r.asError(err)
		}
		if found {
			return r.replay(ctx, c, name, req, kept)
		}
	}
	p, errs, e := r.prepare(ctx, c, name, req, true)
	if e != nil {
		return ApplyAnswer{}, e
	}
	if len(errs) > 0 {
		return ApplyAnswer{}, refusal(errs)
	}
	in.Params, in.Revision = p.params, req.Revision
	hash := requestHash(name, req, p.params)
	// The key is claimed before the action runs, so a retry that arrives while
	// this one is still running can't apply it a second time.
	if keyed {
		kept, reserved, err := r.opts.Undo.ReserveReplay(ctx, Replay{Tenant: who.Tenant, User: who.User, Action: replayScope, Key: in.IdempotencyKey, Hash: hash})
		if err != nil {
			return ApplyAnswer{}, r.asError(err)
		}
		if !reserved {
			return r.replay(ctx, c, name, req, kept)
		}
	}
	res, err := p.action.apply(ctx, c, p.rec, in)
	if err != nil {
		// Nothing changed, so the key is free for a retry.
		if keyed {
			_ = r.opts.Undo.ReleaseReplay(context.WithoutCancel(ctx), who.Tenant, who.User, replayScope, in.IdempotencyKey)
		}
		return ApplyAnswer{}, r.asError(err)
	}
	// From here the change is made: a failure keeps the key claimed, and a
	// retry with it answers request_in_progress instead of applying again.
	if res.Type == "" {
		res.Type = a.info.On
	}
	out := ApplyAnswer{Object: r.changed(ctx, c, res.Type, res.ID)}
	if a.info.Undo {
		payload, err := json.Marshal(res.Undo)
		if err != nil {
			return ApplyAnswer{}, r.asError(err)
		}
		now := r.opts.Now()
		entry := UndoEntry{ID: ids.New("undo"), Tenant: who.Tenant, User: who.User, Action: name, Object: r.ref(res.Type, res.ID),
			Payload: payload, CreatedAt: now, ExpiresAt: now.Add(a.window)}
		if in.Via == "agent" {
			entry.Agent = in.Agent
		}
		if err := r.opts.Undo.SaveUndo(ctx, entry); err != nil {
			return ApplyAnswer{}, r.asError(err)
		}
		out.Undo = &UndoRef{ID: entry.ID, ExpiresAt: entry.ExpiresAt}
	}
	if in.Via == "agent" && r.opts.Undo != nil {
		done := DoneEntry{ID: ids.New("done"), Tenant: who.Tenant, User: who.User, Action: name, Object: r.ref(res.Type, res.ID),
			Agent: in.Agent, CreatedAt: r.opts.Now()}
		if out.Undo != nil {
			done.UndoID = out.Undo.ID
		}
		if err := r.opts.Undo.SaveDone(ctx, done); err != nil {
			return ApplyAnswer{}, r.asError(err)
		}
	}
	if keyed {
		encoded, _ := json.Marshal(out)
		if err := r.opts.Undo.FinishReplay(ctx, Replay{Tenant: who.Tenant, User: who.User, Action: replayScope, Key: in.IdempotencyKey, Hash: hash, Answer: encoded}); err != nil {
			return ApplyAnswer{}, r.asError(err)
		}
	}
	return out, nil
}

// changed is the object an apply or undo answers, read again. It is left out
// when the read or the descriptor fails: without the descriptor an agent's
// answer couldn't be masked, and the change is made either way.
func (r *Registry[C]) changed(ctx context.Context, c C, objectType, id string) *ObjectAnswer {
	t, ok := r.types[objectType]
	if !ok || id == "" {
		return nil
	}
	rec, err := t.get(ctx, c, id)
	if err != nil {
		return nil
	}
	d, err := r.Describe(ctx, c)
	if err != nil {
		return nil
	}
	o := r.answer(ctx, c, d, objectType, rec)
	return &o
}

// replay answers a kept answer after checking the person's current rights
// again, and refuses the same key with another body.
func (r *Registry[C]) replay(ctx context.Context, c C, name string, req ActionRequest, kept Replay) (ApplyAnswer, error) {
	a := r.actions[name]
	// The body first, since the key may have been used for another action: the
	// hash names the action, so another one is idempotency_conflict.
	d, e := r.describe(ctx, c)
	if e != nil {
		return ApplyAnswer{}, e
	}
	params, _ := decodeParams(req.Params, d.Params(name))
	if requestHash(name, req, params) != kept.Hash {
		return ApplyAnswer{}, Fail(http.StatusConflict, "idempotency_conflict", "this key was used with another body")
	}
	if !r.opts.Can(c, a.info.Permission) {
		return ApplyAnswer{}, ErrForbidden
	}
	if a.info.On != "" {
		_, objectType, id, _ := SplitRef(req.Object)
		if _, err := r.types[a.info.On].get(ctx, c, id); err != nil || objectType != a.info.On {
			return ApplyAnswer{}, ErrNotFound
		}
	}
	if kept.Answer == nil {
		return ApplyAnswer{}, ErrRequestInProgress
	}
	var out ApplyAnswer
	if err := json.Unmarshal(kept.Answer, &out); err != nil {
		return ApplyAnswer{}, r.asError(err)
	}
	return out, nil
}

// Undo undoes an applied action, for the person it was applied for only.
func (r *Registry[C]) Undo(ctx context.Context, c C, id string) (ApplyAnswer, error) {
	who := r.opts.Who(c)
	if r.opts.Undo == nil {
		return ApplyAnswer{}, ErrNotFound
	}
	entry, err := r.opts.Undo.GetUndo(ctx, who.Tenant, id)
	if err != nil || entry.User != who.User {
		return ApplyAnswer{}, ErrNotFound
	}
	a, ok := r.actions[entry.Action]
	if !ok || a.revert == nil {
		return ApplyAnswer{}, ErrNotFound
	}
	if entry.Reverted {
		return ApplyAnswer{}, ErrUndone
	}
	// An undo that started and never finished (interrupted, or still running) is
	// run again, past its window too: Revert writes its change at most once.
	started := false
	if !entry.Undone {
		if !r.opts.Now().Before(entry.ExpiresAt) {
			return ApplyAnswer{}, Fail(http.StatusConflict, "undo_expired", "the time to undo it has run out")
		}
		err := r.opts.Undo.SetUndone(ctx, who.Tenant, id, true)
		if err != nil && !errors.Is(err, ErrUndone) {
			return ApplyAnswer{}, r.asError(err)
		}
		started = err == nil
	}
	res, err := a.revert(ctx, c, Undoing{ID: entry.ID, Object: entry.Object, Payload: entry.Payload})
	if errors.Is(err, ErrUndone) {
		// Its change was written before: the undo is done.
		_ = r.opts.Undo.FinishUndo(context.WithoutCancel(ctx), who.Tenant, id)
		return ApplyAnswer{}, ErrUndone
	}
	if err != nil {
		if started {
			_ = r.opts.Undo.SetUndone(context.WithoutCancel(ctx), who.Tenant, id, false)
		}
		return ApplyAnswer{}, r.asError(err)
	}
	if err := r.opts.Undo.FinishUndo(ctx, who.Tenant, id); err != nil {
		return ApplyAnswer{}, r.asError(err)
	}
	_, objectType, objectID, _ := SplitRef(entry.Object)
	if res.Type != "" {
		objectType, objectID = res.Type, res.ID
	}
	return ApplyAnswer{Object: r.changed(ctx, c, objectType, objectID)}, nil
}

// Propose stores a draft for the person an agent acts for, when the action's
// tier allows it and its params validate. It changes nothing else.
func (r *Registry[C]) Propose(ctx context.Context, c C, agent, name string, req ActionRequest) (Proposal, []FieldError, error) {
	a, ok := r.actions[name]
	if !ok {
		return Proposal{}, nil, Fail(http.StatusNotFound, "unknown_action", "the descriptor declares no action "+name)
	}
	if a.info.Agent != Propose && a.info.Agent != Undoable {
		return Proposal{}, nil, ErrNotAllowedForAgent
	}
	p, errs, e := r.prepare(ctx, c, name, req, false)
	if e != nil {
		return Proposal{}, nil, e
	}
	if len(errs) > 0 {
		if len(errs) == 1 && errs[0].Param == "" {
			return Proposal{}, nil, reasonError(errs[0].Code)
		}
		return Proposal{}, errs, nil
	}
	params, _ := json.Marshal(p.params)
	who := r.opts.Who(c)
	now := r.opts.Now()
	draft := Proposal{ID: ids.New("proposal"), Tenant: who.Tenant, User: who.User, Agent: agent, Action: name, Object: req.Object,
		Revision: p.current, Params: params, State: "open", CreatedAt: now, ExpiresAt: now.Add(ProposalLife)}
	if err := r.opts.Proposals.CreateProposal(ctx, draft, ProposalLimit, now); err != nil {
		return Proposal{}, nil, r.asError(err)
	}
	return draft, nil, nil
}

// Proposals are the caller's own open drafts, newest first.
// Undos are the caller's applied actions that can still be undone, newest
// first: what GET /v1/undo answers. An action no longer declared is left out,
// since its undo can't run.
func (r *Registry[C]) Undos(ctx context.Context, c C) ([]UndoAnswer, error) {
	out := []UndoAnswer{}
	if r.opts.Undo == nil {
		return out, nil
	}
	who := r.opts.Who(c)
	list, err := r.opts.Undo.OpenUndos(ctx, who.Tenant, who.User, r.opts.Now())
	if err != nil {
		return nil, r.asError(err)
	}
	for _, u := range list {
		if a, ok := r.actions[u.Action]; !ok || a.revert == nil {
			continue
		}
		out = append(out, UndoAnswer{ID: u.ID, Action: u.Action, Object: u.Object, Agent: u.Agent, CreatedAt: u.CreatedAt, ExpiresAt: u.ExpiresAt})
	}
	return out, nil
}

// DoneWindow is how far back GET /v1/done lists what agents did.
const DoneWindow = 7 * 24 * time.Hour

// Done is what agents applied for the caller in the last DoneWindow, newest
// first, each with its undo ID and window while it can still be undone.
func (r *Registry[C]) Done(ctx context.Context, c C) ([]DoneAnswer, error) {
	out := []DoneAnswer{}
	if r.opts.Undo == nil {
		return out, nil
	}
	who := r.opts.Who(c)
	now := r.opts.Now()
	list, err := r.opts.Undo.RecentDone(ctx, who.Tenant, who.User, now.Add(-DoneWindow), now)
	if err != nil {
		return nil, r.asError(err)
	}
	for _, d := range list {
		answer := DoneAnswer{ID: d.ID, Action: d.Action, Object: d.Object, Agent: d.Agent, CreatedAt: d.CreatedAt}
		if a, ok := r.actions[d.Action]; ok && a.revert != nil && d.UndoID != "" && d.UndoOpen {
			expires := d.UndoExpiresAt
			answer.UndoID, answer.UndoExpiresAt = d.UndoID, &expires
		}
		out = append(out, answer)
	}
	return out, nil
}

func (r *Registry[C]) Proposals(ctx context.Context, c C) ([]ProposalAnswer, error) {
	out := []ProposalAnswer{}
	if r.opts.Proposals == nil {
		return out, nil
	}
	who := r.opts.Who(c)
	list, err := r.opts.Proposals.OpenProposals(ctx, who.Tenant, who.User, r.opts.Now())
	if err != nil {
		return nil, r.asError(err)
	}
	for _, p := range list {
		out = append(out, ProposalAnswer{ID: p.ID, Action: p.Action, Object: p.Object, Revision: p.Revision, Params: p.Params, Agent: p.Agent, CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt})
	}
	return out, nil
}

// ConfirmRequest is a confirm's body: edited params replace the draft's, and
// revision, when given, is the one the person saw. Without it the draft's own
// revision is compared, so a draft whose object changed since is refused with
// stale_revision and stays open, for the person to review again.
type ConfirmRequest struct {
	Params   json.RawMessage `json:"params,omitempty"`
	Revision int64           `json:"revision,omitempty"`
}

func (r *Registry[C]) openProposal(ctx context.Context, c C, id string) (Proposal, error) {
	if r.opts.Proposals == nil {
		return Proposal{}, ErrNotFound
	}
	who := r.opts.Who(c)
	p, err := r.opts.Proposals.GetProposal(ctx, who.Tenant, who.User, id)
	if err != nil {
		return Proposal{}, r.asError(err)
	}
	if p.State != "open" {
		return Proposal{}, ErrProposalClosed
	}
	if !r.opts.Now().Before(p.ExpiresAt) {
		return Proposal{}, Fail(http.StatusConflict, "proposal_expired", "the draft is more than 7 days old")
	}
	return p, nil
}

// Confirm applies a draft under the person's current rights, with via proposal.
func (r *Registry[C]) Confirm(ctx context.Context, c C, id string, body ConfirmRequest) (ApplyAnswer, error) {
	p, err := r.openProposal(ctx, c, id)
	if err != nil {
		return ApplyAnswer{}, err
	}
	params := map[string]json.RawMessage{}
	_ = json.Unmarshal(p.Params, &params)
	if len(body.Params) > 0 {
		edited := map[string]json.RawMessage{}
		if json.Unmarshal(body.Params, &edited) != nil {
			return ApplyAnswer{}, invalid("params must be an object")
		}
		for k, v := range edited {
			params[k] = v
		}
	}
	merged, _ := json.Marshal(params)
	req := ActionRequest{Object: p.Object, Revision: body.Revision, Params: merged}
	if req.Revision == 0 {
		req.Revision = p.Revision
	}
	// A draft kept from before drafts had a revision is confirmed against the current one.
	if a := r.actions[p.Action]; a != nil && a.info.On != "" && req.Revision == 0 {
		_, objectType, objectID, _ := SplitRef(p.Object)
		if t, ok := r.types[objectType]; ok {
			rec, err := t.get(ctx, c, objectID)
			if err != nil {
				return ApplyAnswer{}, r.asError(err)
			}
			req.Revision = t.view(ctx, c, rec).Revision
		}
	}
	who := r.opts.Who(c)
	if err := r.opts.Proposals.DecideProposal(ctx, who.Tenant, who.User, id, "open", "confirmed"); err != nil {
		return ApplyAnswer{}, r.asError(err)
	}
	out, err := r.Apply(ctx, c, p.Action, req, Input{Via: "proposal", Agent: p.Agent, ProposalID: p.ID, IdempotencyKey: "proposal:" + p.ID})
	if err != nil {
		_ = r.opts.Proposals.DecideProposal(context.WithoutCancel(ctx), who.Tenant, who.User, id, "confirmed", "open")
		return ApplyAnswer{}, err
	}
	return out, nil
}

// Dismiss closes a draft with no other effect.
func (r *Registry[C]) Dismiss(ctx context.Context, c C, id string) error {
	if _, err := r.openProposal(ctx, c, id); err != nil {
		return err
	}
	who := r.opts.Who(c)
	return r.opts.Proposals.DecideProposal(ctx, who.Tenant, who.User, id, "open", "dismissed")
}

func encodeBase64JSON(v any) string {
	raw, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeBase64JSON[T any](s string) (T, error) {
	var out T
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}
