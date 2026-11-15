# List workflows

Status: built, November 14, 2026, and checked on the local cluster with three Claude Code runners (t10-agent-control's `scripts/sdlc-workflow-cluster.mjs`). A team's route for its items, such as Build → Review → Test → Approve, kept on its shared list and changed there. See t10-agent-control's `docs/sdlc-team.md` for the team it was made for.

## Why

An SDLC team passes each item along the same route, and sends it back to an earlier step when a check fails. Before this, the route lived only in each teammate's instructions ("then send it to Reviewer"), so:

- changing the route meant editing every teammate's instructions;
- nobody could see where an item was on the route, only who held it;
- a loop such as Builder → Reviewer → Tester → Builder ran into the agent hop limit: the third agent-to-agent send went to the sender's manager, so a failed test could never reach Builder.

## The model

A shared list can have one **workflow**: stages in order. Each stage has

- a **name**, such as Review;
- a **holder**: a list member, person or agent teammate, who gets the item at that stage;
- **on changes**: the earlier stage an item goes back to when its holder finds something, or none when the stage only passes;
- a **guide**: a short note for whoever holds it, such as "Run make e2e on the branch".

And the workflow has

- **rounds**: how many times an item may go back before it goes to the lead instead, 3 by default;
- a **lead**: the person a round too many, or a stage whose holder can no longer take it, goes to. A list owner, the first owner by default.

An item on the list is **on the workflow** once it's started at a stage. Its holder finishes the stage with one of two results:

| Result | The item goes to |
| --- | --- |
| `pass` | The next stage's holder. Passing the last stage marks the item done. |
| `changes` | The stage's "on changes" stage's holder, with the note saying what to fix. A round more. Past the rounds, the lead, off the workflow. |

A move is an assignment, not an offer: the list's owner set the route, so the next holder needn't accept. The holder before becomes a collaborator, as with a send, so it's told but not woken until the item comes back or someone mentions it.

## Starting, moving and leaving

- **Start**: an owner of the item, or a list owner, starts it at a stage, the first by default. In Workbench, the task page offers "Start at Build" on an item on a list with a workflow.
- **Finish a stage**: the item's owner at that stage, by `POST /v1/work-items/{id}/advance` or the agent tool `work_advance`. People get "Pass to Review" and "Send back to Build" on the task page.
- **Move**: a list owner can put an item at any stage, which resets its rounds. This is how a person steps in.
- **Leave**: a transfer outside the workflow, once it's taken, or marking the item done or cancelled by hand, takes it off the workflow; the history keeps where it was. A share, or a comment, leaves it where it is.

## Changing the workflow

A list owner edits the workflow on the list's page, under Workflow: stages in order, each with its holder, where changes go back to and a guide, plus the lead and the rounds. A new workflow can start from Build, Review, Test, Approve. Stages keep their IDs across edits, so items in flight stay where they are. An item at a removed stage stays with its holder, off the workflow, and its history says why. A change takes effect at the next move; the holder of a stage whose holder changed keeps the item until it passes.

Saving checks that every holder is a list member, and that every agent holder takes work from the list: its "Who can send it work" includes the list, or everyone. So the route can't reach an agent its admin didn't open to the team. A holder taken off the list or out of the organization later doesn't get items: they go to the lead.

## Agents and the hop limit

Workflow moves are the route a person set, so they don't count as agent hops: a move resets the item's hops, as a person acting does. Loops are bounded by rounds instead, and the round too many goes to a person. Plain sends between agents keep the hop limit.

`work_get` shows an item's workflow: its stage, the stage's guide, where `pass` and `changes` go, and its rounds. The runner tells its agent to finish a stage with `work_advance` when the item is on a workflow, so a teammate's instructions say only how to do its role, not where to send the item.

## API

| Route | Who | What |
| --- | --- | --- |
| `PUT /v1/lists/{id}/workflow` | List owner | Set the workflow: `stages`, `rounds`, `lead` |
| `DELETE /v1/lists/{id}/workflow` | List owner | Remove it; items stay with their holders |
| `GET /v1/lists/{id}` | List member | Includes `workflow` |
| `POST /v1/work-items/{id}/stage` | Item owner off the workflow, or list owner | Start it, or put it at `stage` |
| `POST /v1/work-items/{id}/advance` | The item's holder | `result` pass or changes, with a `note` |
| `GET /v1/work-items/{id}`, tool `work_get` | Readers | `workflow`: the workflow, the item's `stage`, and where `pass` and `changes` take it |
| Tool `work_advance` | Agent teammates | As `advance` |

## Data

`collab.list_workflows` holds a list's workflow as one JSON document with a version; `collab.work_items` gains `stage` and `rounds`. A move is recorded as an `assigned` offer, as a send is, and the event `workflow.moved` with the stages, the result, the round and the note.

## Not now

- Branches and conditions, such as skipping Test for docs-only changes. A stage holder can pass at once.
- Several holders per stage, or a stage that goes to whoever on the list claims it.
- Starting each new list item automatically.
- Workflows outside a list.
