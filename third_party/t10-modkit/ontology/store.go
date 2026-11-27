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
	Object    string
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
	// Object is the reference the answer shows after an undo.
	Object    string
	Payload   json.RawMessage
	CreatedAt time.Time
	ExpiresAt time.Time
	Undone    bool
}

// Replay is an idempotent action's answer, kept under its key.
type Replay struct {
	Tenant, User, Action, Key string
	// Hash is the request body's, so the same key with another body is refused.
	Hash string
	// Answer is nil while the request that reserved the key is still running.
	Answer json.RawMessage
}

// UndoStore keeps undo entries and idempotent answers in the module's
// database. ontology/undo has one in memory and one in Postgres.
type UndoStore interface {
	SaveUndo(ctx context.Context, u UndoEntry) error
	// GetUndo is an entry by ID within the tenant, or ErrNotFound.
	GetUndo(ctx context.Context, tenant, id string) (UndoEntry, error)
	// SetUndone marks an entry undone or not, and answers ErrUndone when
	// marking one that already is.
	SetUndone(ctx context.Context, tenant, id string, undone bool) error
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
