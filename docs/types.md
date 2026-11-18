# Item types

A module whose work is a request between people can keep it in collab as a **typed item**, instead of building its own offers, inboxes, history, notifications and departures. The module declares the type; collab checks the declaration and runs no module code. An item with no type is `collab.task` and behaves as it always has.

The design and the steps still to come are in the workspace's [`proposals/collab-item-types.md`](../../proposals/collab-item-types.md). This page is what's built: step 1.

## What a type gives an item

| Part | Built | Means |
| --- | --- | --- |
| `fields` | Yes | Values checked on every write against the type's fields |
| `subject` | Yes | The kinds of record, `<module>:<type>`, an item can be about. The item holds one `subject_ref`, `<module>:<type>:<id>`, which grants nothing |
| `visibility` | `team`, `participants` | Who reads it. `scoped` waits for scoped entitlements |
| `departure` | Yes | What the sweep does once all its owners have left |
| States, offer mappings, transitions | No | Step 2. A definition with them is refused, so a module can't think they're enforced |

## Declaring a type

The owning module pushes each type's default with its own service token when it starts, and whenever the type changes:

```http
PUT /internal/v1/types/sales.presales
Authorization: Bearer <t10-sales service token>

{"id": "sales.presales", "owner_module": "sales", "version": 1, "label": "Presales",
 "fields": {"properties": {
   "when": {"type": "string", "maxLength": 200, "title": "When"},
   "what": {"type": "string", "maxLength": 2000},
   "for_whom": {"type": "string", "enum": ["buyer", "boss"]}}},
 "subject": ["sales:deal", "sales:meeting"],
 "visibility": "participants", "departure": "cancel"}
```

- The token's service must be `t10-<module>` for a type `<module>.<name>`. No module can declare another's types, and `collab` is reserved.
- The same version and definition again is a no-op, so every replica pushes at start. A lower version, or the same one with a different definition, answers `409 type_version_stale`.
- `visibility` and `departure` never change for a type ID (`409 type_fixed`). They're copied onto each item, so the read filters need no join, and a module can't weaken what its items promised.
- Collab never calls the module. A module that's down changes nothing.

**Fields** are a small part of JSON Schema that any client can draw: `type` (`string`, `integer`, `number`, `boolean`), `title`, `maxLength` (strings; 2000 when left out), `enum`. Other keywords are refused. Clients show fields in the order they were declared, which collab keeps as `fields.order`. An item's fields are capped at 32 KB.

**Versions.** An item stores the `type_version` it was last written under. A write moves it to the current version. Values for fields the current version no longer declares are kept and shown, but can't be written.

## Using a typed item

People's routes and agent tools are the same as for any item:

- `POST /v1/work-items` and `work_create` take `type`, `fields` and `subject_ref`.
- `PATCH /v1/work-items/{id}` and `work_update` take `fields`, merged key by key, with null clearing one.
- `GET /v1/work-items` and `work_list` filter by `type`, `module` and `subject`. Sales' "this deal's asks" is `?subject=sales:deal:d_12`, through the usual read rule.
- `GET /v1/types` lists the tenant's types, `collab.task` first.

Status, offers, comments, history, notifications, Home and attention counts work on typed items as on any other.

## Visibility

| Profile | Who reads |
| --- | --- |
| `team` | Collab's own rule: creator, owners, collaborators, list members, the recipient of a pending offer, and managers per the tenant's `manager_visibility` |
| `participants` | The creator, owners and the recipient of a pending offer only |

A `participants` item can't be put on a list (`409 not_for_type`, and a database check backs it), can't take collaborators, and is never shown to managers, whatever the tenant's setting: the setting is a ceiling a type can lower. An admin reassigning one from the departures queue may give it only to its creator.

## Departures

| `departure` | When all owners have left |
| --- | --- |
| `reassign` | Collab's own order: offered to the creator, then each departed owner's manager, then the admin queue. `team` types only |
| `creator` | Offered to the creator only. If the creator has left, declines or lets it expire, it's cancelled. The default for `participants` |
| `cancel` | Cancelled at once. The event's detail says `reason: owners_departed`, and the creator is told |

## Rules for changes

- **No type is named in code.** A test fails if non-test code mentions the fixture modules. Collab enforces declarations; anything a module does with its items, it does after reading them.
- **The `participants` rule is checked wherever an item reaches someone new:** `policy.ReportsSQL` and `workitems.ViewAs` (managers), `Create` and `SetList` (lists), `AddCollaborator`, `Reassign`, and the sweep's `successor`. A new way to show an item to someone must check `Item.Participants()`.
