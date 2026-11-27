package ontology

import (
	"errors"
	"net/http"
)

// Error is a refusal with the status and stable code the contract gives it. A
// module's functions return these, or any error wrapping one, and the routes and
// tools answer them as they are. Any other error answers 500 unavailable.
type Error struct {
	Status  int
	Code    string
	Message string
	// Errors are the parameter errors of invalid_params.
	Errors []FieldError
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

// Is matches two errors with the same code, so a module may return its own
// *Error with a message and still match ErrNotFound and the rest.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// FieldError is one parameter's problem. Param is empty for one about the whole
// action, such as forbidden or wrong_state.
type FieldError struct {
	Param string `json:"param"`
	Code  string `json:"code"`
}

// Fail builds an error with a status and code.
func Fail(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// The refusals the contract names.
var (
	ErrNotFound           = Fail(http.StatusNotFound, "not_found", "")
	ErrForbidden          = Fail(http.StatusForbidden, "forbidden", "")
	ErrWrongState         = Fail(http.StatusConflict, "wrong_state", "")
	ErrStaleRevision      = Fail(http.StatusConflict, "stale_revision", "")
	ErrUndoConflict       = Fail(http.StatusConflict, "undo_conflict", "a later change depends on it")
	ErrFilterNotAllowed   = Fail(http.StatusBadRequest, "filter_not_allowed", "")
	ErrTooManyProposals   = Fail(http.StatusConflict, "too_many_proposals", "there are too many drafts waiting; confirm or dismiss some first")
	ErrProposalClosed     = Fail(http.StatusConflict, "proposal_closed", "")
	ErrUndone             = Fail(http.StatusConflict, "undone", "it was already undone")
	ErrRequestInProgress  = Fail(http.StatusConflict, "request_in_progress", "another request with this key is running or stopped before it answered; check the result, or use a new key")
	ErrPersonTokenNeeded  = Fail(http.StatusUnauthorized, "person_token_required", "this route takes a person's own token")
	ErrNotAllowedForAgent = Fail(http.StatusForbidden, "not_allowed_for_agents", "")
	errUnavailable        = Fail(http.StatusInternalServerError, "unavailable", "")
)

// reasonStatus is the status a refusal reason answers with on apply.
func reasonError(reason string) *Error {
	if reason == "forbidden" || reason == "" {
		return ErrForbidden
	}
	return Fail(http.StatusConflict, reason, "")
}

func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return errUnavailable
}
