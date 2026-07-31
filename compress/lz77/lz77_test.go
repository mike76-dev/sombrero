package lz77

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mike76-dev/sombrero/compress/internal/comptest"
)

func codec() comptest.Codec {
	return comptest.Codec{
		Compress:   Compress,
		Decompress: Decompress,
	}
}

func TestRoundTrip(t *testing.T) {
	comptest.Run(t, codec())
}

// The streams [MS-XCA] prints for Plain LZ77, in its LZ77 example. They are the only inputs in this
// package that it did not produce itself, and so the only ones that say anything about what a
// Windows client would make of it.
func TestConformance(t *testing.T) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"

	// 26 literals. The letters are all different, so there is no match of three to be found
	// anywhere in it. The flag word is 26 zero bits, one for each literal, and then six ones,
	// because the flag bits past the end of the data are set rather than clear.
	literals := append([]byte{0x3f, 0x00, 0x00, 0x00}, alphabet...)

	comptest.Conformance(t, codec(), []comptest.Vector{
		{
			Name:   "literals",
			Packed: literals,
			Plain:  []byte(alphabet),
		},
		{
			// "abc" and then a single match of length 297 at a distance of 3. The length runs far
			// past the distance, so the match overlaps what it is still producing and has to be
			// copied a byte at a time rather than block-wise.
			Name:   "overlapping match",
			Packed: []byte{0xff, 0xff, 0xff, 0x1f, 0x61, 0x62, 0x63, 0x17, 0x00, 0x0f, 0xff, 0x26, 0x01},
			Plain:  []byte(strings.Repeat("abc", 100)),
		},
	})
}

// The alphabet has only one valid encoding, so what this compressor writes for it can be held to
// what the specification prints, byte for byte. Nothing else here can be: an encoder chooses among
// several correct streams, and the conformance test asks only that this one never chooses a longer
// stream than Microsoft's. Here there is nothing to choose - no three bytes repeat, so every byte
// is a literal, and the specification fixes the flag bits past the end of the data as ones - which
// makes this the one place the two encoders can be required to agree exactly.
func TestEncodesTheAlphabetAsTheSpecificationDoes(t *testing.T) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	want := append([]byte{0x3f, 0x00, 0x00, 0x00}, alphabet...)

	if got := Compress([]byte(alphabet)); !bytes.Equal(got, want) {
		t.Errorf("compressed to\n\t% x\nwant\n\t% x", got, want)
	}
}
