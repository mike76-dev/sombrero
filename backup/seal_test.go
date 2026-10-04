package backup

import (
	"bytes"
	"errors"
	"testing"
)

// TestSeal verifies that a sealed catalog opens with the key it was sealed
// under and with nothing else, and that bytes that are not one are told apart.
func TestSeal(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 64)
	plain := []byte("the catalog")

	sealed, err := Seal(key, plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, plain) {
		t.Error("the catalog is in the clear")
	}

	got, err := Open(key, sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Errorf("Open: got %q, %v", got, err)
	}

	other := bytes.Repeat([]byte{8}, 64)
	if _, err := Open(other, sealed); err == nil {
		t.Error("the catalog opened with another key")
	}

	tampered := append([]byte{}, sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, err := Open(key, tampered); err == nil {
		t.Error("a changed catalog opened")
	}

	if _, err := Open(key, []byte("just some bytes")); !errors.Is(err, ErrNotSealed) {
		t.Errorf("bytes that are not a sealed catalog: got %v", err)
	}
}
