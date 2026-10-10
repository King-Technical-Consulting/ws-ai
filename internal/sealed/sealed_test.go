package sealed

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

func newKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestSealOpenRoundTrip(t *testing.T) {
	b, err := New(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("user-1|anthropic")
	sealed, err := b.Seal([]byte("sk-ant-secret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("sk-ant-secret")) {
		t.Fatal("the sealed value contains the plaintext")
	}
	got, err := b.Open(sealed, aad)
	if err != nil || string(got) != "sk-ant-secret" {
		t.Fatalf("open = %q, %v", got, err)
	}
	// Sealing twice gives different bytes (a fresh nonce each time).
	again, _ := b.Seal([]byte("sk-ant-secret"), aad)
	if bytes.Equal(sealed, again) {
		t.Error("two seals of one value are identical: the nonce is not fresh")
	}
}

func TestOpenRefusesWrongOwnerKeyAndTampering(t *testing.T) {
	b1, _ := New(newKey(t))
	b2, _ := New(newKey(t))
	sealed, _ := b1.Seal([]byte("secret"), []byte("alice|openai"))

	if _, err := b1.Open(sealed, []byte("bob|openai")); !errors.Is(err, ErrOpen) {
		t.Errorf("another owner opened it: %v", err)
	}
	if _, err := b1.Open(sealed, []byte("alice|anthropic")); !errors.Is(err, ErrOpen) {
		t.Errorf("another provider opened it: %v", err)
	}
	if _, err := b2.Open(sealed, []byte("alice|openai")); !errors.Is(err, ErrOpen) {
		t.Errorf("another key opened it: %v", err)
	}
	flipped := append([]byte(nil), sealed...)
	flipped[len(flipped)-1] ^= 1
	if _, err := b1.Open(flipped, []byte("alice|openai")); !errors.Is(err, ErrOpen) {
		t.Errorf("a tampered value opened: %v", err)
	}
	if _, err := b1.Open([]byte("short"), nil); !errors.Is(err, ErrOpen) {
		t.Errorf("a short value opened: %v", err)
	}
}

func TestNewRejectsBadKeys(t *testing.T) {
	for _, k := range []string{"", "not base64!!", base64.StdEncoding.EncodeToString([]byte("too short"))} {
		if _, err := New(k); err == nil {
			t.Errorf("New(%q) accepted a bad key", k)
		}
	}
}
