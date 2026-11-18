# Item types

A module whose work is a request between people can keep it in collab as a **typed item**, instead of building its own offers, inboxes, history, notifications and departures. The module declares the type; collab checks the declaration and runs no module code. An item with no type is `collab.task` and behaves as it always has.

The design and the steps still to come are in the workspace's [`proposals/collab-item-types.md`](../../proposals/collab-item-types.md). This page is what's built: steps 1 and 2.

## What a type gives an item

| Part | Built | Means |
| --- | --- | --- |
| `fields` | Yes | Values checked on every write against the type's fields |
| `subject` | Yes | The kinds of record, `<module>:<type>`, an item can be about. The item holds one `subject_ref`, `<module>:<type>:<id>`, which grants nothing |
| `visibility` | `team`, `participants` | Who reads it. `scoped` waits for scoped entitlements |
| `departure` | Yes | What the sweep does once all its owners have left |
| `states`, `offers`, `transitions` | Yes | A lifecycle of its own: see [States and moves](#states-and-moves) |
| Event feed, tenant versions, `scoped` | No | Steps 3 to 5 |

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

## States and moves

A type with `states` gives its items a lifecycle. Each state has a `phase`, one of collab's four statuses, and the item's `status` is always its state's phase, so Home, filters, attention counts and the sweep work unchanged.

```json
"states": {"draft": {"phase": "todo", "label": "Draft"}, "sent": {"phase": "todo", "label": "Sent"},
           "accepted": {"phase": "doing", "label": "Accepted"}, "delivered": {"phase": "doing", "label": "Delivered"},
           "reviewed": {"phase": "done", "label": "Reviewed"}, "declined": {"phase": "cancelled", "label": "Declined"},
           "cancelled": {"phase": "cancelled", "label": "Cancelled"}},
"initial": "draft", "cancelled": "cancelled",
"offers": {"send": {"from": ["draft"], "to": "sent", "requires": ["what", "for_whom"]},
           "accept": {"to": "accepted"}, "decline": {"to": "declined", "note": "required"}, "withdraw": {"to": "draft"}},
"transitions": [{"id": "deliver", "label": "Deliver", "from": ["accepted"], "to": "delivered", "by": "owner", "payload": ["reply", "links"]},
                {"id": "review", "from": ["delivered"], "to": "reviewed", "by": "creator"},
                {"id": "cancel", "from": ["draft", "sent", "accepted"], "to": "cancelled", "by": "creator"}]
```

- **`initial`** is a todo or doing state new items start in. **`cancelled`** is the state collab moves an item to itself, as when its owners leave; a type whose `departure` isn't `reassign` must name one.
- **Offers stay the only way ownership changes.** `offers` says what each *transfer* offer event does to the state. A `share` send never moves it. A mapping that's left out leaves the state alone.
  - `send` and `assign` may name `from` states and `requires` fields. A send that doesn't fit fails whole: no offer is made. A send down the reporting line is an assignment, which skips acceptance, so a type with `send` but no `assign` refuses it (`409 transition_not_allowed`).
  - `accept`, `decline` and `withdraw` move only an item still in the state `send` moved it to, unless they name `from`. `decline` may require a note.
  - `withdraw` covers every unanswered end: withdrawn, voided because the recipient left, or expired. An offer ended because the item closed or was deleted doesn't apply it.
  - The state moves before ownership does, so the offers an accept ends find the item already past them.
- **Transitions** are the other moves. `by` is one or more of `creator`, `owner`, `collaborator`, `recipient`, `list_member`: relations `policy` already computes. `requires` names fields that must be filled, or payload keys that must be given. `payload` allows `reply` (text, up to 10,000 characters) and `links` (up to 20 `{url, title}`, https only), stored on the move's event as its result. `note: required` needs a note.
- **Nothing else moves the state.** `PATCH` with `status` answers `409 transition_not_allowed`, and a typed item with states never goes on a list workflow (`409 typed_item_not_on_workflow`; auto-start leaves it alone).
- **`moves`.** `GET /v1/work-items/{id}` and `work_get` list the transitions the caller may take now, with what each needs, so a client draws a button per move.
- **A state a later version removed** can only be left by a transition to a cancelled state.

Every state change is one `item.state_changed` event with `from_state`, `to_state`, `status_from`, `status_to`, and `transition` or `offer_event`, plus any `note` and `payload`. Everyone on the item and its creator are told. A move into a closed phase ends its pending offers.

Agents take transitions with `work_transition`, which serves agent teammates only, like `work_send`. An assistant acting for a person reads and drafts, and the person makes the move.

## Visibility

| Profile | Who reads |
| --- | --- |
| `team` | Collab's own rule: creator, owners, collaborators, list members, the recipient of a pending offer, and managers per the tenant's `manager_visibility` |
| `participants` | The creator, owners and the recipient of a pending offer only |

A sender whose transfer is accepted stays on as a collaborator, as on any item, so whoever handed a `participants` item on can still read it. A `participants` item can't be put on a list (`409 not_for_type`, and a database check backs it), can't take collaborators, and is never shown to managers, whatever the tenant's setting: the setting is a ceiling a type can lower. An admin reassigning one from the departures queue may give it only to its creator.

## Departures

| `departure` | When all owners have left |
| --- | --- |
| `reassign` | Collab's own order: offered to the creator, then each departed owner's manager, then the admin queue. `team` types only |
| `creator` | Offered to the creator only. If the creator has left, declines or lets it expire, it's cancelled. The default for `participants` |
| `cancel` | Cancelled at once, into the type's `cancelled` state if it has states. The event's detail says `reason: owners_departed`, and the creator is told |

## Rules for changes

- **No type is named in code.** A test fails if non-test code mentions the fixture modules. Collab enforces declarations; anything a module does with its items, it does after reading them.
- **The `participants` rule is checked wherever an item reaches someone new:** `policy.ReportsSQL` and `workitems.ViewAs` (managers), `Create` and `SetList` (lists), `AddCollaborator`, `Reassign`, and the sweep's `successor`. A new way to show an item to someone must check `Item.Participants()`.
