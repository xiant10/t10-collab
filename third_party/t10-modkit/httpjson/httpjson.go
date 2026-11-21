// Package httpjson writes and reads the JSON bodies of t10's APIs, including the
// one error body every t10 API answers with:
//
//	{"error": "scope_unavailable", "message": "Optional sentence for a person."}
//
// error is a stable snake_case code a client may branch on; message is optional.
// OAuth endpoints and OpenAI-compatible routes keep the shape their protocol fixes.
package httpjson

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// ErrorBody is the error every t10 API answers with.
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// Write sends body as JSON with the status. API answers are never cached.
func Write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Error sends the error body. message may be empty.
func Error(w http.ResponseWriter, status int, code, message string) {
	Write(w, status, ErrorBody{Error: code, Message: message})
}

// Decode reads the request's JSON body into into. On a body that isn't valid JSON
// or carries more than one value, it answers 400 invalid_request itself and returns
// false; on one over a limit the route set with http.MaxBytesReader, 413 too_large.
func Decode(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(into)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("more than one JSON value")
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			Error(w, http.StatusRequestEntityTooLarge, "too_large", "the body is larger than this route takes")
			return false
		}
		Error(w, http.StatusBadRequest, "invalid_request", "the body isn't valid JSON for this route")
		return false
	}
	return true
}
