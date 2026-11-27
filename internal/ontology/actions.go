package ontology

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	onto "github.com/xiant10/t10-modkit/ontology"

	"t10-collab/internal/offers"
	"t10-collab/internal/policy"
	"t10-collab/internal/types"
	"t10-collab/internal/workitems"
)

// The actions, each over the service its route calls and the policy check that
// route makes. Collab's rule is that an assistant reads and drafts, and only the
// person sends: a task the person keeps to themselves an assistant may make,
// and the person can undo it; anything other people see waits for the person to
// confirm. Accepting, declining, claiming, deleting and administration aren't
// actions, so no assistant can even draft them.

// undoWindow is how long the person can undo a task an assistant made.
const undoWindow = 24 * time.Hour

func str(in onto.Input, name string) string {
	s, _ := in.Params[name].(string)
	return s
}

func strPtr(in onto.Input, name string) *string {
	if s, ok := in.Params[name].(string); ok {
		return &s
	}
	return nil
}

// userID is the ID in an auth:user reference.
func userID(ref string) string {
	return strings.TrimPrefix(ref, "auth:user:")
}

// open refuses every action on an item read only as a manager: that view is
// read-only, as the routes' is.
func open(r item, can bool) string {
	if r.Manager || !can {
		return "forbidden"
	}
	return ""
}

func (o *Ontology) declareActions() {
	o.declareCreate()
	edit := map[string]onto.Param{
		"title":          {Type: onto.String, Name: "Title", NameZh: "标题"},
		"description":    {Type: onto.Text, Name: "Description", NameZh: "说明"},
		"priority":       {Type: onto.Enum, Name: "Priority", NameZh: "优先级", Values: priorities},
		"due":            {Type: onto.Timestamp, Name: "Due", NameZh: "截止时间"},
		"clear_deadline": {Type: onto.Boolean, Name: "No due date", NameZh: "取消截止时间"},
	}
	onto.AddAction(o.reg, onto.ActionType[policy.Caller, item]{
		Name: "work_item.edit", On: "work_item", Label: "Edit", LabelZh: "修改",
		Description: "Change a task's title, description, priority or due date. Everyone on the task sees the change, so the user confirms it.",
		Params:      edit, Permission: policy.ActionWrite, Agent: onto.Propose,
		Available: func(_ context.Context, _ policy.Caller, r item) string { return open(r, r.Relation.CanEdit()) },
		Validate: func(_ context.Context, _ policy.Caller, _ item, in onto.Input) []onto.FieldError {
			if len(in.Params) == 0 {
				return []onto.FieldError{{Param: "params", Code: "required"}}
			}
			if _, ok := in.Params["due"]; ok && in.Params["clear_deadline"] == true {
				return []onto.FieldError{{Param: "clear_deadline", Code: "conflict"}}
			}
			return nil
		},
		Apply: func(ctx context.Context, c policy.Caller, r item, in onto.Input) (onto.Result, error) {
			p := workitems.Patch{Version: int(in.Revision), Title: strPtr(in, "title"), Description: strPtr(in, "description"), Priority: strPtr(in, "priority")}
			if due, ok := in.Params["due"].(time.Time); ok {
				p.DueAt = workitems.OptionalTime{Set: true, Time: &due}
			} else if in.Params["clear_deadline"] == true {
				p.DueAt = workitems.OptionalTime{Set: true}
			}
			_, err := o.d.Items.Update(ctx, c, r.ID, p)
			return onto.Result{ID: r.ID}, err
		},
	})

	onto.AddAction(o.reg, onto.ActionType[policy.Caller, item]{
		Name: "work_item.set_status", On: "work_item", Label: "Set status", LabelZh: "改状态",
		Description: "Move a plain task to todo, doing, done or cancelled. An item of a module's type moves by its own steps instead: work_item.move.",
		Params:      map[string]onto.Param{"to": {Type: onto.Enum, Name: "To", NameZh: "改为", Values: statuses, Required: true}},
		Permission:  policy.ActionWrite, Agent: onto.Propose,
		Available: func(_ context.Context, _ policy.Caller, r item) string {
			if reason := open(r, r.Relation.CanEdit()); reason != "" {
				return reason
			}
			if r.State != "" {
				return "wrong_state"
			}
			return ""
		},
		Validate: func(_ context.Context, _ policy.Caller, r item, in onto.Input) []onto.FieldError {
			to := str(in, "to")
			switch {
			case to == r.Status:
				return []onto.FieldError{{Param: "to", Code: "same_status"}}
			case (to == workitems.StatusCancelled || r.Status == workitems.StatusCancelled) && !r.Relation.CanCancel():
				return []onto.FieldError{{Code: "forbidden"}}
			}
			return nil
		},
		Apply: func(ctx context.Context, c policy.Caller, r item, in onto.Input) (onto.Result, error) {
			_, err := o.d.Items.Update(ctx, c, r.ID, workitems.Patch{Version: int(in.Revision), Status: strPtr(in, "to")})
			return onto.Result{ID: r.ID}, err
		},
	})

	onto.AddAction(o.reg, onto.ActionType[policy.Caller, item]{
		Name: "work_item.move", On: "work_item", Label: "Move", LabelZh: "推进",
		Description: "Take one of the steps a module's item type allows from the item's state, such as answering a help request. The company's steps are move's values in work_describe; the item's actions say whether a move is open to the user now.",
		Params: map[string]onto.Param{
			"move": {Type: onto.Enum, Name: "Step", NameZh: "步骤", Required: true, Label: true},
			"note": {Type: onto.Text, Name: "Note", NameZh: "备注"},
		},
		Permission: policy.ActionWrite, Agent: onto.Propose,
		Available: func(_ context.Context, _ policy.Caller, r item) string {
			if r.Manager {
				return "forbidden"
			}
			if r.State == "" || len(workitems.Moves(r.t, r.Item, r.Relation)) == 0 {
				return "wrong_state"
			}
			return ""
		},
		Validate: func(_ context.Context, _ policy.Caller, r item, in onto.Input) []onto.FieldError {
			for _, m := range workitems.Moves(r.t, r.Item, r.Relation) {
				if m.ID == str(in, "move") {
					return nil
				}
			}
			return []onto.FieldError{{Param: "move", Code: "not_allowed"}}
		},
		Apply: func(ctx context.Context, c policy.Caller, r item, in onto.Input) (onto.Result, error) {
			_, err := o.d.Items.Transition(ctx, c, r.ID, workitems.TransitionRequest{Transition: str(in, "move"), Note: str(in, "note"), Version: int(in.Revision)})
			return onto.Result{ID: r.ID}, err
		},
	})

	onto.AddAction(o.reg, onto.ActionType[policy.Caller, item]{
		Name: "work_item.comment", On: "work_item", Label: "Comment", LabelZh: "评论",
		Description: "Comment on a task. Everyone on it is told at once, so it can't be taken back: the user confirms it first. Write @usr_… for someone the user should mention.",
		Params:      map[string]onto.Param{"body": {Type: onto.Text, Name: "Comment", NameZh: "内容", Required: true}},
		Permission:  policy.ActionWrite, Agent: onto.Propose,
		Available: func(_ context.Context, _ policy.Caller, r item) string { return open(r, r.Relation.CanRead()) },
		Apply: func(ctx context.Context, c policy.Caller, r item, in onto.Input) (onto.Result, error) {
			_, err := o.d.Items.AddComment(ctx, c, r.ID, str(in, "body"), nil)
			return onto.Result{ID: r.ID}, err
		},
	})

	onto.AddAction(o.reg, onto.ActionType[policy.Caller, item]{
		Name: "work_item.link", On: "work_item", Label: "Add a link", LabelZh: "加链接",
		Description: "Add a link to where the task's work is, such as a document or pull request. https only.",
		Params: map[string]onto.Param{
			"url":   {Type: onto.String, Name: "Link", NameZh: "链接", Required: true},
			"title": {Type: onto.String, Name: "Title", NameZh: "标题", Required: true},
			"kind":  {Type: onto.Enum, Name: "Kind", NameZh: "类型", Values: []string{"pull_request", "commit", "build", "document", "other"}},
		},
		Permission: policy.ActionWrite, Agent: onto.Propose,
		Available: func(_ context.Context, _ policy.Caller, r item) string { return open(r, r.Relation.CanEdit()) },
		Apply: func(ctx context.Context, c policy.Caller, r item, in onto.Input) (onto.Result, error) {
			kind := str(in, "kind")
			if kind == "" {
				kind = "other"
			}
			_, err := o.d.Items.AddLink(ctx, c, r.ID, workitems.NewLink{URL: str(in, "url"), Title: str(in, "title"), Kind: kind})
			return onto.Result{ID: r.ID}, err
		},
	})

	onto.AddAction(o.reg, onto.ActionType[policy.Caller, item]{
		Name: "work_item.send", On: "work_item", Label: "Send", LabelZh: "转交",
		Description: "Send the user's task to a colleague, with a note. Down the user's reporting line it's assigned; otherwise it's an offer the colleague accepts or declines. transfer hands it over; share adds them beside the user. Find colleagues with work_people.",
		Params: map[string]onto.Param{
			"to":   {Type: onto.Ref, To: "auth:user", Name: "To", NameZh: "转给", Required: true},
			"mode": {Type: onto.Enum, Name: "Mode", NameZh: "方式", Values: []string{offers.ModeTransfer, offers.ModeShare}},
			"note": {Type: onto.Text, Name: "Note", NameZh: "附言"},
		},
		Permission: policy.ActionSend, Agent: onto.Propose,
		Available: func(_ context.Context, _ policy.Caller, r item) string {
			if reason := open(r, r.Relation.CanSend()); reason != "" {
				return reason
			}
			if r.Closed() {
				return "wrong_state"
			}
			return ""
		},
		Apply: func(ctx context.Context, c policy.Caller, r item, in onto.Input) (onto.Result, error) {
			_, err := o.d.Offers.Send(ctx, c, r.ID, offers.Send{To: userID(str(in, "to")), Mode: str(in, "mode"), Note: str(in, "note")})
			return onto.Result{ID: r.ID}, err
		},
	})
}

// declareCreate is the one undoable action: a task of the person's own, which
// nobody else sees until they share it.
func (o *Ontology) declareCreate() {
	onto.AddAction(o.reg, onto.ActionType[policy.Caller, struct{}]{
		Name: "work_item.create", Label: "New task", LabelZh: "新建任务",
		Description: "Make a task owned by the user, which nobody else sees until they share or send it. The user can undo it for a day, until someone else is on it.",
		Params: map[string]onto.Param{
			"title":       {Type: onto.String, Name: "Title", NameZh: "标题", Required: true},
			"description": {Type: onto.Text, Name: "Description", NameZh: "说明"},
			"priority":    {Type: onto.Enum, Name: "Priority", NameZh: "优先级", Values: priorities},
			"due":         {Type: onto.Timestamp, Name: "Due", NameZh: "截止时间"},
			"subject":     {Type: onto.String, Name: "About", NameZh: "关于"},
		},
		Permission: policy.ActionWrite, Idempotent: true, Undo: true, UndoWindow: undoWindow, Agent: onto.Undoable,
		Validate: func(_ context.Context, _ policy.Caller, _ struct{}, in onto.Input) []onto.FieldError {
			if types.TaskType.CheckSubject(str(in, "subject")) != nil {
				return []onto.FieldError{{Param: "subject", Code: "wrong_type"}}
			}
			return nil
		},
		Apply: func(ctx context.Context, c policy.Caller, _ struct{}, in onto.Input) (onto.Result, error) {
			n := workitems.NewItem{Title: str(in, "title"), Description: str(in, "description"), Priority: str(in, "priority"), SubjectRef: str(in, "subject")}
			if due, ok := in.Params["due"].(time.Time); ok {
				n.DueAt = &due
			}
			created, err := o.d.Items.Create(ctx, c, n, nil)
			if err != nil {
				return onto.Result{}, err
			}
			return onto.Result{Type: "work_item", ID: created.ID, Undo: map[string]string{"item": created.ID}}, nil
		},
		Revert: func(ctx context.Context, c policy.Caller, u onto.Undoing) (onto.Result, error) {
			var payload struct {
				Item string `json:"item"`
			}
			if err := json.Unmarshal(u.Payload, &payload); err != nil {
				return onto.Result{}, err
			}
			if err := o.d.Items.UndoCreate(ctx, c, payload.Item); err != nil {
				return onto.Result{}, err
			}
			return onto.Result{Type: "work_item", ID: payload.Item}, nil
		},
	})
}
