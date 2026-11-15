# List workflows

Status: built, November 14, 2026; several holders, skipping and starting new items added the same day. Checked on the local cluster with three Claude Code runners (t10-agent-control's `scripts/sdlc-workflow-cluster.mjs`). A team's route for its items, such as Build → Review → Test → Approve, kept on its shared list and changed there. See t10-agent-control's `docs/sdlc-team.md` for the team it was made for.

## Why

An SDLC team passes each item along the same route, and sends it back to an earlier step when a check fails. Before this, the route lived only in each teammate's instructions ("then send it to Reviewer"), so:

- changing the route meant editing every teammate's instructions;
- nobody could see where an item was on the route, only who held it;
- a loop such as Builder → Reviewer → Tester → Builder ran into the agent hop limit: the third agent-to-agent send went to the sender's manager, so a failed test could never reach Builder.

## The model

A shared list can have one **workflow**: stages in order. Each stage has

- a **name**, such as Review;
- **holders**: one or more list members, people or agent teammates. The item goes to one of them: the one that held it before, so changes go back to the builder that built it, or else the one with the fewest open items on the list, the first named on a tie;
- **on changes**: the earlier stage an item goes back to when its holder finds something, or none when the stage only passes;
- a **guide**: a short note for whoever holds it, such as "Run make e2e on the branch";
- **skip when**: when the holder before may pass the item over this stage, such as "Only docs changed", or none when it can't be skipped. Not on the first stage.

And the workflow has

- **rounds**: how many times an item may go back before it goes to the lead instead, 3 by default;
- a **lead**: the person a round too many, or a stage whose holder can no longer take it, goes to. A list owner, the first owner by default;
- **start new items**: whether each new item on the list starts at the first stage by itself.

An item on the list is **on the workflow** once it's started at a stage. Its holder finishes the stage with one of two results:

| Result | The item goes to |
| --- | --- |
| `pass` | The next stage's holder. Passing the last stage marks the item done. |
| `pass` with `to` | A later stage's holder, over stages whose "skip when" the holder judges true. The note says why; the history keeps what was skipped. |
| `changes` | The stage's "on changes" stage's holder, with the note saying what to fix. A round more. Past the rounds, the lead, off the workflow. |

A move is an assignment, not an offer: the list's owner set the route, so the next holder needn't accept. The holder before becomes a collaborator, as with a send, so it's told but not woken until the item comes back or someone mentions it.

## Starting, moving and leaving

- **Start**: an owner of the item, or a list owner, starts it at a stage, the first by default. Any list member may start an item nobody owns at the first stage, as they may claim it. In Workbench, the task page offers "Start at Build" on an item on a list with a workflow. With "start new items" on, a new item on the list starts at the first stage as it's made, and the list's agents aren't told of it to claim it; an item sent to someone as it's made, or made with a suggestion, goes there instead.
- **Finish a stage**: the item's owner at that stage, by `POST /v1/work-items/{id}/advance` or the agent tool `work_advance`. People get "Pass to Review" and "Send back to Build" on the task page.
- **Move**: a list owner can put an item at any stage, which resets its rounds. This is how a person steps in.
- **Leave**: a transfer outside the workflow, once it's taken, or marking the item done or cancelled by hand, takes it off the workflow; the history keeps where it was. A share, or a comment, leaves it where it is.

## Changing the workflow

A list owner edits the workflow on the list's page, under Workflow: stages in order, each with its holder, where changes go back to and a guide, plus the lead and the rounds. A new workflow can start from Build, Review, Test, Approve. Stages keep their IDs across edits, so items in flight stay where they are. An item at a removed stage stays with its holder, off the workflow, and its history says why. A change takes effect at the next move; the holder of a stage whose holder changed keeps the item until it passes.

**Holders agree first.** A move gives an item without asking, and only the reporting line may do that between people. So a person who is a plain member of the list holds stages only once they agree: saving the workflow tells each one newly named, and they agree on the list's page (`PUT /v1/lists/{id}/holding`), or stop (`DELETE`), which leaves what they hold with them. Until then, moves pass them over as they would a holder who has left. List owners agreed by owning the list, and keep holding if they're made members; agent accounts agree through their intake.

Saving checks that every holder is a list member, and that every agent holder takes work from the list: its "Who can send it work" includes the list, or everyone. So the route can't reach an agent its admin didn't open to the team. Each move checks again, so a holder taken off the list or out of the organization, a plain member who hasn't agreed or has stopped, or an agent its admin has since closed to the list, doesn't get items: another holder of the stage does, or else the lead.

## When the list's members change

The workflow follows the list's members, however they change: removed by an owner, leaving, a role change, a linked team's sync, or leaving the organization.

- A holder no longer on the list leaves each stage that has other holders. A stage whose only holder left keeps them, so its items go to the lead until an owner changes it; the list page says so before an owner removes them.
- A lead who is no longer an owner gives way to the person who has owned the list longest, who is told. With no person owning the list, the admin queue gives it an owner, who becomes the lead.

Either change is recorded as `list.workflow_changed` with the reason `members_changed`.

## On the list

The list's page shows the workflow above its tasks, and groups open tasks by stage, each with its holder, then those not on the workflow. On a list with a workflow, a task nobody owns offers "Start at Build" rather than Claim. The lists page marks lists that have a workflow.

## Agents and the hop limit

Workflow moves are the route a person set, so they don't count as agent hops: a move resets the item's hops, as a person acting does. Loops are bounded by rounds instead, and the round too many goes to a person. Plain sends between agents keep the hop limit.

`work_get` shows an item's workflow: its stage, the stage's guide, where `pass` and `changes` go, and its rounds. The runner tells its agent to finish a stage with `work_advance` when the item is on a workflow, so a teammate's instructions say only how to do its role, not where to send the item.

## API

| Route | Who | What |
| --- | --- | --- |
| `PUT /v1/lists/{id}/workflow` | List owner | Set the workflow: `stages`, `rounds`, `lead` |
| `DELETE /v1/lists/{id}/workflow` | List owner | Remove it; items stay with their holders |
| `GET /v1/lists/{id}` | List member | Includes `workflow` |
| `PUT`, `DELETE /v1/lists/{id}/holding` | A plain member | Agree to hold stages, or stop |
| `PUT /v1/work-items/{id}/list` | Item owner | Put an item made elsewhere on the list, where "start new items" starts it |
| `POST /v1/work-items/{id}/stage` | Item owner off the workflow, or list owner | Start it, or put it at `stage` |
| `POST /v1/work-items/{id}/advance` | The item's holder | `result` pass or changes, with a `note`; `to` a stage from `skip` |
| `GET /v1/work-items/{id}`, tool `work_get` | Readers | `workflow`: the workflow, the item's `stage`, where `pass` and `changes` take it, and the stages it may `skip` to |
| Tool `work_advance` | Agent teammates | As `advance` |

## Data

`collab.list_workflows` holds a list's workflow as one JSON document with a version, and `auto_start`; `collab.work_items` gains `stage` and `rounds`. A move is recorded as an `assigned` offer, as a send is, and the event `workflow.moved` with the stages, the result, the round and the note.

## Not now

- Conditions t10 checks itself, such as on changed files. "Skip when" is judged by the holder before, person or agent, who says why.
- A stage that waits for whoever on the list claims it. Several holders share a stage by turns instead.
- Workflows outside a list.
