package cmac

import (
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"testing"
)

// unhex turns a written-out byte string into the bytes it stands for.
func unhex(t *testing.T, s string) []byte {
	t.Helper()

	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("could not read %q: %v", s, err)
	}

	return b
}

// TestCMACAgainstTheSpecExamples are the worked examples of SP 800-38B, which is the document
// this code says it implements. Nothing else in the package settles whether it does: a message
// authentication code that is self-consistent but not the one the specification describes agrees
// with nobody, and every peer would refuse everything it signed.
func TestCMACAgainstTheSpecExamples(t *testing.T) {
	// The key of the AES-128 examples, D.1.
	key := unhex(t, "2b7e151628aed2a6abf7158809cf4f3c")

	// The message of the examples, taken to the length each one calls for.
	msg := unhex(t, ""+
		"6bc1bee22e409f96e93d7e117393172a"+
		"ae2d8a571e03ac9c9eb76fac45af8e51"+
		"30c81c46a35ce411e5fbc1191a0a52ef"+
		"f69f2445df4f9b17ad2b417be66c3710")

	for _, tt := range []struct {
		name string
		len  int
		want string
	}{
		{"an empty message", 0, "bb1d6929e95937287fa37d129b756746"},
		{"one block", 16, "070a16b46b4d4144f79bdd9dd04a287c"},
		{"two blocks and a half", 40, "dfa66747de9ae63030ca32611497c827"},
		{"four blocks", 64, "51f0bebf7e3b9d92fc49741779363cfe"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ciph, err := aes.NewCipher(key)
			if err != nil {
				t.Fatalf("could not build the cipher: %v", err)
			}

			d := New(ciph)
			d.Write(msg[:tt.len])

			if got, want := d.Sum(nil), unhex(t, tt.want); !bytes.Equal(got, want) {
				t.Fatalf("the code came out %x, want the %x of the specification", got, want)
			}
		})
	}
}

// TestCMACWritesAddUp is the message handed over in pieces. The code is over the message, so how
// it arrived must not reach the answer — and the state is carried between writes a block at a
// time, which is exactly where that could go wrong.
func TestCMACWritesAddUp(t *testing.T) {
	key := bytes.Repeat([]byte{0x2b}, 16)
	msg := bytes.Repeat([]byte("sombrero"), 32)

	ciph, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("could not build the cipher: %v", err)
	}

	whole := New(ciph)
	whole.Write(msg)
	want := whole.Sum(nil)

	// The message is cut at every length up to a few blocks, so a boundary that is mishandled
	// shows up wherever it is.
	for at := 0; at <= 48 && at <= len(msg); at++ {
		d := New(ciph)
		d.Write(msg[:at])
		d.Write(msg[at:])

		if got := d.Sum(nil); !bytes.Equal(got, want) {
			t.Fatalf("cut at %d the code came out %x, want %x", at, got, want)
		}
	}
}

// TestCMACSumLeavesTheMessageAlone is what the code promises in as many words: the caller may
// take a code and go on writing. Sum answers about what has been written so far without
// disturbing it.
func TestCMACSumLeavesTheMessageAlone(t *testing.T) {
	key := unhex(t, "2b7e151628aed2a6abf7158809cf4f3c")
	msg := unhex(t, "6bc1bee22e409f96e93d7e117393172a"+"ae2d8a571e03ac9c9eb76fac45af8e51")

	ciph, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("could not build the cipher: %v", err)
	}

	going := New(ciph)
	going.Write(msg[:16])
	going.Sum(nil) // Taken and thrown away; the message carries on from here.
	going.Write(msg[16:])

	straight := New(ciph)
	straight.Write(msg)

	if !bytes.Equal(going.Sum(nil), straight.Sum(nil)) {
		t.Fatal("taking the code in the middle changed the message it was over")
	}
}

// TestCMACSumAppends is the contract of the interface: the code goes behind what Sum was given.
func TestCMACSumAppends(t *testing.T) {
	ciph, err := aes.NewCipher(bytes.Repeat([]byte{0x2b}, 16))
	if err != nil {
		t.Fatalf("could not build the cipher: %v", err)
	}

	d := New(ciph)
	d.Write([]byte("sombrero"))

	prefix := []byte("before")
	got := d.Sum(prefix)

	if !bytes.HasPrefix(got, prefix) {
		t.Fatal("what came back does not start with what was handed in")
	}
	if len(got) != len(prefix)+d.Size() {
		t.Fatalf("what came back is %d bytes long, want %d", len(got), len(prefix)+d.Size())
	}
}

// TestCMACReset starts the message over. A signer is reused for the message after this one, and
// what went before must not reach it.
func TestCMACReset(t *testing.T) {
	key := unhex(t, "2b7e151628aed2a6abf7158809cf4f3c")
	msg := unhex(t, "6bc1bee22e409f96e93d7e117393172a")

	ciph, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("could not build the cipher: %v", err)
	}

	d := New(ciph)
	d.Write([]byte("the message before"))
	d.Reset()
	d.Write(msg)

	if got, want := d.Sum(nil), unhex(t, "070a16b46b4d4144f79bdd9dd04a287c"); !bytes.Equal(got, want) {
		t.Fatalf("after the reset the code came out %x, want %x", got, want)
	}
}

// TestCMACTellsMessagesApart is the least a message authentication code has to do. A message one
// bit from another, and a message that is another with a zero byte on the end, both have to come
// out under codes of their own: the padding of the last block is what the second turns on.
func TestCMACTellsMessagesApart(t *testing.T) {
	ciph, err := aes.NewCipher(bytes.Repeat([]byte{0x2b}, 16))
	if err != nil {
		t.Fatalf("could not build the cipher: %v", err)
	}

	code := func(msg []byte) []byte {
		d := New(ciph)
		d.Write(msg)
		return d.Sum(nil)
	}

	msg := bytes.Repeat([]byte{0x5a}, 16)

	flipped := bytes.Clone(msg)
	flipped[3] ^= 1
	if bytes.Equal(code(msg), code(flipped)) {
		t.Error("two messages a bit apart came out under the same code")
	}

	if bytes.Equal(code(msg), code(append(bytes.Clone(msg), 0))) {
		t.Error("a message and the same message with a zero on the end came out under the same code")
	}

	if bytes.Equal(code(nil), code([]byte{0})) {
		t.Error("the empty message and a single zero byte came out under the same code")
	}
}

// TestCMACSizes are what the interface promises about the code it produces.
func TestCMACSizes(t *testing.T) {
	ciph, err := aes.NewCipher(bytes.Repeat([]byte{0x2b}, 16))
	if err != nil {
		t.Fatalf("could not build the cipher: %v", err)
	}

	d := New(ciph)
	if d.Size() != aes.BlockSize {
		t.Errorf("the code is said to be %d bytes long, want %d", d.Size(), aes.BlockSize)
	}
	if d.BlockSize() != aes.BlockSize {
		t.Errorf("the block is said to be %d bytes long, want %d", d.BlockSize(), aes.BlockSize)
	}
	if got := len(d.Sum(nil)); got != d.Size() {
		t.Errorf("the code came out %d bytes long, want the %d it says", got, d.Size())
	}
}

// TestCMACRefusesABlockSizeItCannotWorkIn is the cipher this construction has no subkeys for. It
// gives up rather than carry on with a polynomial that does not belong to the block size.
func TestCMACRefusesABlockSizeItCannotWorkIn(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a cipher of an unusable block size was accepted")
		}
	}()

	New(oddBlock{})
}

// oddBlock is a cipher of a block size the construction has no polynomial for.
type oddBlock struct{}

func (oddBlock) BlockSize() int          { return 12 }
func (oddBlock) Encrypt(dst, src []byte) { copy(dst, src) }
func (oddBlock) Decrypt(dst, src []byte) { copy(dst, src) }
