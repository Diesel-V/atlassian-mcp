// Package crypto provides authenticated encryption for secrets at rest and in cookies.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrDecrypt is returned when a ciphertext is malformed or fails authentication.
var ErrDecrypt = errors.New("crypto: decryption failed")

// Sealer encrypts and decrypts small payloads with AES-256-GCM.
// The associated data binds a ciphertext to its context (e.g. column and owner),
// so a value copied to another row or purpose fails to decrypt.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer creates a Sealer from a 32-byte key.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext. The output is nonce||ciphertext.
func (s *Sealer) Seal(plaintext, associatedData []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plaintext, associatedData), nil
}

// Open decrypts a value produced by Seal.
func (s *Sealer) Open(sealed, associatedData []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n+s.aead.Overhead() {
		return nil, ErrDecrypt
	}
	pt, err := s.aead.Open(nil, sealed[:n], sealed[n:], associatedData)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// SealString encrypts a string and returns URL-safe base64.
func (s *Sealer) SealString(plaintext, associatedData string) (string, error) {
	b, err := s.Seal([]byte(plaintext), []byte(associatedData))
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// OpenString decrypts a value produced by SealString.
func (s *Sealer) OpenString(sealed, associatedData string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return "", ErrDecrypt
	}
	pt, err := s.Open(b, []byte(associatedData))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
