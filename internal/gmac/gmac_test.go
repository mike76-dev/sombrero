package gmac

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

var (
	testKey   = bytes.Repeat([]byte{0x2b}, 16)
	testNonce = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
)

// gcmTag is what the standard library makes of the same message under the same key and nonce.
// GMAC is GCM over a message that is all associated data and no plaintext, so the tag the one
// produces is the tag the other has to produce; the library is the authority on which it is.
func gcmTag(t *testing.T, key, nonce, msg []byte) []byte {
	t.Helper()

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("could not build the cipher: %v", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("could not build the cipher: %v", err)
	}

	return aead.Seal(nil, nonce, nil, msg)
}

// TestGMACMatchesGCM is the whole of what this package has to get right, measured against the
// implementation of the same thing in the standard library.
func TestGMACMatchesGCM(t *testing.T) {
	for _, tt := range []struct {
		name string
		msg  []byte
	}{
		{"nothing at all", nil},
		{"a single byte", []byte{0x61}},
		{"less than a block", []byte("sombrero")},
		{"exactly a block", bytes.Repeat([]byte{0x5a}, 16)},
		{"a block and a bit", bytes.Repeat([]byte{0x5a}, 17)},
		{"several blocks", bytes.Repeat([]byte("sombrero"), 64)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g, err := New(testKey, testNonce)
			if err != nil {
				t.Fatalf("could not build the signer: %v", err)
			}

			g.Write(tt.msg)

			if got, want := g.Sum(nil), gcmTag(t, testKey, testNonce, tt.msg); !bytes.Equal(got, want) {
				t.Fatalf("the tag is %x, want %x", got, want)
			}
		})
	}
}

// TestGMACWritesAddUp is the message handed over in pieces. A signature is over the message, not
// over the way it arrived, so how it was written must not change what comes out.
func TestGMACWritesAddUp(t *testing.T) {
	msg := bytes.Repeat([]byte("sombrero"), 32)

	whole, err := New(testKey, testNonce)
	if err != nil {
		t.Fatalf("could not build the signer: %v", err)
	}
	whole.Write(msg)

	pieces, err := New(testKey, testNonce)
	if err != nil {
		t.Fatalf("could not build the signer: %v", err)
	}
	for _, at := range []int{0, 1, 7, 16, 17, 64} {
		if at > len(msg) {
			break
		}
		pieces.Write(msg[:at])
		msg = msg[at:]
	}
	pieces.Write(msg)

	if !bytes.Equal(whole.Sum(nil), pieces.Sum(nil)) {
		t.Fatal("a message written in pieces did not come out as the same message")
	}
}

// TestGMACSumLeavesTheMessageAlone is the caller that takes the tag and carries on. Sum answers
// about what has been written so far; it does not consume it.
func TestGMACSumLeavesTheMessageAlone(t *testing.T) {
	g, err := New(testKey, testNonce)
	if err != nil {
		t.Fatalf("could not build the signer: %v", err)
	}

	g.Write([]byte("sombrero"))
	first := g.Sum(nil)

	if second := g.Sum(nil); !bytes.Equal(first, second) {
		t.Fatal("asking twice gave two different tags")
	}
	if want := gcmTag(t, testKey, testNonce, []byte("sombrero")); !bytes.Equal(first, want) {
		t.Fatal("the tag is not the one over what was written")
	}
}

// TestGMACSumAppends is the contract of the interface: Sum puts the tag behind what it was given
// rather than in place of it.
func TestGMACSumAppends(t *testing.T) {
	g, err := New(testKey, testNonce)
	if err != nil {
		t.Fatalf("could not build the signer: %v", err)
	}
	g.Write([]byte("sombrero"))

	prefix := []byte("before")
	got := g.Sum(prefix)

	if !bytes.HasPrefix(got, prefix) {
		t.Fatal("what came back does not start with what was handed in")
	}
	if len(got) != len(prefix)+g.Size() {
		t.Fatalf("what came back is %d bytes long, want %d", len(got), len(prefix)+g.Size())
	}
}

// TestGMACReset starts the message over, which is what a signer reused for the next message does.
func TestGMACReset(t *testing.T) {
	g, err := New(testKey, testNonce)
	if err != nil {
		t.Fatalf("could not build the signer: %v", err)
	}

	g.Write([]byte("the message before"))
	g.Reset()
	g.Write([]byte("sombrero"))

	if want := gcmTag(t, testKey, testNonce, []byte("sombrero")); !bytes.Equal(g.Sum(nil), want) {
		t.Fatal("what was written before the reset is still part of the tag")
	}
}

// TestGMACTagDependsOnTheNonce is the message signed twice under different nonces. The nonce is
// what tells one message from another and one direction from the other, so it has to reach the
// tag: a tag that ignored it could be lifted from one message onto another.
func TestGMACTagDependsOnTheNonce(t *testing.T) {
	msg := []byte("sombrero")

	// The bit is turned over rather than set, so that the two nonces differ whatever the one
	// they are built from happens to hold in that place.
	other := bytes.Clone(testNonce)
	other[8] ^= 1

	first, err := New(testKey, testNonce)
	if err != nil {
		t.Fatalf("could not build the signer: %v", err)
	}
	first.Write(msg)

	second, err := New(testKey, other)
	if err != nil {
		t.Fatalf("could not build the signer: %v", err)
	}
	second.Write(msg)

	if bytes.Equal(first.Sum(nil), second.Sum(nil)) {
		t.Fatal("the same message under two nonces came out with the same tag")
	}
}

// TestGMACSizes are the two the interface is asked for. A caller that trims a tag to Size, or
// that buffers by BlockSize, gets what it was promised.
func TestGMACSizes(t *testing.T) {
	g, err := New(testKey, testNonce)
	if err != nil {
		t.Fatalf("could not build the signer: %v", err)
	}

	if g.Size() != 16 {
		t.Errorf("the tag is said to be %d bytes long, want 16", g.Size())
	}
	if g.BlockSize() != aes.BlockSize {
		t.Errorf("the block is said to be %d bytes long, want %d", g.BlockSize(), aes.BlockSize)
	}
	if got := len(g.Sum(nil)); got != g.Size() {
		t.Errorf("the tag came out %d bytes long, want the %d it says", got, g.Size())
	}
}

// TestGMACRefusesWhatItCannotSignWith is the key or the nonce that is not of a size the cipher
// works in. It is answered with an error rather than a signer that would sign wrongly.
func TestGMACRefusesWhatItCannotSignWith(t *testing.T) {
	for _, tt := range []struct {
		name  string
		key   []byte
		nonce []byte
	}{
		{"a key of no length", nil, testNonce},
		{"a key of the wrong length", bytes.Repeat([]byte{1}, 15), testNonce},
		{"a nonce of no length", testKey, nil},
		{"a nonce too short", testKey, make([]byte, 11)},
		{"a nonce too long", testKey, make([]byte, 13)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.key, tt.nonce); err == nil {
				t.Fatal("a signer was built out of something it cannot sign with")
			}
		})
	}
}
