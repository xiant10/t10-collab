// Package ids generates the prefixed random IDs used across the service.
package ids

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns prefix_<32 hex characters>, for example usr_3fa9….
func New(prefix string) string {
	b := make([]byte, 16)
	rand.Read(b) // Never fails; see crypto/rand.
	return prefix + "_" + hex.EncodeToString(b)
}
