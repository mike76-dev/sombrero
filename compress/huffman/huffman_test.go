package huffman

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

// The streams [MS-XCA] prints for LZ77+Huffman. Each is a 256-byte table of code lengths followed
// by the bit stream, and between them they cover both the plain case and the awkward one: a match
// too long to state in its own symbol, whose length spills out of the bit stream into bytes beside
// it.
func TestConformance(t *testing.T) {
	comptest.Conformance(t, codec(), []comptest.Vector{
		{
			Name:   "literals",
			Packed: literalsVector(),
			Plain:  []byte("abcdefghijklmnopqrstuvwxyz"),
		},
		{
			Name:   "long match",
			Packed: longMatchVector(),
			Plain:  []byte(strings.Repeat("abc", 100)),
		},
	})
}

// The alphabet, as a run of literals. Twenty-two of the letters take five bits and the last four
// take four, and the end-of-file symbol takes four.
func literalsVector() []byte {
	v := make([]byte, tableSize)

	// Symbols 97 ('a') to 118 ('v') are five bits, 119 ('w') to 122 ('z') are four.
	for sym := 97; sym <= 122; sym++ {
		length := byte(5)
		if sym >= 119 {
			length = 4
		}
		if sym%2 == 0 {
			v[sym/2] |= length
		} else {
			v[sym/2] |= length << 4
		}
	}
	v[eofSymbol/2] |= 4 // the end-of-file symbol, four bits

	return append(v, 0xd8, 0x52, 0x3e, 0xd7, 0x94, 0x11, 0x5b, 0xe9, 0x19, 0x5f,
		0xf9, 0xd6, 0x7c, 0xdf, 0x8d, 0x04, 0x00, 0x00, 0x00, 0x00)
}

// "abc" and then a match of length 297 at a distance of 3. The length is far past what the symbol
// can hold, so it follows as the bytes ff 26 01: 0xff says it needs more room, and 0x0126 is 294,
// which is 297 less the three that every length has taken off it.
func longMatchVector() []byte {
	v := make([]byte, tableSize)

	v[97/2] |= 3 << 4   // 'a', three bits
	v[98/2] |= 3        // 'b', three bits
	v[99/2] |= 2 << 4   // 'c', two bits
	v[eofSymbol/2] |= 2 // end of file, two bits

	// Symbol 287 is 256 + 15 + 16, a match whose length did not fit and whose distance has its top
	// bit at position 1, so the distance is 2 or 3 and one further bit says which.
	v[287/2] |= 2 << 4

	return append(v, 0xa8, 0xdc, 0x00, 0x00, 0xff, 0x26, 0x01)
}

// The table is built rather than transcribed, so it is worth checking it came out as the
// specification prints it before anything is decoded from it.
func TestVectorTablesMatchTheSpecification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		table []byte
		want  map[int]byte // offset within the table to its value; the rest are zero
	}{
		{"literals", literalsVector(), map[int]byte{
			48: 0x50, 49: 0x55, 50: 0x55, 51: 0x55, 52: 0x55, 53: 0x55, 54: 0x55,
			55: 0x55, 56: 0x55, 57: 0x55, 58: 0x55, 59: 0x45, 60: 0x44, 61: 0x04,
			128: 0x04,
		}},
		{"long match", longMatchVector(), map[int]byte{
			48: 0x30, 49: 0x23, 128: 0x02, 143: 0x20,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, got := range tc.table[:tableSize] {
				if want := tc.want[i]; got != want {
					t.Errorf("table byte %d is %#02x, want %#02x", i, got, want)
				}
			}
		})
	}
}

// The length of a compressed block is not written down anywhere: a decompressor works it out from
// how many bits it took. Both vectors say what the answer has to be, so both are checked - getting
// this wrong is invisible on one block and takes the next block's table with it on two.
func TestBlockLengthIsWorkedOutExactly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		packed []byte
		plain  string
	}{
		{"literals", literalsVector(), "abcdefghijklmnopqrstuvwxyz"},
		{"long match", longMatchVector(), strings.Repeat("abc", 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lengths [symbolCount]uint8
			for i, b := range tc.packed[:tableSize] {
				lengths[2*i] = b & 0x0f
				lengths[2*i+1] = b >> 4
			}

			_, used, err := decompressBlock(newDecodeTable(lengths[:]), tc.packed[tableSize:], nil, len(tc.plain), true)
			if err != nil {
				t.Fatalf("the block would not decode: %v", err)
			}

			if want := len(tc.packed) - tableSize; used != want {
				t.Errorf("the block was measured at %d bytes, want %d", used, want)
			}
		})
	}
}

// More than 64 KiB is more than one block, which is the only part of the format that neither vector
// reaches: the specification prints no stream long enough. What is checked here is that this
// package agrees with itself across a boundary - that the second table is looked for exactly where
// the first block ended - which is necessary for interoperating but, on its own, not sufficient.
func TestMultipleBlocks(t *testing.T) {
	for _, size := range []int{blockSize - 1, blockSize, blockSize + 1, 3*blockSize + 77} {
		src := make([]byte, size)
		for i := range src {
			// Compressible enough to make matches, varied enough not to become one long run.
			src[i] = byte(i%251) ^ byte(i>>11)
		}

		packed := Compress(src)
		got, err := Decompress(packed, len(src))
		if err != nil {
			t.Fatalf("%d bytes would not decompress: %v", size, err)
		}

		if !bytes.Equal(got, src) {
			t.Errorf("%d bytes did not survive the round trip", size)
		}
	}
}

// TestNoMatchIsWrittenAsTheEndOfTheStream is the one match this encoding cannot say. A match carries
// its length above the shortest and its distance as the bits below the highest, both added to the
// end-of-file symbol — so the shortest match at the nearest distance adds nothing to either and is
// the end-of-file symbol itself. Four bytes of the same value is enough to reach it, and a peer
// reading the stream stops one byte in: everything after that is a file it refuses.
func TestNoMatchIsWrittenAsTheEndOfTheStream(t *testing.T) {
	// Every run length around the shortest match, alone and with data on either side of it, at the
	// front of a block and inside one.
	for n := 1; n <= 20; n++ {
		run := bytes.Repeat([]byte{0x42}, n)

		cases := map[string][]byte{
			"alone":            run,
			"after a literal":  append([]byte{0x01}, run...),
			"between literals": append(append([]byte("head"), run...), []byte("tail")...),
			"twice over":       append(append([]byte{}, run...), run...),
		}

		for name, data := range cases {
			packed := Compress(data)
			got, err := Decompress(packed, len(data))
			if err != nil {
				t.Fatalf("a run of %d %s: %d bytes would not decompress: %v", n, name, len(data), err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("a run of %d %s came back as %x, want %x", n, name, got, data)
			}
		}
	}

	// And the symbol itself is never written for anything but the end: a match of three at a
	// distance of one is the case, so the items of that input carry no such match.
	items, _ := parse(bytes.Repeat([]byte{0x42}, 4), true)
	for _, it := range items {
		if it.length == minMatch && it.distance == 1 {
			t.Error("the shortest match at the nearest distance was emitted, which is the end-of-file symbol")
		}
	}
}
