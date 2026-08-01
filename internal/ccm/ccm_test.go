package ccm

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
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

// newCCM builds the mode over AES under the given key.
func newCCM(t *testing.T, key []byte, nonceSize, tagSize int) cipher.AEAD {
	t.Helper()

	ciph, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("could not build the cipher: %v", err)
	}

	aead, err := NewCCMWithNonceAndTagSizes(ciph, nonceSize, tagSize)
	if err != nil {
		t.Fatalf("could not build the mode: %v", err)
	}

	return aead
}

// TestCCMAgainstRFC3610 is a worked example of the mode from the document that first wrote it
// down. Sealing and opening against each other only shows the two halves agree; whether they
// agree with anybody else is settled here, and nothing else in the package settles it.
//
// The example counts its nonce at thirteen bytes and its tag at eight, which is not what SMB
// asks for. It is the formatting of the counter and the authentication blocks that it pins, and
// that formatting is what the mode is; the sizes are carried through it as parameters.
func TestCCMAgainstRFC3610(t *testing.T) {
	key := unhex(t, "c0c1c2c3c4c5c6c7c8c9cacbcccdcecf")
	nonce := unhex(t, "00000003020100a0a1a2a3a4a5")
	data := unhex(t, "0001020304050607")
	plaintext := unhex(t, "08090a0b0c0d0e0f101112131415161718191a1b1c1d1e")
	want := unhex(t, "588c979a61c663d2f066d0c2c0f989806d5f6b61dac38417e8d12cfdf926e0")

	aead := newCCM(t, key, 13, 8)

	got := aead.Seal(nil, nonce, plaintext, data)
	if !bytes.Equal(got, want) {
		t.Fatalf("the mode sealed to %x, want the %x of the specification", got, want)
	}

	back, err := aead.Open(nil, nonce, got, data)
	if err != nil {
		t.Fatalf("the mode would not open what the specification says it should: %v", err)
	}
	if !bytes.Equal(back, plaintext) {
		t.Fatalf("what came back is %x, want %x", back, plaintext)
	}
}

// TestCCMRoundTrip walks the sizes the mode may be built with, including the pair SMB settles on,
// and the message shapes it has to carry.
func TestCCMRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x2b}, 16)

	for _, sizes := range []struct {
		nonce, tag int
	}{
		{11, 16}, // What SMB negotiates.
		{7, 4},
		{13, 16},
		{12, 8},
		{8, 14},
	} {
		for _, msg := range []struct {
			name      string
			plaintext []byte
			data      []byte
		}{
			{"nothing at all", nil, nil},
			{"only associated data", nil, []byte("the header")},
			{"only a payload", []byte("sombrero"), nil},
			{"a payload under a block", []byte("sombrero"), []byte("the header")},
			{"a payload of exactly a block", bytes.Repeat([]byte{0x5a}, 16), []byte("the header")},
			{"a payload over several blocks", bytes.Repeat([]byte("sombrero"), 64), bytes.Repeat([]byte{1}, 40)},
		} {
			t.Run(msg.name, func(t *testing.T) {
				aead := newCCM(t, key, sizes.nonce, sizes.tag)
				nonce := bytes.Repeat([]byte{0x11}, sizes.nonce)

				sealed := aead.Seal(nil, nonce, msg.plaintext, msg.data)
				if got, want := len(sealed), len(msg.plaintext)+sizes.tag; got != want {
					t.Fatalf("what was sealed is %d bytes long, want %d", got, want)
				}

				back, err := aead.Open(nil, nonce, sealed, msg.data)
				if err != nil {
					t.Fatalf("what was sealed would not open: %v", err)
				}
				if !bytes.Equal(back, msg.plaintext) {
					t.Fatal("what came back is not what went in")
				}
			})
		}
	}
}

// TestCCMRefusesWhatWasTampered is the whole point of sealing rather than merely encrypting. A
// message that changed anywhere — in what it carries, in what it is sealed under, or in the tag
// that vouches for it — does not come back open.
func TestCCMRefusesWhatWasTampered(t *testing.T) {
	key := bytes.Repeat([]byte{0x2b}, 16)
	nonce := bytes.Repeat([]byte{0x11}, 11)
	plaintext := []byte("the message that was sent")
	data := []byte("the header it travelled under")

	aead := newCCM(t, key, 11, 16)
	sealed := aead.Seal(nil, nonce, plaintext, data)

	for _, tt := range []struct {
		name   string
		change func() (sealed, nonce, data []byte)
	}{
		{
			name: "a byte of what it carries",
			change: func() ([]byte, []byte, []byte) {
				bad := bytes.Clone(sealed)
				bad[0] ^= 1
				return bad, nonce, data
			},
		},
		{
			name: "a byte of the tag",
			change: func() ([]byte, []byte, []byte) {
				bad := bytes.Clone(sealed)
				bad[len(bad)-1] ^= 1
				return bad, nonce, data
			},
		},
		{
			name: "a byte of the associated data",
			change: func() ([]byte, []byte, []byte) {
				bad := bytes.Clone(data)
				bad[0] ^= 1
				return sealed, nonce, bad
			},
		},
		{
			name: "the nonce it was sealed under",
			change: func() ([]byte, []byte, []byte) {
				bad := bytes.Clone(nonce)
				bad[0] ^= 1
				return sealed, bad, data
			},
		},
		{
			name: "the associated data taken away",
			change: func() ([]byte, []byte, []byte) {
				return sealed, nonce, nil
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, n, d := tt.change()
			if _, err := aead.Open(nil, n, s, d); err == nil {
				t.Fatal("a message that changed on the way was opened all the same")
			}
		})
	}
}

// TestCCMOpenLeavesNothingBehindOnFailure is the message that did not check out. Whatever was
// worked out on the way to finding that out is not handed back: it is unauthenticated plaintext,
// which is exactly what the tag is there to keep from being used.
func TestCCMOpenLeavesNothingBehindOnFailure(t *testing.T) {
	key := bytes.Repeat([]byte{0x2b}, 16)
	nonce := bytes.Repeat([]byte{0x11}, 11)

	aead := newCCM(t, key, 11, 16)
	sealed := aead.Seal(nil, nonce, []byte("the message that was sent"), nil)
	sealed[3] ^= 1

	got, err := aead.Open(nil, nonce, sealed, nil)
	if err == nil {
		t.Fatal("a message that changed on the way was opened all the same")
	}
	if len(got) != 0 {
		t.Fatalf("%d bytes came back out of a message that did not check out", len(got))
	}
}

// TestCCMSealAppends is the contract of the interface: what is sealed goes behind what Seal was
// given rather than in place of it.
func TestCCMSealAppends(t *testing.T) {
	aead := newCCM(t, bytes.Repeat([]byte{0x2b}, 16), 11, 16)
	nonce := bytes.Repeat([]byte{0x11}, 11)

	prefix := []byte("before")
	got := aead.Seal(prefix, nonce, []byte("sombrero"), nil)

	if !bytes.HasPrefix(got, prefix) {
		t.Fatal("what came back does not start with what was handed in")
	}
	if want := len(prefix) + len("sombrero") + aead.Overhead(); len(got) != want {
		t.Fatalf("what came back is %d bytes long, want %d", len(got), want)
	}
}

// TestCCMSizes are what the mode says about itself, which is what the caller lays out its buffers
// and its nonces by.
func TestCCMSizes(t *testing.T) {
	aead := newCCM(t, bytes.Repeat([]byte{0x2b}, 16), 11, 16)

	if aead.NonceSize() != 11 {
		t.Errorf("the nonce is said to be %d bytes long, want 11", aead.NonceSize())
	}
	if aead.Overhead() != 16 {
		t.Errorf("the tag is said to be %d bytes long, want 16", aead.Overhead())
	}
}

// TestCCMRefusesSizesItCannotWorkIn is the mode asked for on terms the construction does not
// have. The counter block has a fixed room in it that the nonce and the length of the payload
// share, so not every size can be carried.
func TestCCMRefusesSizesItCannotWorkIn(t *testing.T) {
	ciph, err := aes.NewCipher(bytes.Repeat([]byte{0x2b}, 16))
	if err != nil {
		t.Fatalf("could not build the cipher: %v", err)
	}

	for _, tt := range []struct {
		name       string
		nonce, tag int
	}{
		{"a nonce too short", 6, 16},
		{"a nonce too long", 14, 16},
		{"a tag too short", 11, 2},
		{"a tag too long", 11, 18},
		{"a tag of an odd length", 11, 15},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewCCMWithNonceAndTagSizes(ciph, tt.nonce, tt.tag); err == nil {
				t.Fatal("the mode was built on terms it cannot work in")
			}
		})
	}
}

// TestCCMRefusesABlockSizeItCannotWorkIn is the cipher whose blocks are not the size the
// formatting of this mode is written around.
func TestCCMRefusesABlockSizeItCannotWorkIn(t *testing.T) {
	if _, err := NewCCMWithNonceAndTagSizes(oddBlock{}, 11, 16); err == nil {
		t.Fatal("the mode was built over a cipher of the wrong block size")
	}
}

// TestCCMSealRefusesAPayloadItCannotCount is the payload longer than the room left to count it
// in. The length shares the counter block with the nonce, so a long nonce leaves little room:
// eleven bytes of nonce leave four, which is more than anything SMB sends, but the limit is
// answered for rather than run past.
func TestCCMSealRefusesAPayloadItCannotCount(t *testing.T) {
	aead := newCCM(t, bytes.Repeat([]byte{0x2b}, 16), 13, 16)
	nonce := bytes.Repeat([]byte{0x11}, 13)

	// Thirteen bytes of nonce leave two to count the payload in, so anything over 65535 bytes
	// cannot be described by the block it would be sealed under.
	if got := aead.Seal(nil, nonce, make([]byte, 1<<16), nil); got != nil {
		t.Fatal("a payload longer than the mode can count was sealed all the same")
	}
}

// oddBlock is a cipher whose blocks are not the size this mode is written around.
type oddBlock struct{}

func (oddBlock) BlockSize() int          { return 8 }
func (oddBlock) Encrypt(dst, src []byte) { copy(dst, src) }
func (oddBlock) Decrypt(dst, src []byte) { copy(dst, src) }
