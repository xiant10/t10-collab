---
title: "Collab's ontology"
audience: consumer
kind: reference
status: current
summary: "What t10-collab serves through the ontology contract: tasks, lists, comments and offers under collab's access policy, item types as the tenant layer, and actions an assistant drafts or takes."
---

# Collab's ontology

Collab serves the workspace's ontology contract (`contracts/ontology/README.md`), so any app or agent reads and acts on tasks the same way as on every other module's records. A reference such as `collab:work_item:wi_3c1…`, which a sales help request stores once sent, resolves here for whoever may read the task, and for nobody else.

The core descriptor is pinned in [ontology.json](ontology.json). `internal/ontology` declares it over collab's own services, with t10-modkit's `ontology` package serving the routes and generated tools.

## Objects

Every read applies collab's one access policy (`internal/policy`) in SQL before sorting and paging, as the routes do. Anything the caller can't read answers `404` and is left out of searches and resolves.

| Type | Reads for | Holds |
| --- | --- | --- |
| `work_item` | Its creator, owners, collaborators, the recipient of a pending offer, members of its list, and a manager of an owner as the tenant's manager visibility allows. An item of a `participants` type reads only for its creator, owners and pending recipient | `title`, `description`, `status`, `priority`, `deadline`, `type`, `state`, `list`, `subject`, `creator`, `created_at`, `updated_at`; `overdue`, `due_soon`; and for the caller `relation`, `pending_offers`, `working` and `comment_count`. Links: `owners`, `collaborators`, `comments`, `offers` |
| `list` | Its members | `name`, `archived`, `role` (the caller's), `created_at`. Links: `items`, `members` |
| `comment` | Whoever reads its item | `body`, `author`, `item`, `created_at` |
| `offer` | Its sender, its recipient, and the item's owners | `item`, `from`, `to`, `mode`, `note`, `state`, `created_at`, `responded_at` |

A work item's revision is its `version`. It implements `t10:HasDeadline` and `t10:Commentable`.

**Where a task stands.** `relation` is the caller's strongest tie: `owner`, `creator`, `collaborator`, `recipient`, `list_member`, or `manager` for a read only a manager makes. A search filters on it, so "my tasks" and "my overdue tasks" are searches:

```json
POST /v1/objects/work_item/search
{"filter": {"and": [{"property": "relation", "op": "eq", "value": "owner"},
                    {"property": "deadline", "op": "lt", "value": "2026-11-27T09:00:00Z"}]},
 "sort": {"property": "deadline"}, "limit": 20}
```

`overdue` (open and past due) and `due_soon` (open and due within a day) say the same for a client that can't put the time in a filter, such as a fixed greeting variable: "my overdue tasks" is also `relation eq owner` and `overdue eq true`.

`working` counts the agent teammates told about the task in the last 15 minutes that haven't acted since; it can't be filtered or sorted on. A search sorts on the core's properties, not on type fields.

## Item types are the tenant layer

The [item types](types.md) a tenant has are its tenant layer:

- `work_item.type` takes their IDs, beside the built-in `collab.task`;
- `work_item.state` takes their states;
- each type's fields are properties named for the type, with its dot as an underscore: `sales.presales`'s `when` is `sales_presales__when`. A string field with options is an enum, a number a decimal, and the rest text, integer or boolean. They're set on items of that type only;
- `work_item.move`'s `move` takes their transitions' IDs.

The layer's revision counts every change to a type or a tenant's version of one.

## Actions

Collab's rule is that an assistant acting for a person reads and drafts, and only the person sends. Each action calls the service its route calls, under the policy check that route makes.

| Action | On | Tier | Does |
| --- | --- | --- | --- |
| `work_item.create` | (creates) | undoable, 24 h, idempotent | A task the person owns, which nobody else sees until they share or send it. Undo deletes it, and answers `undo_conflict` once someone else owns it, follows it, was sent it or commented on it, or it's on a list |
| `work_item.edit` | `work_item` | propose | Title, description, priority, due date (`due`, or `clear_deadline`) |
| `work_item.set_status` | `work_item` | propose | `todo`, `doing`, `done` or `cancelled`, for a plain task. Only its creator or an owner cancels |
| `work_item.move` | `work_item` | propose | A step its type allows from its state, for a typed item; the item's actions say whether one is open to the caller |
| `work_item.comment` | `work_item` | propose | A comment everyone on the task is told of |
| `work_item.link` | `work_item` | propose | An https link to where its work is |
| `work_item.send` | `work_item` | propose | Send it to a colleague: assigned down the person's reporting line, an offer to accept otherwise |

Accepting, declining, claiming, deleting and list or settings administration aren't actions, so no assistant can draft them: people do them in the app, and agent teammates with their own tools.

What an assistant made through `work_act` is listed for the person at `GET /v1/undo`, with the agent, until it's undone or the day is over. Everything an assistant did through `work_act` in the last 7 days, undoable or not, is at `GET /v1/done`, with its undo ID while it can still be undone: Workbench shows it under **Done for you**, with Undo where it can.

A draft is confirmed under the person's rights at the time. One whose task changed since answers `stale_revision`, and Workbench offers to confirm it anyway with the current revision. Drafts and undo entries live in collab's own schema (`collab.ontology_proposals`, `collab.ontology_undo`, `collab.ontology_replays`, `collab.ontology_done`).

## Agent tools

An assistant acting for a person has the generated `work_describe`, `work_search`, `work_get`, `work_propose` and `work_act`, with collab's `work_people` and `work_history`, under `work:read`, `work:propose` and `work:act`. The manifest lists `auto_apply`: with `work:apply` too, which the person grants by letting their assistant act without asking, `work_act` also takes the `propose` actions, with no draft, and the event it writes has `auto_apply: true` in its detail. An agent teammate, whose run's principal is a tenant agent account, also has its own tools under `work:write` and `work:send`: nobody would confirm its drafts, so it acts as itself. The list is the same for every caller; a teammate's tool refuses anyone else and says to draft with `work_propose`. See [api.md](api.md#agent-tools-mcp).

## Checks

- `internal/server/ontology_test.go`: each object equals its route's answer, strangers find nothing, a `participants` item resolves for its two people, a manager's view, paging "my overdue tasks", the tenant layer, drafts confirmed and stale, undo and its conflicts, and each action's availability against its route.
- `internal/server/conformance_test.go` runs the workspace's conformance suite, vendored in `third_party/ontology-conformance`, in `make ci`.
- `TestTheCoreDescriptorIsPinned` holds [ontology.json](ontology.json): a change to the core raises `ontology.Version`, then `UPDATE_ONTOLOGY=1 go test ./internal/server -run Pinned`.
