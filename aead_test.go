package main

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// Which cipher a connection encrypts with is settled at the negotiate and read back off the
// connection, and until now nothing checked that the right one was reached for. A round trip cannot:
// it seals and opens with the same object, so a cipher swapped for another on both sides comes apart
// again and the test passes. What catches it is a known answer - bytes this server had no hand in
// producing.
//
// The vectors below were produced with OpenSSL, through the AESCCM and AESGCM of the Python
// cryptography package, and each was checked by decrypting the bytes written down here back to the
// plaintext. They are at the sizes SMB works at rather than the ones a specification document
// happens to tabulate: an 11-byte nonce and a 16-byte tag under CCM ([MS-SMB2] 3.1.4.3), and a
// 12-byte nonce under GCM. The RFC 3610 vectors in internal/ccm cover the mode itself at the sizes
// that document uses; these cover the parameters this server picks, and which of the two modes it
// picks them for.

// aeadVector is one worked example of a cipher: a key, a nonce, what was sealed and what came out.
type aeadVector struct {
	name   string
	cipher uint16
	nonce  string // hex
	want   string // hex, the sealed bytes with the tag behind them
}

// The key, plaintext and additional data are the same for both, so that the vectors differ in
// nothing but the cipher and the nonce it takes.
const (
	aeadKey       = "000102030405060708090a0b0c0d0e0f"
	aeadPlaintext = "sombrero known answer vector"
	aeadData      = "202122232425262728292a2b2c2d2e2f"
)

var aeadVectors = []aeadVector{
	{
		name:   "AES-128-CCM",
		cipher: smb2.AES_128_CCM,
		nonce:  "101112131415161718191a",
		want:   "3f0010b9b2d823c4ae67870c550dbecf4f1baf3c3dfd0c8a8934e9713624928314b38c964469486459276b74",
	},
	{
		name:   "AES-128-GCM",
		cipher: smb2.AES_128_GCM,
		nonce:  "101112131415161718191a1b",
		want:   "b7416ecd7d2ac48037b6339ab049cb5f54cf03e244d41ddae6bf466385bbc3a79ab0228c6b595f422dc376ab",
	},
}

// unhex is a vector written down as hex, as bytes.
func unhex(t *testing.T, s string) []byte {
	t.Helper()

	buf, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("the vector %q is not hex: %v", s, err)
	}

	return buf
}

// TestAEADAgainstKnownAnswers seals a worked example under each cipher and holds the bytes against
// what another implementation produced. A cipher reached for in place of the one the connection
// settled on fails here, which no round trip would notice.
func TestAEADAgainstKnownAnswers(t *testing.T) {
	for _, v := range aeadVectors {
		t.Run(v.name, func(t *testing.T) {
			c := &connection{cipherID: v.cipher}
			sealer, err := (&session{}).aead(unhex(t, aeadKey), c)
			if err != nil {
				t.Fatalf("no cipher for %#x: %v", v.cipher, err)
			}

			nonce := unhex(t, v.nonce)
			if sealer.NonceSize() != len(nonce) {
				t.Fatalf("the cipher takes a nonce of %d bytes, want the %d SMB gives it", sealer.NonceSize(), len(nonce))
			}

			got := sealer.Seal(nil, nonce, []byte(aeadPlaintext), unhex(t, aeadData))
			if want := unhex(t, v.want); !bytes.Equal(got, want) {
				t.Errorf("sealed %x,\n                    want %x", got, want)
			}
		})
	}
}

// TestAEADOpensAKnownAnswer is the other direction: bytes another implementation sealed, opened by
// this one. Sealing alone would leave a cipher that is wrong in the same way both ways undetected.
func TestAEADOpensAKnownAnswer(t *testing.T) {
	for _, v := range aeadVectors {
		t.Run(v.name, func(t *testing.T) {
			c := &connection{cipherID: v.cipher}
			opener, err := (&session{}).aead(unhex(t, aeadKey), c)
			if err != nil {
				t.Fatalf("no cipher for %#x: %v", v.cipher, err)
			}

			got, err := opener.Open(nil, unhex(t, v.nonce), unhex(t, v.want), unhex(t, aeadData))
			if err != nil {
				t.Fatalf("the sealed vector did not come apart: %v", err)
			}
			if string(got) != aeadPlaintext {
				t.Errorf("opened %q, want %q", got, aeadPlaintext)
			}
		})
	}
}

// TestAEADRefusesTheOtherCiphersVector is what makes the vectors worth having: each is refused under
// the cipher it does not belong to. Two vectors that opened under either cipher would pin nothing.
func TestAEADRefusesTheOtherCiphersVector(t *testing.T) {
	for _, v := range aeadVectors {
		for _, other := range aeadVectors {
			if other.cipher == v.cipher {
				continue
			}

			t.Run(v.name+" under "+other.name, func(t *testing.T) {
				c := &connection{cipherID: other.cipher}
				opener, err := (&session{}).aead(unhex(t, aeadKey), c)
				if err != nil {
					t.Fatalf("no cipher for %#x: %v", other.cipher, err)
				}

				// The nonce of the vector is the length its own cipher takes, which need not be the
				// length this one does; it is cut or padded so that the cipher is what is on trial
				// here rather than the length of the nonce.
				nonce := make([]byte, opener.NonceSize())
				copy(nonce, unhex(t, v.nonce))

				if _, err := opener.Open(nil, nonce, unhex(t, v.want), unhex(t, aeadData)); err == nil {
					t.Error("the vector of one cipher came apart under the other")
				}
			})
		}
	}
}

// TestAEADWithoutACipher is the connection that settled on none. There is nothing to seal with, and
// asking for a cipher says so rather than falling back on one.
func TestAEADWithoutACipher(t *testing.T) {
	for _, dialect := range []uint16{smb2.SMB_DIALECT_202, smb2.SMB_DIALECT_21, smb2.SMB_DIALECT_302, smb2.SMB_DIALECT_311} {
		c := &connection{negotiateDialect: dialect}
		if _, err := (&session{}).aead(unhex(t, aeadKey), c); err != errNoCipher {
			t.Errorf("dialect %#x with no cipher settled answered %v, want no cipher", dialect, err)
		}
	}
}
