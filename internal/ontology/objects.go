package ontology

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	onto "github.com/xiant10/t10-modkit/ontology"

	"t10-collab/internal/lists"
	"t10-collab/internal/policy"
	"t10-collab/internal/types"
	"t10-collab/internal/workitems"
)

// The object types: work items, lists, comments and offers, read under
// collab's one policy, in SQL before paging.

// item is a work item as the ontology reads it, with its type as it applies in
// the tenant now, for its fields and moves.
type item struct {
	workitems.Standing
	t types.Type
}

// relation is the caller's strongest tie to it.
func (r item) relation() string {
	if r.Manager {
		return policy.TieManager
	}
	return r.Relation.Tie()
}

// The item's statuses and priorities, as the routes take them.
var (
	statuses   = []string{workitems.StatusTodo, workitems.StatusDoing, workitems.StatusDone, workitems.StatusCancelled}
	priorities = []string{"low", "normal", "high"}
)

func (o *Ontology) declareItems() {
	onto.AddObject(o.reg, onto.ObjectType[policy.Caller, item]{
		Name: "work_item", Label: "Task", LabelZh: "任务", Title: "title",
		Description: "Work people and agent teammates give each other: a task, or an item of a module's type such as a help request. Each says where it stands for the user: their relation to it, its pending offers, and the agent teammates working on it.",
		Implements:  []string{"t10:HasDeadline", "t10:Commentable"},
		Properties: map[string]onto.Property{
			"title":          {Type: onto.String, Name: "Title", NameZh: "标题"},
			"description":    {Type: onto.Text, Name: "Description", NameZh: "说明"},
			"status":         {Type: onto.Enum, Name: "Status", NameZh: "状态", Values: statuses},
			"priority":       {Type: onto.Enum, Name: "Priority", NameZh: "优先级", Values: priorities},
			"deadline":       {Type: onto.Timestamp, Name: "Due", NameZh: "截止时间"},
			"type":           {Type: onto.Enum, Name: "Type", NameZh: "类型", Values: []string{types.Task}},
			"state":          {Type: onto.Enum, Name: "State", NameZh: "阶段"},
			"list":           {Type: onto.Ref, To: "collab:list", Name: "List", NameZh: "清单"},
			"subject":        {Type: onto.String, Name: "About", NameZh: "关于"},
			"creator":        {Type: onto.Ref, To: "auth:user", Name: "Created by", NameZh: "创建人"},
			"relation":       {Type: onto.Enum, Name: "Your relation", NameZh: "你和它的关系", Values: policy.Ties, Derived: true},
			"pending_offers": {Type: onto.Integer, Name: "Offers waiting", NameZh: "待接收", Derived: true},
			"working":        {Type: onto.Integer, Name: "Agents working on it", NameZh: "处理中的智能体", Derived: true},
			"comment_count":  {Type: onto.Integer, Name: "Comments", NameZh: "评论数", Derived: true},
			"created_at":     {Type: onto.Timestamp, Name: "Created", NameZh: "创建时间"},
			"updated_at":     {Type: onto.Timestamp, Name: "Updated", NameZh: "更新时间"},
		},
		Links: map[string]onto.Link{
			"owners":        {To: "auth:user", Many: true, Name: "Owners", NameZh: "负责人"},
			"collaborators": {To: "auth:user", Many: true, Name: "Collaborators", NameZh: "协作者"},
			"comments":      {To: "collab:comment", Many: true, Name: "Comments", NameZh: "评论"},
			"offers":        {To: "collab:offer", Many: true, Name: "Offers", NameZh: "转交"},
		},
		DefaultSort: onto.Sort{Property: "created_at", Desc: true},
		Search: func(ctx context.Context, c policy.Caller, q onto.Query) ([]item, error) {
			cols, err := o.itemColumns(ctx, c)
			if err != nil {
				return nil, err
			}
			a := &args{first: workitems.FirstArg}
			filter, err := where(q.Filter, cols, a)
			if err != nil {
				return nil, err
			}
			orderBy, after, err := order(q.Sort, q.After, cols, a)
			if err != nil {
				return nil, err
			}
			return o.items(ctx, c, workitems.Query{Where: filter + " AND " + after, Args: a.values, Order: orderBy, Limit: q.Limit + 1})
		},
		Get: func(ctx context.Context, c policy.Caller, id string) (item, error) {
			found, err := o.items(ctx, c, workitems.Query{Where: fmt.Sprintf("i.id = $%d", workitems.FirstArg), Args: []any{id}, Order: "i.id", Limit: 1})
			if err != nil {
				return item{}, err
			}
			if len(found) == 0 {
				return item{}, onto.ErrNotFound
			}
			return found[0], nil
		},
		View: func(_ context.Context, _ policy.Caller, r item) onto.Object {
			p := map[string]any{
				"title": r.Title, "description": r.Description, "status": r.Status, "priority": r.Priority,
				"deadline": nil, "type": r.Type, "state": nil, "list": nil, "subject": nil, "creator": "auth:user:" + r.CreatorID,
				"relation": r.relation(), "pending_offers": int64(r.PendingOffers), "working": int64(r.Working),
				"comment_count": int64(r.Comments), "created_at": r.CreatedAt, "updated_at": r.UpdatedAt,
			}
			if r.DueAt != nil {
				p["deadline"] = *r.DueAt
			}
			if r.State != "" {
				p["state"] = r.State
			}
			if r.ListID != "" {
				p["list"] = "collab:list:" + r.ListID
			}
			if r.SubjectRef != "" {
				p["subject"] = r.SubjectRef
			}
			for key, f := range r.t.Fields.Properties {
				if v, ok := r.Fields[key]; ok {
					p[fieldProperty(r.t.ID, key)] = fieldValue(f, v)
				}
			}
			return onto.Object{ID: r.ID, Title: r.Title, Revision: int64(r.Version), Properties: p}
		},
		Follow: func(ctx context.Context, c policy.Caller, r item, link string, p onto.Page) ([]onto.LinkRow, error) {
			return o.followItem(ctx, c, r, link, p)
		},
	})
}

// items finds items and resolves each one's type once per page.
func (o *Ontology) items(ctx context.Context, c policy.Caller, q workitems.Query) ([]item, error) {
	found, err := o.d.Items.Find(ctx, c, q)
	if err != nil {
		return nil, err
	}
	typeOf := map[string]types.Type{}
	out := make([]item, len(found))
	for i, st := range found {
		t, ok := typeOf[st.Type]
		if !ok {
			if t, err = o.d.Types.Resolve(ctx, c.TenantID, st.Type); err != nil && !errors.Is(err, types.ErrUnknown) {
				return nil, err
			}
			typeOf[st.Type] = t
		}
		out[i] = item{Standing: st, t: t}
	}
	return out, nil
}

// itemColumns are the properties a search may filter and sort on, with the
// tenant's type fields.
func (o *Ontology) itemColumns(ctx context.Context, c policy.Caller) (map[string]column, error) {
	cols := map[string]column{
		"title":          {expr: "i.title", kind: onto.String},
		"description":    {expr: "i.description", kind: onto.Text, unsorted: true},
		"status":         {expr: "i.status", kind: onto.Enum},
		"priority":       {expr: "i.priority", kind: onto.Enum},
		"deadline":       {expr: "i.due_at", sort: "COALESCE(i.due_at, 'infinity')", null: "'infinity'::timestamptz", kind: onto.Timestamp},
		"type":           {expr: "i.type", kind: onto.Enum},
		"state":          {expr: "i.state", sort: "COALESCE(i.state, '')", null: "''", kind: onto.Enum},
		"list":           {expr: "('collab:list:' || i.list_id)", sort: "COALESCE(i.list_id, '')", null: "''", kind: onto.Ref, unsorted: true},
		"subject":        {expr: "i.subject_ref", sort: "COALESCE(i.subject_ref, '')", null: "''", kind: onto.String},
		"creator":        {expr: "('auth:user:' || i.creator_id)", kind: onto.Ref, unsorted: true},
		"relation":       {expr: "relation", kind: onto.Enum},
		"pending_offers": {expr: "pending_offers", kind: onto.Integer},
		"comment_count":  {expr: "comment_count", kind: onto.Integer},
		"created_at":     {expr: "i.created_at", kind: onto.Timestamp},
		"updated_at":     {expr: "i.updated_at", kind: onto.Timestamp},
	}
	all, err := o.d.Types.List(ctx, c.TenantID)
	if err != nil {
		return nil, err
	}
	for _, t := range all {
		for key, f := range t.Fields.Properties {
			value := fmt.Sprintf("(CASE WHEN i.type = %s THEN i.fields -> %s END)", quote(t.ID), quote(key))
			col := column{kind: fieldKind(f)}
			switch col.kind {
			case onto.Integer, onto.Decimal:
				col.expr = fmt.Sprintf("(CASE WHEN jsonb_typeof(%s) = 'number' THEN (%[1]s #>> '{}')::numeric END)", value)
				col.cast = "::numeric"
			case onto.Boolean:
				col.expr = fmt.Sprintf("(CASE WHEN jsonb_typeof(%s) = 'boolean' THEN (%[1]s)::boolean END)", value)
				col.unsorted = true
			default:
				col.expr = fmt.Sprintf("(%s #>> '{}')", value)
			}
			// Items of other types have no value, so a sort can't page past them.
			col.unsorted = true
			cols[fieldProperty(t.ID, key)] = col
		}
	}
	return cols, nil
}

// fieldProperty names a type's field in the tenant layer: crm.lead's source
// is crm_lead__source.
func fieldProperty(typeID, field string) string {
	return strings.ReplaceAll(typeID, ".", "_") + "__" + field
}

// fieldKind is a field's property type: a string with options is an enum.
func fieldKind(f types.Field) onto.Kind {
	switch f.Type {
	case types.KindInteger:
		return onto.Integer
	case types.KindNumber:
		return onto.Decimal
	case types.KindBoolean:
		return onto.Boolean
	}
	if len(f.Enum) > 0 {
		return onto.Enum
	}
	return onto.Text
}

// fieldValue is a stored field value as its property's type: JSON numbers are
// float64, an integer is an int64 and a decimal a string.
func fieldValue(f types.Field, v any) any {
	n, ok := v.(float64)
	if !ok {
		return v
	}
	if fieldKind(f) == onto.Integer {
		return int64(n)
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}

func (o *Ontology) followItem(ctx context.Context, c policy.Caller, r item, link string, p onto.Page) ([]onto.LinkRow, error) {
	limit := p.Limit + 1
	var sql string
	switch link {
	case "owners", "collaborators":
		users := r.Owners
		if link == "collaborators" {
			users = r.Collaborators
		}
		rows := []onto.LinkRow{}
		for _, u := range users {
			ref := "auth:user:" + u
			if p.After != "" && ref <= p.After {
				continue
			}
			if rows = append(rows, onto.LinkRow{Ref: ref}); len(rows) == limit {
				break
			}
		}
		return rows, nil
	case "comments":
		sql = "SELECT id FROM collab.comments WHERE tenant_id = $1 AND item_id = $2 AND deleted_at IS NULL AND id > $3 ORDER BY id LIMIT $4"
	case "offers":
		sql = "SELECT id FROM collab.offers WHERE tenant_id = $1 AND item_id = $2 AND id > $3 ORDER BY id LIMIT $4"
	default:
		return nil, onto.ErrNotFound
	}
	rows, err := o.d.Pool.Query(ctx, sql, c.TenantID, r.ID, p.After, limit)
	if err != nil {
		return nil, fmt.Errorf("follow %s: %w", link, err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("follow %s: %w", link, err)
	}
	out := make([]onto.LinkRow, len(found))
	for i, id := range found {
		out[i] = onto.LinkRow{ID: id}
	}
	return out, nil
}

// list is a list the caller is a member of, with their role on it.
type list struct {
	lists.List
	role string
}

func (o *Ontology) declareLists() {
	cols := map[string]column{
		"name":       {expr: "i.name", kind: onto.String},
		"archived":   {expr: "(i.archived_at IS NOT NULL)", kind: onto.Boolean, unsorted: true},
		"role":       {expr: "i.role", kind: onto.Enum},
		"created_at": {expr: "i.created_at", kind: onto.Timestamp},
	}
	onto.AddObject(o.reg, onto.ObjectType[policy.Caller, list]{
		Name: "list", Label: "List", LabelZh: "清单", Title: "name",
		Description: "A shared list of tasks that its members manage together, often a team's. A member can take any unowned task on it.",
		Properties: map[string]onto.Property{
			"name":       {Type: onto.String, Name: "Name", NameZh: "名称"},
			"archived":   {Type: onto.Boolean, Name: "Archived", NameZh: "已归档"},
			"role":       {Type: onto.Enum, Name: "Your role", NameZh: "你的角色", Values: []string{"owner", "member"}, Derived: true},
			"created_at": {Type: onto.Timestamp, Name: "Created", NameZh: "创建时间"},
		},
		Links: map[string]onto.Link{
			"items":   {To: "collab:work_item", Many: true, Name: "Tasks", NameZh: "任务"},
			"members": {To: "auth:user", Many: true, Name: "Members", NameZh: "成员"},
		},
		DefaultSort: onto.Sort{Property: "name"},
		Search: func(ctx context.Context, c policy.Caller, q onto.Query) ([]list, error) {
			a := &args{first: 4}
			filter, err := where(q.Filter, cols, a)
			if err != nil {
				return nil, err
			}
			orderBy, after, err := order(q.Sort, q.After, cols, a)
			if err != nil {
				return nil, err
			}
			return o.lists(ctx, c, filter+" AND "+after, orderBy, q.Limit+1, a.values...)
		},
		Get: func(ctx context.Context, c policy.Caller, id string) (list, error) {
			found, err := o.lists(ctx, c, "i.id = $4", "i.id", 1, id)
			if err != nil {
				return list{}, err
			}
			if len(found) == 0 {
				return list{}, onto.ErrNotFound
			}
			return found[0], nil
		},
		View: func(_ context.Context, _ policy.Caller, r list) onto.Object {
			return onto.Object{ID: r.ID, Title: r.Name, Revision: 1, Properties: map[string]any{
				"name": r.Name, "archived": r.ArchivedAt != nil, "role": r.role, "created_at": r.CreatedAt,
			}}
		},
		Follow: func(ctx context.Context, c policy.Caller, r list, link string, p onto.Page) ([]onto.LinkRow, error) {
			sql := `SELECT i.id FROM collab.work_items i WHERE i.tenant_id = $1 AND i.list_id = $2 AND i.deleted_at IS NULL AND i.id > $3
				ORDER BY i.id LIMIT $4`
			if link == "members" {
				sql = `SELECT 'auth:user:' || user_id FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2
					AND 'auth:user:' || user_id > $3 ORDER BY 1 LIMIT $4`
			}
			rows, err := o.d.Pool.Query(ctx, sql, c.TenantID, r.ID, p.After, p.Limit+1)
			if err != nil {
				return nil, fmt.Errorf("follow %s: %w", link, err)
			}
			found, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return nil, fmt.Errorf("follow %s: %w", link, err)
			}
			out := make([]onto.LinkRow, len(found))
			for i, v := range found {
				if link == "members" {
					out[i] = onto.LinkRow{Ref: v}
				} else {
					out[i] = onto.LinkRow{ID: v}
				}
			}
			return out, nil
		},
	})
}

// lists answers the caller's lists that match, as members see them.
func (o *Ontology) lists(ctx context.Context, c policy.Caller, filter, orderBy string, limit int, more ...any) ([]list, error) {
	rows, err := o.d.Pool.Query(ctx, `WITH mine AS (
			SELECT l.id, l.name, COALESCE(l.org_unit_id, '') AS org_unit_id, l.created_at, l.archived_at, m.role
			FROM collab.lists l JOIN collab.list_members m ON m.tenant_id = l.tenant_id AND m.list_id = l.id AND m.user_id = $2
			WHERE l.tenant_id = $1)
		SELECT id, name, org_unit_id, created_at, archived_at, role FROM mine i WHERE `+filter+" ORDER BY "+orderBy+" LIMIT $3",
		append([]any{c.TenantID, c.UserID, limit}, more...)...)
	if err != nil {
		return nil, fmt.Errorf("find lists: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (list, error) {
		var l list
		err := row.Scan(&l.ID, &l.Name, &l.OrgUnitID, &l.CreatedAt, &l.ArchivedAt, &l.role)
		return l, err
	})
}

// comment is a comment on an item the caller can read.
type comment struct {
	ID, ItemID, AuthorID, Body string
	CreatedAt                  time.Time
}

func (o *Ontology) declareComments() {
	cols := map[string]column{
		"body":       {expr: "i.body", kind: onto.Text, unsorted: true},
		"author":     {expr: "('auth:user:' || i.author_id)", kind: onto.Ref, unsorted: true},
		"item":       {expr: "('collab:work_item:' || i.item_id)", kind: onto.Ref, unsorted: true},
		"created_at": {expr: "i.created_at", kind: onto.Timestamp},
	}
	onto.AddObject(o.reg, onto.ObjectType[policy.Caller, comment]{
		Name: "comment", Label: "Comment", LabelZh: "评论", Title: "body",
		Description: "A comment on a task, which everyone who reads the task reads.",
		Properties: map[string]onto.Property{
			"body":       {Type: onto.Text, Name: "Comment", NameZh: "内容"},
			"author":     {Type: onto.Ref, To: "auth:user", Name: "By", NameZh: "评论人"},
			"item":       {Type: onto.Ref, To: "collab:work_item", Name: "On", NameZh: "所属任务"},
			"created_at": {Type: onto.Timestamp, Name: "Written", NameZh: "时间"},
		},
		DefaultSort: onto.Sort{Property: "created_at", Desc: true},
		Search: func(ctx context.Context, c policy.Caller, q onto.Query) ([]comment, error) {
			a := &args{first: 5}
			filter, err := where(q.Filter, cols, a)
			if err != nil {
				return nil, err
			}
			orderBy, after, err := order(q.Sort, q.After, cols, a)
			if err != nil {
				return nil, err
			}
			return o.comments(ctx, c, filter+" AND "+after, orderBy, q.Limit+1, a.values...)
		},
		Get: func(ctx context.Context, c policy.Caller, id string) (comment, error) {
			found, err := o.comments(ctx, c, "i.id = $5", "i.id", 1, id)
			if err != nil {
				return comment{}, err
			}
			if len(found) == 0 {
				return comment{}, onto.ErrNotFound
			}
			return found[0], nil
		},
		View: func(_ context.Context, _ policy.Caller, r comment) onto.Object {
			return onto.Object{ID: r.ID, Title: r.Body, Revision: 1, Properties: map[string]any{
				"body": r.Body, "author": "auth:user:" + r.AuthorID, "item": "collab:work_item:" + r.ItemID, "created_at": r.CreatedAt,
			}}
		},
	})
}

// comments answers comments on items the caller can read, as the item's
// readers do: its people, and a manager the tenant lets read it.
func (o *Ontology) comments(ctx context.Context, c policy.Caller, filter, orderBy string, limit int, more ...any) ([]comment, error) {
	rows, err := o.d.Pool.Query(ctx, `WITH readable AS (
			SELECT m.id, m.item_id, m.author_id, m.body, m.created_at
			FROM collab.comments m JOIN collab.work_items i ON i.tenant_id = m.tenant_id AND i.id = m.item_id
			WHERE m.tenant_id = $1 AND m.deleted_at IS NULL AND (`+policy.VisibleSQL("$1", "$2")+" OR "+policy.ReportsSQL("$1", "$3::text[]")+`))
		SELECT id, item_id, author_id, body, created_at FROM readable i WHERE `+filter+" ORDER BY "+orderBy+" LIMIT $4",
		append([]any{c.TenantID, c.UserID, o.d.Items.Reports(ctx, c), limit}, more...)...)
	if err != nil {
		return nil, fmt.Errorf("find comments: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[comment])
}

// offer is a send of an item: an offer to accept, or an assignment.
type offer struct {
	ID, ItemID, From, To, Mode, Note, State string
	CreatedAt                               time.Time
	RespondedAt                             *time.Time
}

var offerStates = []string{"pending", "assigned", "accepted", "declined", "withdrawn", "voided"}

func (o *Ontology) declareOffers() {
	cols := map[string]column{
		"item":       {expr: "('collab:work_item:' || i.item_id)", kind: onto.Ref, unsorted: true},
		"from":       {expr: "('auth:user:' || i.from_user_id)", kind: onto.Ref, unsorted: true},
		"to":         {expr: "('auth:user:' || i.to_user_id)", kind: onto.Ref, unsorted: true},
		"mode":       {expr: "i.mode", kind: onto.Enum},
		"state":      {expr: "i.state", kind: onto.Enum},
		"created_at": {expr: "i.created_at", kind: onto.Timestamp},
	}
	onto.AddObject(o.reg, onto.ObjectType[policy.Caller, offer]{
		Name: "offer", Label: "Hand-off", LabelZh: "转交", Title: "note",
		Description: "A task sent from one person to another: an offer they accept or decline, or an assignment down the reporting line. mode transfer hands it over; share adds them beside the owners. Only people answer offers.",
		Properties: map[string]onto.Property{
			"item":         {Type: onto.Ref, To: "collab:work_item", Name: "Task", NameZh: "任务"},
			"from":         {Type: onto.Ref, To: "auth:user", Name: "From", NameZh: "发送人"},
			"to":           {Type: onto.Ref, To: "auth:user", Name: "To", NameZh: "接收人"},
			"mode":         {Type: onto.Enum, Name: "Mode", NameZh: "方式", Values: []string{"transfer", "share"}},
			"note":         {Type: onto.Text, Name: "Note", NameZh: "附言"},
			"state":        {Type: onto.Enum, Name: "State", NameZh: "状态", Values: offerStates},
			"created_at":   {Type: onto.Timestamp, Name: "Sent", NameZh: "发送时间"},
			"responded_at": {Type: onto.Timestamp, Name: "Answered", NameZh: "答复时间"},
		},
		DefaultSort: onto.Sort{Property: "created_at", Desc: true},
		Search: func(ctx context.Context, c policy.Caller, q onto.Query) ([]offer, error) {
			a := &args{first: 4}
			filter, err := where(q.Filter, cols, a)
			if err != nil {
				return nil, err
			}
			orderBy, after, err := order(q.Sort, q.After, cols, a)
			if err != nil {
				return nil, err
			}
			return o.offers(ctx, c, filter+" AND "+after, orderBy, q.Limit+1, a.values...)
		},
		Get: func(ctx context.Context, c policy.Caller, id string) (offer, error) {
			found, err := o.offers(ctx, c, "i.id = $4", "i.id", 1, id)
			if err != nil {
				return offer{}, err
			}
			if len(found) == 0 {
				return offer{}, onto.ErrNotFound
			}
			return found[0], nil
		},
		View: func(_ context.Context, _ policy.Caller, r offer) onto.Object {
			p := map[string]any{
				"item": "collab:work_item:" + r.ItemID, "from": "auth:user:" + r.From, "to": "auth:user:" + r.To,
				"mode": r.Mode, "note": r.Note, "state": r.State, "created_at": r.CreatedAt, "responded_at": nil,
			}
			if r.RespondedAt != nil {
				p["responded_at"] = *r.RespondedAt
			}
			return onto.Object{ID: r.ID, Title: r.Note, Revision: 1, Properties: p}
		},
	})
}

// offers answers offers the caller sent or was sent, and those on items they
// own, of items not deleted.
func (o *Ontology) offers(ctx context.Context, c policy.Caller, filter, orderBy string, limit int, more ...any) ([]offer, error) {
	rows, err := o.d.Pool.Query(ctx, `WITH theirs AS (
			SELECT f.id, f.item_id, f.from_user_id, f.to_user_id, f.mode, f.note, f.state, f.created_at, f.responded_at
			FROM collab.offers f JOIN collab.work_items w ON w.tenant_id = f.tenant_id AND w.id = f.item_id AND w.deleted_at IS NULL
			WHERE f.tenant_id = $1 AND (f.from_user_id = $2 OR f.to_user_id = $2 OR EXISTS (
				SELECT 1 FROM collab.work_item_owners ow WHERE ow.tenant_id = f.tenant_id AND ow.item_id = f.item_id AND ow.user_id = $2)))
		SELECT id, item_id, from_user_id, to_user_id, mode, note, state, created_at, responded_at FROM theirs i
		WHERE `+filter+" ORDER BY "+orderBy+" LIMIT $3",
		append([]any{c.TenantID, c.UserID, limit}, more...)...)
	if err != nil {
		return nil, fmt.Errorf("find offers: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[offer])
}
