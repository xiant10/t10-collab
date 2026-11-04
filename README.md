# t10-collab

People in a tenant send work to each other and manage it together. This is a small Go service with its own Postgres database, and it trusts the auth service for identity and org facts only. [docs/design.md](docs/design.md) explains why.

It has no link to `agent-pi` tasks. Users see "task", but code, schema and API say **work item**, so a search for `task` in t10 only ever finds agent work.

## Status

All five rollout steps are built. V1 is an API with no client of its own, and agents reach it over MCP at `/mcp`.

### Changes from the design

| Design says | Built | Why |
| --- | --- | --- |
| `GET /v1/notifications?after=` with no ordering column | The cursor is `<xid>.<seq>`, and reads stop at the oldest running transaction | IDs are random, and a plain sequence skips a transaction that commits late |
| Delete: creator | Delete: any owner | A creator could otherwise hide work they've handed to someone else |
| Pending offers end when an item closes | They also end on delete and assignment, and a transfer ends the sender's other offers | A stale offer could otherwise take an item from its new owner |
| Sends can't be retried safely | `send` takes an optional `client_ref` | A retry after an assignment would fail, because the sender no longer owns the item |
| Reopening isn't limited | Moving out of `cancelled` needs the right to cancel | A collaborator could otherwise undo an owner's decision |
| `priority` values aren't defined | `low`, `normal`, `high` | |
| Unowned items are only reached by the last owner leaving | `POST /v1/work-items` also takes `unowned: true` with `list_id` | A shared backlog needs a way to start one |
| Delete: creator | The creator can still delete an unowned list item | It's nobody else's work, and nobody could delete it otherwise |
| A `departures` table records who was handled | No table. Each sweep works out what an item needs from its owners and offers | A record of who was handled stops retries, and skips anyone who's reinstated and leaves again |
| A declined item stays with its sender, and offers never expire | Offers holding a departed owner's item expire after 14 days, and a successor's decline sends the item to the queue | The item could otherwise wait on someone who has left, forever |
| The sweep offers an item to the creator even if an offer is pending | A live offer is left in place, and the sweep moves on only if it fails | One pending offer already fills the slot |
| Suspended owners and departed list owners have no way out | They get the queue too, with `GET /v1/admin/departures/lists` and `POST /v1/admin/departures/lists/{id}/owner` | Their work could otherwise never be moved |
| `GET /v1/lists/{id}` isn't listed | Added, with members | A client has no other way to read them |
| Removing or demoting a list's last owner isn't covered | Refused with `409 last_list_owner` | A list with no owner can't be managed |
| Changing a link to another unit isn't covered | The old unit's members become `manual`, the same as clearing | Nobody loses access by surprise |
| Agents get `work:send` and `work:delete` tools, outside default grants | No such tools | The design's open question recommended read and draft only. Fewer tools means less for injected text to reach |
| Delegated tokens use the API like any caller | Routes still refuse them, and only `/mcp` takes them | One action can't tell `PUT /v1/admin/settings` from an ordinary write, so `work:write` would reach settings for an admin's agent |

The first three, and the four on departures, come from the design review. `docs/design.md` now describes departures as built.

## Run it

Start `auth-server`'s stack and add the sample accounts, then:

```sh
docker compose up -d --build
go run ./cmd/testclient
```

The auth stack has to include the prerequisite changes, and its setup has to have run once since, so that the `t10-collab` service account exists.

`testclient` signs in with the auth service's dev password grant and walks through declining, accepting, assigning, notifications, lists, unit links, manager visibility and tenant isolation.

## Agents

`POST /mcp` offers these tools, and `agent-pi` connects with `auth: "grant"`:

| Action | Tools |
| --- | --- |
| `work:read` | `work_list`, `work_get`, `work_comments`, `work_history`, `work_offers`, `work_lists`, `work_people` |
| `work:write` | `work_create`, `work_update`, `work_comment` |

- Tools call the same services as the routes, so an agent can do no more than its person.
- A write records the grant in `events.actor_grant_id`, and the event shows it as `actor_grant_id`.
- A result is JSON inside one `<module_record>` tag, which `agent-pi`'s prompt treats as data. Other people's text can't close the tag, because JSON writes `<` as `\u003c`.
- A grant with constraints is refused, because none are defined here yet.
- Listing needs no token, because `agent-pi` lists at startup with no user. The list holds nothing about a tenant.

To turn it on, deploy `auth-server` first, because `agent-pi` asks for what `clients.yaml` allows. Then set these on `agent-pi`:

```sh
COLLAB_MCP_URL=http://t10-collab:8082/mcp
GRANT_ACTIONS=agent:use,work:read,work:write
GRANT_AUDIENCES=agent-pi,t10-collab
```

A conversation keeps the grant it opened with, so one from before this change has no access to the tools. A new conversation does.

## Test

```sh
TEST_DATABASE_URL='postgres://…' go test ./...
```

Tests need a throwaway Postgres, because they drop the `collab` schema. Without the variable, the database tests skip.

## Layout

| Package | Owns |
| --- | --- |
| `internal/workitems` | Items, status, owners, collaborators, comments |
| `internal/offers` | Sending, accepting, declining, withdrawing |
| `internal/lists` | Shared lists, their members and their unit links |
| `internal/sweep` | The 15-minute job: team-linked lists, and moving work from people who leave |
| `internal/policy` | Every access rule, tenant settings, and the SQL filter for "items this user can see" |
| `internal/activity` | Item history and in-app notifications |
| `third_party/t10-modkit/people` | Org reads from the auth service. They fail closed |
| `internal/server` | HTTP routes, with the required action declared on each one, and the agent tools in `mcp.go` |
| `verifier` | Copied from `auth-server`, which has no remote to import from. Keep the two identical |
| `docs/openapi.yaml` | The API. A test keeps it in step with the routes |

This service meets the module contract in `../contracts/module/README.md`. Clients find it through `GET /v1/module`, and `GET /internal/v1/attention` answers only `t10-notify`'s service token.

A forbidden item answers `404`, the same as a missing one. A caller who can see an item, but can't do what they asked, gets `403`.

## License

Apache License 2.0. See [LICENSE](LICENSE).

Shared org reads, IDs and pagination now come from `t10-modkit`. Until its first published release, this repo carries a source snapshot under `third_party/t10-modkit` so standalone builds require no sibling checkout. The workspace `scripts/check-modkit.sh` checks snapshot parity.
