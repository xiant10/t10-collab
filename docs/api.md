# t10-collab API

The reference for calling `t10-collab`: people's routes, admin routes, the agent tools at `/mcp`, and the events it posts to `t10-automations`. [`openapi.yaml`](openapi.yaml) is the machine-readable spec, and a test keeps it in step with the routes. [`development.md`](development.md) explains the code behind them.

## Reaching it

| From | Base URL | Token |
| --- | --- | --- |
| A browser, through workbench | `/api/modules/collab`, such as `/api/modules/collab/v1/work-items` | Workbench adds the signed-in person's token |
| Another service, or a test | `http://t10-collab:18040` on the cluster, `:8082` in its own Compose stack | A person's own access token from `auth-server` |
| An agent | `POST /mcp` | A delegated token whose grant lists `t10-collab` as an audience |

Every route except `/mcp`, `/internal/v1/attention` and the health checks takes a **person's own token**. `GET /healthz` answers `200` while the process runs, and `GET /readyz` `200` while Postgres answers, else `503`. The tenant and the person come from the token, never from the request. A delegated token (one an agent holds) is refused on every route with `403`, because agents reach collab only through `/mcp`.

## Conventions

- **JSON** both ways, with bodies up to 1 MiB. Times are RFC 3339.
- **IDs** carry a prefix: `wi_` work items, `ofr_` offers, `lst_` lists, `usr_` people, `acc_` agent accounts.
- **Errors** look like `{"error": "<code>", "message": "<words for a person>"}`. See [Error codes](#error-codes).
- **Not found or forbidden.** An item the caller can't see answers `404`, the same as a missing one. One they can see, but can't change the way they asked, answers `403`.
- **Paging.** Lists take `limit` (1 to 200, default 50) and `cursor`, and return `next_cursor`, which is empty on the last page.
- **Versions.** `PATCH /v1/work-items/{id}` needs the `version` the caller read. A stale one answers `409 version_conflict`, and the client reads the item again.
- **Retries.** Item creation and sends take an optional `client_ref`. A retry with the same value returns the first result instead of acting twice.
- **Send limits.** A person may send 100 items an hour and an agent account 60. Past that, sends answer `429 rate_limited`.

## Work items

Users see these as "tasks".

| Method and path | Does |
| --- | --- |
| `POST /v1/work-items` | Create an item owned by the caller. With `send`, also send it. With `list_id`, put it on a list the caller is on, and with `unowned: true` leave it there for any member to claim. `type`, `fields` and `subject_ref` make it a module's [typed item](types.md) |
| `GET /v1/work-items` | Items the caller can see. Filters: `view` (`owned`, `sent`, `collaborating`, `list` with `list`, `reports`, `claimable`), `status` (`todo`, `doing`, `done`, `cancelled`, or `open` for the first two), `q` (title contains), `due_from`, `due_before`, `type`, `module`, `subject` |
| `GET /v1/work-items/{id}` | One item, plus `can` (what the caller may do), `pending_offers`, `working` (agents expected to act on it now), `mention` (the latest comment that named the caller) and, on a list with a workflow, `workflow` |
| `PATCH /v1/work-items/{id}` | Change `title`, `description`, `priority`, `status`, `due_at` (null clears it) or `fields` (merged; null clears a key). Needs `version` |
| `GET /v1/types` | The [item types](types.md) in the caller's tenant, `collab.task` first, so a client can draw any item's fields |
| `DELETE /v1/work-items/{id}` | Soft delete, for any owner, or the creator of an unowned list item |
| `GET /v1/work-items/{id}/events` | Its history, oldest first |
| `POST /v1/work-items/{id}/claim` | Claim an unowned list item |
| `PUT /v1/work-items/{id}/list` | Put an item the caller owns on a list, or take it off with `list_id: ""` |
| `DELETE /v1/work-items/{id}/owners/me` | Stop being an owner. The last owner can't (`409 last_owner`) |
| `PUT`, `DELETE /v1/work-items/{id}/collaborators/{user}` | Add or remove a collaborator |
| `GET`, `POST /v1/work-items/{id}/comments` | Read comments, oldest first, or add one: `{body, mentions?}`. `mentions` are user IDs to tell, who must be able to read the item |
| `POST /v1/work-items/{id}/links` | Add a link: `{url, title?, kind?}`, with `url` https and at most 20 per item. `kind` is `pull_request`, `commit`, `build`, `document` or `other`, guessed from the URL when left out |
| `DELETE /v1/work-items/{id}/links/{link}` | Remove a link |
| `DELETE /v1/work-items/{id}/suggestion` | Dismiss a send an assistant suggested |

Creating an item and sending it at once:

```http
POST /v1/work-items
Authorization: Bearer <person's token>

{"title": "Renew the office lease", "priority": "high", "due_at": "2026-12-01T09:00:00Z",
 "client_ref": "lease-2026", "send": {"to": "usr_9f2…", "mode": "transfer", "note": "You have the landlord's contact"}}
```

```json
{"id": "wi_3c1…", "title": "Renew the office lease", "status": "todo", "priority": "high",
 "due_at": "2026-12-01T09:00:00Z", "creator_id": "usr_1a7…", "version": 1,
 "owners": ["usr_1a7…"], "collaborators": [], "created_at": "…", "updated_at": "…"}
```

The item stays the caller's until the recipient accepts the offer.

## Sending and offers

`POST /v1/work-items/{id}/send` takes `{to, mode?, note?, client_ref?}`. `mode` is `transfer` (the default), which gives the item away, or `share`, which keeps the sender as an owner. The service decides what kind of send it is:

| `state` in the answer | When |
| --- | --- |
| `pending` | An offer, which the recipient accepts or declines |
| `assigned` | The recipient reports to the sender, so no acceptance is needed |

A send to an agent account needs that account's intake to allow the sender, or it answers `403 not_accepting`. A send that would make the third agent-to-agent hand-off on an item with no person acting goes to the sending agent's manager instead.

| Method and path | Does |
| --- | --- |
| `GET /v1/offers?box=inbox` | Offers waiting for the caller's answer |
| `GET /v1/offers?box=sent` | Offers the caller sent |
| `POST /v1/offers/{id}/accept` | Accept, for the recipient |
| `POST /v1/offers/{id}/decline` | Decline, for the recipient, with an optional `note` the sender sees |
| `POST /v1/offers/{id}/withdraw` | Withdraw, for the sender or any owner |

An offer's `state` is one of `pending`, `assigned`, `accepted`, `declined`, `withdrawn`, `voided` (the item changed hands another way) or `expired`. Only offers holding a departed owner's item expire, after 14 days.

## Workflows

A list may carry a workflow: stages, each with holders, that items move through. [`workflows.md`](workflows.md) has the rules.

| Method and path | Does |
| --- | --- |
| `PUT /v1/lists/{id}/workflow` | Set it, as a list owner: `{stages, rounds?, lead?, auto_start?}`. A stage is `{id?, name, holders, skip_when?, on_changes?, guide?}` |
| `DELETE /v1/lists/{id}/workflow` | Remove it. Items stay with their holders |
| `POST /v1/work-items/{id}/stage` | Start an item on the workflow, or, as a list owner, put it at `stage`. Its rounds start over |
| `POST /v1/work-items/{id}/advance` | Finish the caller's stage: `{result: "pass" or "changes", note?, to?}`. `changes` needs a `note`, and `to` passes over stages that may be skipped |
| `PUT`, `DELETE /v1/lists/{id}/holding` | A plain list member agrees to hold stages, or stops |

## Lists

| Method and path | Does |
| --- | --- |
| `GET /v1/lists` | The lists the caller is on, with `counts` and `has_workflow`. `archived=true` lists archived ones |
| `POST /v1/lists` | Create one: `{name}`. The caller owns it |
| `GET /v1/lists/{id}` | One list with its `members` and `workflow` |
| `PATCH /v1/lists/{id}` | `{name?, archived?, org_unit_id?}`. A unit link fills its members from that org unit, and `""` clears it |
| `PUT /v1/lists/{id}/members/{user}` | Add a member, or change their `role` (`owner` or `member`) |
| `DELETE /v1/lists/{id}/members/{user}` | Remove a member as a list owner, or leave as yourself. A member who comes from the unit link answers `409 managed_by_unit` |

## People, units and notifications

| Method and path | Does |
| --- | --- |
| `GET /v1/people?q=&unit=` | Active members of the tenant by display name, people and agent teammates (`agent: true`) |
| `GET /v1/units` | The tenant's org units |
| `GET /v1/notifications?after=` | The caller's notifications after a cursor, oldest first, with the next `cursor`. Poll about every 30 seconds |
| `POST /v1/notifications/read` | Mark some read: `{ids}` |
| `GET /v1/settings` | Tenant settings, for any member |

A notification's `target` names the record as `collab:work_item:<id>`, and its `event` is the item event that caused it.

## Admin routes

These need a tenant admin, or a member holding `work:admin` from `auth-server`.

| Method and path | Does |
| --- | --- |
| `PUT /v1/admin/settings` | `{manager_visibility}`: `none`, `direct_reports` or `all_reports`, which decides what a manager sees with `view=reports` |
| `GET`, `PUT /v1/admin/agents/{user}/intake` | Who may offer work to an agent account: `{everyone?, units?, lists?}`. With nothing set, only its reporting line can give it work |
| `GET /v1/admin/intake-lists` | Every shared list by name, for picking intake lists |
| `GET /v1/admin/departures` | Items nobody can act on, because owners left or were suspended |
| `POST /v1/admin/departures/{item}/reassign` | Give a queued item a new owner: `{to}` |
| `GET /v1/admin/departures/lists` | Lists nobody can manage |
| `POST /v1/admin/departures/lists/{id}/owner` | Give a queued list a new owner: `{to}` |

## Module contract

| Method and path | Does |
| --- | --- |
| `GET /v1/module` | The manifest: `id: collab`, `api_version: 1`, `action_prefix: work`, `features`, and `trigger_events`, the event kinds automations can fire on |
| `GET /internal/v1/attention?tenant=&user=&since=` | Counts for `t10-notify`'s digest, such as offers to answer and items overdue, with no titles or names. Only `t10-notify`'s service token may call it |
| `PUT /internal/v1/types/{type}` | A module declares an [item type](types.md). Only the owning module's service token, `t10-<module>`, may call it |

## Agent tools (MCP)

`POST /mcp` takes one JSON-RPC message per request (MCP over Streamable HTTP, with no session or stream). `initialize`, `ping` and `tools/list` need no token. `tools/call` needs a delegated token listing `t10-collab`, with the tool's action on the token and on every grant in its chain.

```http
POST /mcp
Authorization: Bearer <delegated token>

{"jsonrpc": "2.0", "id": 7, "method": "tools/call",
 "params": {"name": "work_comment", "arguments": {"id": "wi_3c1…", "body": "Draft is ready for review."}}}
```

A result's text is JSON inside one `<module_record>` tag, which marks it as data for the model. A refused or failed call still answers `200`, with `isError: true` and a reason the model can read.

| Tool | Action | Arguments | Does |
| --- | --- | --- | --- |
| `work_list` | `work:read` | `view?`, `status?`, `list_id?`, `due?` (`overdue`, `soon`), paging | List the principal's items |
| `work_get` | `work:read` | `id` | One item in full, with its workflow stage |
| `work_comments` | `work:read` | `id`, paging | Comments, oldest first |
| `work_history` | `work:read` | `id`, paging | History, oldest first |
| `work_offers` | `work:read` | `box` (`inbox`, `sent`), paging | Offers waiting, or sent |
| `work_lists` | `work:read` | `archived?`, paging | Shared lists |
| `work_people` | `work:read` | `q?` or `ids?`, paging | Find colleagues, or name user IDs |
| `work_create` | `work:write` | `title`, `description?`, `priority?`, `due_at?`, `list_id?`, `unowned?`, `suggest_to?`, `suggest_note?` | Create an item that stays with the principal, optionally suggesting a send |
| `work_suggest` | `work:write` | `id`, `to`, `note?` | Suggest a send, for the person to make |
| `work_update` | `work:write` | `id`, `version`, and fields to change | Edit fields or status |
| `work_comment` | `work:write` | `id`, `body`, `mentions?` | Comment, telling people on the item |
| `work_link` | `work:write` | `id`, `url`, `title?`, `kind?` | Add a link |
| `work_send` | `work:send`, teammates only | `id`, `to`, `note`, `mode?` | Send an item |
| `work_advance` | `work:send`, teammates only | `id`, `result`, `note`, `to?` | Finish its workflow stage |
| `work_accept` | `work:send`, teammates only | `offer_id` | Accept an offer |
| `work_decline` | `work:send`, teammates only | `offer_id`, `note` | Decline an offer |
| `work_claim` | `work:write`, teammates only | `id` | Claim an unowned list item |

"Teammates only" means the token's principal must be an agent account (`acc_…`). An assistant acting for a person reads and drafts, and the person sends. A grant with constraints is refused, because collab defines none.

## Events it posts

For each change that concerns an agent account, collab posts an event to `t10-automations`' `POST /v1/events`, at least once, with its own service token:

```json
{"events": [{
  "id": "obx_5d2…", "source": "t10-collab", "tenant": "ten_acme", "kind": "work.assigned",
  "ref": "collab:work_item:wi_3c1…", "for": "acc_7f1…", "actor": "acc_3d1…",
  "cause_grant": "grt_14b…", "cause_chain": ["grt_c86…", "grt_3c2…", "grt_14b…"],
  "hops": 1, "at": "2026-11-15T10:54:38Z"
}]}
```

| Field | Means |
| --- | --- |
| `kind` | `work.offered`, `work.assigned` (assignments, reassignments and workflow moves), `work.accepted`, `work.declined`, `work.commented` or `list.item_added` |
| `for` | The agent account it concerns |
| `actor` | Who made the change, a person or an agent |
| `cause_grant`, `cause_chain` | When an agent made it: the grant of its tool call, and that grant's chain, root first |
| `hops` | Agent sends, or agent comments if more, on the item since a person last acted |

The payload holds IDs only. A run reads the item through its own tools.

## Error codes

| Code | Status | Means |
| --- | --- | --- |
| `invalid_token` | 401 | Missing, invalid or expired token |
| `forbidden` | 403 | The caller can see it, but can't do this; or a token that can't use this route |
| `not_accepting` | 403 | That agent account doesn't take work from the caller |
| `not_found` | 404 | Missing, or not visible to the caller |
| `invalid_request` | 400 | A malformed body, a bad field, or a bad cursor |
| `invalid_recipient`, `invalid_unit`, `not_on_list`, `holder_not_accepting` | 422 | The named person, unit or holder can't be used here |
| `version_conflict` | 409 | Someone changed the item since it was read |
| `last_owner`, `last_list_owner` | 409 | The item or list would have no owner |
| `already_owner`, `already_owned` | 409 | Nothing to do: they own it, or someone claimed it first |
| `item_closed`, `list_archived` | 409 | It's done, cancelled or archived |
| `managed_by_unit` | 409 | The member comes from the list's unit link |
| `offer_exists`, `offer_not_pending`, `client_ref_reused` | 409 | An offer is already pending, it's already answered, or the `client_ref` was used for something else |
| `not_on_workflow`, `no_way_back`, `cant_skip` | 409 | Workflow moves that don't apply |
| `too_many_links` | 409 | The item has 20 links |
| `not_for_type` | 409 | The item's type doesn't allow it, such as a list or collaborator on a `participants` item |
| `unknown_type` | 422 | No such item type in this tenant |
| `invalid_type`, `type_version_stale`, `type_fixed` | 400, 409 | On `PUT /internal/v1/types/{type}`: a bad definition, a version that isn't newer, or a changed `visibility` or `departure` |
| `agent_loop` | 409 | Agents have passed it on too often, and the sending agent has no manager to send it to instead |
| `rate_limited` | 429 | Too many sends this hour |
| `org_unavailable` | 503 | `auth-server` couldn't answer an org question |
| `internal_error` | 500 | Something failed here |
