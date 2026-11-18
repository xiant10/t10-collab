---
title: "Developing t10-collab"
audience: contributor
kind: guide
status: current
summary: "How t10-collab's code works and how to change it safely."
---

# Developing t10-collab

How the code works and how to change it safely. [`design.md`](design.md) is the design as built and why, [`workflows.md`](workflows.md) covers list workflows, [`../README.md`](../README.md) has the departures from the first design, the agent tools and the package list, [`api.md`](api.md) is the API reference, and [`openapi.yaml`](openapi.yaml) its machine-readable spec.

## In one paragraph

`t10-collab` is the module where a tenant's people and agent teammates send work items (shown as "tasks") to each other and manage them together. It's a Go service with its own Postgres schema, `collab`. It trusts `auth-server` for identity and org facts (members, managers, units) and nothing else. People reach it through workbench's module proxy with their own token, and agents reach it only through the MCP endpoint `/mcp`. Every change records an event and the notifications it causes in the same transaction. An outbox carries the changes that concern agent teammates to `t10-automations`, which starts their runs.

## Layout

| Package | Owns |
| --- | --- |
| `cmd/t10-collab` | Wiring: config, migrations, the HTTP server, the sweep, the event poster |
| `cmd/testclient` | A walkthrough against a running stack, signed in with the dev password grant |
| `internal/config` | Environment variables |
| `internal/db` | The pool, and embedded migrations applied at start under an advisory lock |
| `internal/policy` | `Caller`, every access rule (`Relation.CanRead`, `CanEdit`, `CanSend`…), and `VisibleSQL`, the read rule as a SQL filter |
| `internal/workitems` | Items, status, owners, collaborators, comments, links, suggestions |
| `internal/types` | Item types modules declare: definitions, field checks, the store. See [`types.md`](types.md) |
| `internal/offers` | Sends, accepts, declines, withdrawals, the agent-loop redirect, and workflow moves (`workflow.go`) |
| `internal/lists` | Shared lists, members, unit links, workflows and stage holders |
| `internal/activity` | `Record` (event, notifications and outbox rows in one call), item history, the notification feed, agent hop counts |
| `internal/events` | The outbox poster to `t10-automations` |
| `internal/sweep` | The 15-minute job: team-linked lists, and moving work away from members who left |
| `internal/roles` | Who administers collab: tenant admins and holders of `work:admin` |
| `internal/server` | HTTP routes (`server.go`) with the action each needs, and the agent tools (`mcp.go`) |
| `third_party/t10-modkit` | Org reads (`people`), IDs and paging, as a source snapshot until modkit is released |
| `verifier` | Token verification, copied from `auth-server`. Keep the copies identical |

## How a request flows

**A person.** `guard` verifies the bearer token and refuses one an agent holds. The handler builds a `policy.Caller` from the token, never from the request. It calls a service (`workitems`, `offers`, `lists`), which opens a transaction, reads the item with the caller's relation (`Relation`: creator, owner, collaborator, recipient, list member), checks `policy`, changes rows, and calls `activity.Record` before committing. `classify` turns service errors into statuses. An item the caller can't see answers `404`, the same as a missing one; an item they can see but can't change answers `403`.

**An agent.** `POST /mcp` speaks JSON-RPC. `tools/list` needs no token, because a harness lists tools at startup. `tools/call` verifies a delegated token and refuses:

- a grant with constraints, since none are defined here;
- a teammate-only tool (`work_send`, `work_advance`, `work_transition`, `work_accept`, `work_decline`, `work_claim`) when the caller isn't an agent account, so an assistant acting for a person only reads and drafts (`work_suggest`);
- a tool whose action (`work:read`, `work:write`, `work:send`) the token's chain doesn't allow.

The tool runs the same service code as the routes, with a `Caller` that also holds the agent's `GrantID` and `GrantChain`, so an agent can do no more than its principal. Results go back as JSON inside one `<module_record>` tag, which harnesses treat as data.

## Events, notifications and the outbox

`activity.Record(ctx, tx, caller, Change{ItemID, Kind, Detail, Notify})` is the one way to say something happened. In the caller's transaction it writes:

1. one `collab.events` row: the item's history, with `actor_grant_id` when an agent acted;
2. one `collab.notifications` row per member in `Notify`, never the actor;
3. for a notified agent account, when the kind is in `AgentEvents`, one `collab.outbox` row. It carries the event kind `t10-automations` fires on (`work.offered`, `work.assigned`, `work.commented`…), the actor, the agent's grant (`cause_grant_id`) and grant chain (`cause_chain`), and the agent hops on the item since a person last acted.

Two filters keep agents from being woken for nothing:

- an agent that has handed the item on isn't woken by later changes unless a comment mentions it (`handedOn`);
- a send that would make the third agent-to-agent hand-off with no person acting goes to the sending agent's manager instead (`offers.loop`), and the event detail records `redirected_from`.

`events.Poster` posts unsent outbox rows every 2 seconds, in batches of 100, with this service's own token. It retries with backoff up to 5 minutes and keeps sent rows for 7 days. Posting is at least once, so `t10-automations` drops repeats by event ID. With no `AUTOMATIONS_URL`, rows are marked sent and pruned, so a stack without automations keeps no growing outbox.

The notification feed (`GET /v1/notifications`) pages by a `<xid>.<seq>` cursor and stops short of any transaction still running, so a slow commit is never skipped.

## Rules for changes

- **One access rule, in two forms.** `Relation.CanRead` and `policy.VisibleSQL` must change together. Lists filter in SQL before paging, so a page never comes back short because rows were hidden afterwards.
- **Record in the same transaction.** Any state change calls `activity.Record` before commit. History, notifications and outbox rows never disagree with the item.
- **Agents only through `/mcp`.** Routes refuse delegated tokens. A new agent ability is a tool with an action, and it's `agentOnly` when only a teammate acting as itself should have it.
- **Text from people is data.** A tool result goes in `<module_record>`, and a work item prompt goes through `workItemPrompt` on the workbench side. Don't build instructions out of item text.
- **"Task" in the UI, "work item" in code.** A search for `task` in t10 code should find only agent work.
- **Migrations.** Add a new numbered file in `internal/db/migrations`, and never edit one that has shipped.
- **The OpenAPI file follows the routes.** `openapi_test.go` fails when they differ.
- **The outbox format is shared.** `t10-automations` rejects unknown fields, so deploy it before a `t10-collab` that posts a new event field.
- **Two copies stay identical:** `verifier` with `auth-server`'s, and `permissions.yaml` with the workspace's `deploy/k8s/catalog/collab/collab.yaml`.

### Common changes

| To | Do |
| --- | --- |
| Add a route | Add it to `routes()` with its action and a handler taking `policy.Caller`, with the logic in a service package. Add it to `openapi.yaml` |
| Add an agent tool | Add a `tool` in `tools()` with a JSON schema, an action and `run`, which calls the same service as the route. Set `agentOnly` when only teammates may use it, and list it in the README |
| Add an event kind | Call `activity.Record` with the new `Kind`, then add its label to workbench's `modules/collab/events.ts` (and `zh.ts`). If teammates should wake on it, map it in `AgentEvents` and decide whether workbench's `INTAKE_EVENTS` subscribes |
| Change who may see or do something | Change `internal/policy` only, keep `VisibleSQL` in step, and add a case to the access tests |
| Add a tenant setting | Add it to the settings in `policy` and to `PUT /v1/admin/settings`, which tenant admins and `work:admin` holders reach |

## Running it

On the local Docker Desktop cluster, namespace `t10`, it listens on 18040:

```sh
docker build -t t10-collab:my-preview .
kubectl -n t10 set image deploy/t10-collab t10-collab=t10-collab:my-preview
kubectl -n t10 port-forward svc/t10-collab 29340:18040
```

Roll back with `kubectl rollout undo`. On its own, `docker compose up -d --build` starts it on 8082 with its own Postgres. That needs `auth-server`'s stack on the host, with its setup run so the `t10-collab` service account exists. `go run ./cmd/testclient` then walks through offers, assignments, notifications, lists, unit links, manager visibility and tenant isolation.

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | Its own Postgres database |
| `AUTH_ISSUER`, `AUTH_URL` | `auth-server` as named in tokens, and where to reach it |
| `AUTH_CLIENT_ID`, `AUTH_CLIENT_SECRET` | Its service account, for org reads and posting events |
| `AUTOMATIONS_URL` | `t10-automations`, which takes teammates' events. Empty turns posting off |
| `COLLAB_LISTEN_ADDR` | Listen address |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Tracing. Empty turns it off |

## Tests

```sh
docker run -d --rm --name collab-test-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=collab_test -p 55436:5432 postgres:17-alpine
TEST_DATABASE_URL='postgres://postgres:test@localhost:55436/collab_test?sslmode=disable' go test -p 1 ./...
```

- Without `TEST_DATABASE_URL`, the database tests skip. Each test drops and remigrates the `collab` schema, so use a throwaway Postgres and `-p 1`.
- `newHarness` builds the server over a fake org: tenant `acme` with `ada`, `bob`, `carol` (who manages `bob` and `ada`), `dan` and `eve`, plus `gus` in `globex`. `h.as(user, …)`, `h.want(…)` and `h.wantError(…)` call routes as a person; `h.item` and `h.send` set up common state.
- Agent tokens are strings the test verifier reads: `agent-read` and `agent-write` for an assistant, and `teammate:<account>` or `teammate:<account>@<grant>` for a teammate's run, whose chain is `<grant>_run` then `<grant>`. `call(t, url, token, tool, args)` runs a tool.
- `h.outbox()` and `h.clearOutbox()` check what would be posted to `t10-automations`.

## Debugging

- An item's history: `GET /v1/work-items/{id}/events`, or `collab.events` by `item_id` ordered by `at`. `actor_grant_id` names the agent run that made a change.
- What reached `t10-automations`: `collab.outbox`, where `sent_at` is null for rows still waiting, and `attempts` with `next_at` show retries.
- A teammate that wasn't woken: check that the kind is in `AgentEvents`, that it wasn't filtered as handed on, and on the automations side that its intake automation lists the kind.
