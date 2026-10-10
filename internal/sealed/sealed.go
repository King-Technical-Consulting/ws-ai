// Package sealed seals small secrets (a person's own provider API key) for
// storage in the database. The server holds the only key, from
// WS_SECRETS_KEY; the database alone cannot open what it stores.
package sealed

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// Box seals and opens values with AES-256-GCM.
type Box struct{ aead cipher.AEAD }

// ErrOpen means a sealed value could not be opened: tampered, sealed for
// another owner, or sealed under a different key.
var ErrOpen = errors.New("sealed: cannot open")

// New builds a Box from a base64 encoded 32 byte key (WS_SECRETS_KEY).
func New(b64 string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		if key, err = base64.RawStdEncoding.DecodeString(b64); err != nil {
			return nil, errors.New("sealed: key is not base64")
		}
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("sealed: key must be 32 bytes, got %d", len(key))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext. aad binds the result to its owner (the user and
// provider ids), so a sealed value copied to another row will not open.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts a value made by Seal with the same aad.
func (b *Box) Open(sealed, aad []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, ErrOpen
	}
	out, err := b.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return nil, ErrOpen
	}
	return out, nil
}

// ProviderKeyAAD is the additional data a person's provider key is sealed
// with, binding it to the user and provider it belongs to.
func ProviderKeyAAD(userID, providerID string) []byte {
	return []byte(userID + "|" + providerID)
}
