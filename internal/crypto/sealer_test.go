package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func newTestSealer(t *testing.T, b byte) *Sealer {
	t.Helper()
	s, err := NewSealer(bytes.Repeat([]byte{b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	s := newTestSealer(t, 1)
	sealed, err := s.SealString("secret-token", "oauth:alice:confluence")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.OpenString(sealed, "oauth:alice:confluence")
	if err != nil || got != "secret-token" {
		t.Fatalf("OpenString = %q, %v", got, err)
	}
}

func TestNonceIsRandom(t *testing.T) {
	s := newTestSealer(t, 1)
	a, _ := s.SealString("x", "ad")
	b, _ := s.SealString("x", "ad")
	if a == b {
		t.Fatal("two seals of the same plaintext are identical")
	}
}

func TestOpenFailures(t *testing.T) {
	s := newTestSealer(t, 1)
	sealed, _ := s.SealString("secret", "ad-1")

	tests := map[string]func() (string, error){
		"wrong associated data": func() (string, error) { return s.OpenString(sealed, "ad-2") },
		"wrong key":             func() (string, error) { return newTestSealer(t, 2).OpenString(sealed, "ad-1") },
		"not base64":            func() (string, error) { return s.OpenString("%%%", "ad-1") },
		"too short":             func() (string, error) { return s.OpenString("AAAA", "ad-1") },
		"tampered": func() (string, error) {
			b := []byte(sealed)
			b[len(b)-2] ^= 'A' ^ 'B'
			return s.OpenString(string(b), "ad-1")
		},
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := fn(); !errors.Is(err, ErrDecrypt) {
				t.Fatalf("err = %v, want ErrDecrypt", err)
			}
		})
	}
}

func TestNewSealerKeyLength(t *testing.T) {
	if _, err := NewSealer(make([]byte, 16)); err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}
