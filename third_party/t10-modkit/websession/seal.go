package websession

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// sealer encrypts what the service stores, with AES-256-GCM. The additional
// data ties a value to its purpose, so a value can't be moved between columns.
type sealer struct {
	aead cipher.AEAD
}

func newSealer(key []byte) (*sealer, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("session key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("session key: %w", err)
	}
	return &sealer{aead: aead}, nil
}

func (s *sealer) seal(plain []byte, purpose string) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, plain, []byte(purpose))
}

func (s *sealer) open(sealed []byte, purpose string) ([]byte, error) {
	if len(sealed) < s.aead.NonceSize() {
		return nil, errors.New("sealed value is too short")
	}
	nonce, body := sealed[:s.aead.NonceSize()], sealed[s.aead.NonceSize():]
	return s.aead.Open(nil, nonce, body, []byte(purpose))
}

// Sealer is the same sealing for a caller with its own store, such as a module
// that keeps its session in a cookie: AES-256-GCM, the nonce first, with the
// purpose as additional data so a value can't be moved to another use.
type Sealer struct{ s *sealer }

// NewSealer takes a 32-byte key.
func NewSealer(key []byte) (Sealer, error) {
	s, err := newSealer(key)
	return Sealer{s: s}, err
}

// Seal encrypts plain for one purpose.
func (s Sealer) Seal(plain []byte, purpose string) []byte { return s.s.seal(plain, purpose) }

// Open decrypts what Seal made for the same purpose.
func (s Sealer) Open(sealed []byte, purpose string) ([]byte, error) { return s.s.open(sealed, purpose) }
