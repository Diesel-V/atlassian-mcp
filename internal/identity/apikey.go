package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// APIKeyPrefix marks personal keys so they can be told apart from JWTs and found by secret scanners.
const APIKeyPrefix = "amcp_"

// ErrMalformedKey is returned for strings that are not personal keys.
var ErrMalformedKey = errors.New("identity: malformed api key")

// NewAPIKey generates a key. The full key is shown to the user once;
// only id and HashSecret(secret) are stored.
func NewAPIKey() (full, id, secret string, err error) {
	idBytes := make([]byte, 8)
	secretBytes := make([]byte, 32)
	if _, err = rand.Read(idBytes); err != nil {
		return "", "", "", err
	}
	if _, err = rand.Read(secretBytes); err != nil {
		return "", "", "", err
	}
	id = hex.EncodeToString(idBytes)
	secret = base64.RawURLEncoding.EncodeToString(secretBytes)
	return APIKeyPrefix + id + "_" + secret, id, secret, nil
}

// ParseAPIKey splits a key into id and secret.
func ParseAPIKey(full string) (id, secret string, err error) {
	rest, ok := strings.CutPrefix(full, APIKeyPrefix)
	if !ok {
		return "", "", ErrMalformedKey
	}
	id, secret, ok = strings.Cut(rest, "_")
	if !ok || len(id) != 16 || len(secret) != 43 {
		return "", "", ErrMalformedKey
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", "", ErrMalformedKey
	}
	return id, secret, nil
}

// HashSecret returns the stored representation of a key secret.
// The secret has 256 bits of entropy, so a fast hash is sufficient.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// SecretMatches compares a presented secret with a stored hash in constant time.
func SecretMatches(secret, storedHash string) bool {
	return subtle.ConstantTimeCompare([]byte(HashSecret(secret)), []byte(storedHash)) == 1
}
