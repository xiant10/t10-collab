// Package page encodes keyset cursors. Lists page by key, not offset, so a row
// added between two reads can't repeat or skip one.
package page

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrBadCursor means a client sent a cursor this service didn't issue.
var ErrBadCursor = errors.New("invalid cursor")

// Encode builds the cursor after a row ordered by (at, id).
func Encode(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%d|%s", at.UnixMicro(), id))
}

// Decode reads a cursor. An empty one means the first page.
func Decode(cursor string) (at time.Time, id string, ok bool, err error) {
	if cursor == "" {
		return time.Time{}, "", false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", false, ErrBadCursor
	}
	micros, id, found := strings.Cut(string(raw), "|")
	value, err := strconv.ParseInt(micros, 10, 64)
	if !found || err != nil || id == "" {
		return time.Time{}, "", false, ErrBadCursor
	}
	return time.UnixMicro(value), id, true, nil
}
