package ontology

import (
	"context"
	"encoding/json"
	"time"
)

// ProposalLife is how long a draft waits; after that the facts it describes are old.
const ProposalLife = 7 * 24 * time.Hour

// ProposalLimit is how many open drafts one person holds per module.
const ProposalLimit = 50

// Proposal is an action an agent drafted for the person it acts for.
type Proposal struct {
	ID, Tenant, User string
	// Agent names the agent that drafted it.
	Agent  string
	Action string
	// Object is the reference acted on, empty for a create.
	Object string
	// Revision is the object's revision when the draft was made, so a confirm
	// after it changed is refused; 0 for a create, or a draft kept from before.
	Revision  int64
	Params    json.RawMessage
	State     string // open, confirmed or dismissed
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ProposalStore keeps drafts in the module's database. ontology/proposals has
// one in memory and one in Postgres.
type ProposalStore interface {
	// CreateProposal stores an open draft, or ErrTooManyProposals when the
	// person already has limit open ones that haven't expired.
	CreateProposal(ctx context.Context, p Proposal, limit int, now time.Time) error
	// OpenProposals are one person's open, unexpired drafts, newest first.
	OpenProposals(ctx context.Context, tenant, user string, now time.Time) ([]Proposal, error)
	// GetProposal is one of the person's drafts in any state, or ErrNotFound,
	// also for someone else's.
	GetProposal(ctx context.Context, tenant, user, id string) (Proposal, error)
	// DecideProposal moves a draft from one state to another, or answers
	// ErrProposalClosed when it isn't in from.
	DecideProposal(ctx context.Context, tenant, user, id, from, to string) error
}

// UndoEntry is an applied action that its person can undo.
type UndoEntry struct {
	ID, Tenant, User string
	Action           string
	// Agent names the agent that applied it through its _act tool, and is
	// empty when the person applied it, themselves or by confirming a draft.
	Agent string
	// Object is the reference the answer shows after an undo.
	Object    string
	Payload   json.RawMessage
	CreatedAt time.Time
	ExpiresAt time.Time
	// Undone is set when an undo starts, and Reverted once its compensating
	// change is written. Undone without Reverted is an undo still running, or one
	// that was interrupted, which the person can run again.
	Undone   bool
	Reverted bool
}

// Replay is an idempotent action's answer, kept under its key. Action is the
// scope the key is kept under, the whole module ("*") since v0.3.0.
type Replay struct {
	Tenant, User, Action, Key string
	// Hash is the request body's, so the same key with another body is refused.
	Hash string
	// Answer is nil while the request that reserved the key is still running.
	Answer json.RawMessage
}

// UndoStore keeps undo entries and idempotent answers in the module's
// database. ontology/undo has one in memory and one in Postgres.
// DoneEntry is an action an agent applied for its person through its _act
// tool, kept so the person can see what was done for them.
type DoneEntry struct {
	ID, Tenant, User string
	Action, Object   string
	Agent            string
	// UndoID names the undo entry, for an undo action.
	UndoID    string
	CreatedAt time.Time
	// UndoOpen and UndoExpiresAt are read from the undo entry: whether it can
	// still be undone, and until when.
	UndoOpen      bool
	UndoExpiresAt time.Time
}

type UndoStore interface {
	SaveUndo(ctx context.Context, u UndoEntry) error
	// SaveDone records an action an agent applied for its person.
	SaveDone(ctx context.Context, d DoneEntry) error
	// RecentDone is the person's done entries made at or after since, newest
	// first, each saying whether its undo entry is still open at now.
	RecentDone(ctx context.Context, tenant, user string, since, now time.Time) ([]DoneEntry, error)
	// OpenUndos are the person's entries that can still be undone at now: not
	// undone, and before they expire. Newest first.
	OpenUndos(ctx context.Context, tenant, user string, now time.Time) ([]UndoEntry, error)
	// GetUndo is an entry by ID within the tenant, or ErrNotFound.
	GetUndo(ctx context.Context, tenant, id string) (UndoEntry, error)
	// SetUndone marks an entry's undo started or not, and answers ErrUndone when
	// marking one that already is.
	SetUndone(ctx context.Context, tenant, id string, undone bool) error
	// FinishUndo marks an entry reverted: its undo is done.
	FinishUndo(ctx context.Context, tenant, id string) error
	// GetReplay is what is kept under a key, and whether there is anything.
	GetReplay(ctx context.Context, tenant, user, action, key string) (Replay, bool, error)
	// ReserveReplay claims a key, with r.Hash and no answer, before the action
	// runs, so two requests with one key never both run it. When another
	// request holds the key it answers what that one keeps, and false.
	ReserveReplay(ctx context.Context, r Replay) (Replay, bool, error)
	// FinishReplay keeps the answer under a key this request reserved.
	FinishReplay(ctx context.Context, r Replay) error
	// ReleaseReplay frees a key whose action failed before changing anything,
	// so a retry runs it. A key with an answer is kept.
	ReleaseReplay(ctx context.Context, tenant, user, action, key string) error
}
