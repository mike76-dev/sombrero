package kdf

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"testing"
)

// reference is the key derivation of SP 800-108 in counter mode, written out from the definition
// rather than from the code under test: for a single iteration, the pseudorandom function is
// taken over the counter, the label, a byte of zero, the context and the length wanted, in that
// order, and the first L bits of what comes back are the key.
//
// It is written here so that the shape of the input is checked against the specification and not
// against itself. A derivation that agrees with nobody is the kind that works perfectly until it
// meets another implementation, and then cannot sign anything either side will accept.
func reference(ki, label, context []byte, bits uint32) []byte {
	h := hmac.New(sha256.New, ki)

	h.Write(binary.BigEndian.AppendUint32(nil, 1)) // [i]2, counted from one
	h.Write(label)
	h.Write([]byte{0x00}) // The separator between the label and the context
	h.Write(context)
	h.Write(binary.BigEndian.AppendUint32(nil, bits)) // [L]2

	return h.Sum(nil)[:bits/8]
}

var (
	testKey     = bytes.Repeat([]byte{0x5a}, 16)
	testLabel   = []byte("SMBSigningKey\x00")
	testContext = bytes.Repeat([]byte{0x3c}, 64)
)

// TestKdfFollowsTheSpecification measures what the code produces against the construction the
// specification lays down: the counter big-endian and counted from one, the label and the context
// kept apart by a zero byte, and the length written out at the end in bits.
func TestKdfFollowsTheSpecification(t *testing.T) {
	for _, tt := range []struct {
		name    string
		label   []byte
		context []byte
	}{
		{"a signing key", []byte("SMBSigningKey\x00"), testContext},
		{"an application key", []byte("SMBAppKey\x00"), testContext},
		{"a key to the client", []byte("SMBS2CCipherKey\x00"), testContext},
		{"a key from the client", []byte("SMBC2SCipherKey\x00"), testContext},
		{"the older signing label", []byte("SMB2AESCMAC\x00"), []byte("SmbSign\x00")},
		{"nothing for a label", nil, testContext},
		{"nothing for a context", testLabel, nil},
		{"nothing for either", nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Kdf(testKey, tt.label, tt.context)
			want := reference(testKey, tt.label, tt.context, 128)

			if !bytes.Equal(got, want) {
				t.Fatalf("the key came out %x, want the %x the specification calls for", got, want)
			}
		})
	}
}

// TestKdfProducesAKeyOfTheRightLength is the size everything downstream is built around: the
// ciphers are AES-128, and a key of any other length would not go into them.
func TestKdfProducesAKeyOfTheRightLength(t *testing.T) {
	if got := len(Kdf(testKey, testLabel, testContext)); got != 16 {
		t.Fatalf("the key is %d bytes long, want 16", got)
	}
}

// TestKdfIsSettledByWhatGoesIn is the same three inputs asked for twice. Both ends of a session
// derive their keys separately and never compare them, so a derivation that wandered would leave
// the two signing with different keys and each refusing the other.
func TestKdfIsSettledByWhatGoesIn(t *testing.T) {
	first := Kdf(testKey, testLabel, testContext)
	second := Kdf(testKey, testLabel, testContext)

	if !bytes.Equal(first, second) {
		t.Fatal("the same inputs gave two different keys")
	}
}

// TestKdfSeparatesTheLabelFromTheContext is what the zero byte between them is for. Without it
// the two run together, and a label and context that meet at a different place come out with the
// same key: the key meant for signing would then be the key meant for encrypting, which is the
// one thing deriving several keys from one secret is supposed to prevent.
func TestKdfSeparatesTheLabelFromTheContext(t *testing.T) {
	joined := Kdf(testKey, []byte("AB"), []byte("CD"))
	moved := Kdf(testKey, []byte("A"), []byte("BCD"))

	if bytes.Equal(joined, moved) {
		t.Fatal("a label and a context that meet at a different place gave the same key")
	}
}

// TestKdfGivesEachPurposeAKeyOfItsOwn is what the whole derivation is for. One session key stands
// behind all four, and they are told apart by nothing but the label, so any two of them coming
// out alike would mean traffic protected under one could be forged under another.
func TestKdfGivesEachPurposeAKeyOfItsOwn(t *testing.T) {
	labels := [][]byte{
		[]byte("SMBSigningKey\x00"),
		[]byte("SMBAppKey\x00"),
		[]byte("SMBS2CCipherKey\x00"),
		[]byte("SMBC2SCipherKey\x00"),
	}

	seen := make(map[string][]byte)
	for _, label := range labels {
		key := Kdf(testKey, label, testContext)
		if before, found := seen[string(key)]; found {
			t.Fatalf("the key for %q is the key for %q", label, before)
		}
		seen[string(key)] = label
	}
}

// TestKdfFollowsEachOfItsInputs is the derivation moving when anything it was given moves. The
// context of a 3.1.1 session is the hash of everything the two sides said while setting it up,
// which is what ties the keys to that exchange; a context that did not reach the key would leave
// the keys of a tampered exchange the same as the keys of an honest one.
func TestKdfFollowsEachOfItsInputs(t *testing.T) {
	base := Kdf(testKey, testLabel, testContext)

	otherKey := bytes.Clone(testKey)
	otherKey[0] ^= 1
	if bytes.Equal(base, Kdf(otherKey, testLabel, testContext)) {
		t.Error("a different session key gave the same derived key")
	}

	otherLabel := bytes.Clone(testLabel)
	otherLabel[0] ^= 1
	if bytes.Equal(base, Kdf(testKey, otherLabel, testContext)) {
		t.Error("a different label gave the same derived key")
	}

	otherContext := bytes.Clone(testContext)
	otherContext[len(otherContext)-1] ^= 1
	if bytes.Equal(base, Kdf(testKey, testLabel, otherContext)) {
		t.Error("a different context gave the same derived key")
	}
}
