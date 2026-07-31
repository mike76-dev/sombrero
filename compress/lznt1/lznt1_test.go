package lznt1

import (
	"testing"

	"github.com/mike76-dev/sombrero/compress/internal/comptest"
)

func codec() comptest.Codec {
	return comptest.Codec{
		Compress: Compress,

		// LZNT1 carries the length of each chunk in its header, so it has no use for the one the
		// tests hand it.
		Decompress: func(src []byte, _ int) ([]byte, error) { return Decompress(src) },
	}
}

func TestRoundTrip(t *testing.T) {
	comptest.Run(t, codec())
}

// The stream [MS-XCA] prints for LZNT1. It is worth having for one thing in particular: the example
// is built around a compressed word whose length reaches past what has been produced so far, which
// is the case a decompressor gets wrong if it copies the match block-wise instead of byte by byte.
func TestConformance(t *testing.T) {
	// The ANSI string of the example, terminal NUL included. The specification gives its length, so
	// that is checked here rather than trusted: a slip in copying it out would otherwise quietly
	// change what this test is testing.
	const plain = "F# F# G A A G F# E D D E F# F# E E F# F# G A A G F# E D D E F# E D D E E F# D E F# G F# D E F# G F# E D E A F# F# G A A G F# E D D E F# E D D\x00"
	if len(plain) != 142 {
		t.Fatalf("the string from the specification came out %d bytes, want 142", len(plain))
	}

	packed := []byte{
		0x38, 0xb0, 0x88, 0x46, 0x23, 0x20, 0x00, 0x20,
		0x47, 0x20, 0x41, 0x00, 0x10, 0xa2, 0x47, 0x01,
		0xa0, 0x45, 0x20, 0x44, 0x00, 0x08, 0x45, 0x01,
		0x50, 0x79, 0x00, 0xc0, 0x45, 0x20, 0x05, 0x24,
		0x13, 0x88, 0x05, 0xb4, 0x02, 0x4a, 0x44, 0xef,
		0x03, 0x58, 0x02, 0x8c, 0x09, 0x16, 0x01, 0x48,
		0x45, 0x00, 0xbe, 0x00, 0x9e, 0x00, 0x04, 0x01,
		0x18, 0x90, 0x00,
	}
	if len(packed) != 59 {
		t.Fatalf("the stream from the specification came out %d bytes, want 59", len(packed))
	}

	comptest.Conformance(t, codec(), []comptest.Vector{
		{Name: "single chunk", Packed: packed, Plain: []byte(plain)},
	})
}
