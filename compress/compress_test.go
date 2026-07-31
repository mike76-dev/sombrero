package compress

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// Every algorithm the server is willing to negotiate has to survive its own round trip through the
// dispatcher, which is where the algorithm number is turned into a codec. Getting that mapping
// wrong is not something the codecs themselves can catch.
func TestEveryAlgorithmRoundTrips(t *testing.T) {
	inputs := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"one byte", []byte{0x2a}},
		{"repetitive", []byte(strings.Repeat("abc", 100))},
		{"incompressible", incompressible(1 << 16)},
		{"larger than a block", []byte(strings.Repeat("the quick brown fox ", 8000))},
	}

	for _, algo := range []struct {
		name string
		id   uint16
	}{
		{"LZNT1", smb2.COMPRESSION_LZNT1},
		{"LZ77", smb2.COMPRESSION_LZ77},
		{"LZ77+Huffman", smb2.COMPRESSION_LZ77_HUFFMAN},
		{"LZ4", smb2.COMPRESSION_LZ4},
	} {
		t.Run(algo.name, func(t *testing.T) {
			c := New(algo.id)

			for _, in := range inputs {
				t.Run(in.name, func(t *testing.T) {
					packed, err := c.Compress(in.data)
					if err != nil {
						t.Fatalf("compressing %d bytes: %v", len(in.data), err)
					}

					// The uncompressed length is what the SMB2 compression transform header
					// carries, and is what the receiver has to work from.
					got, err := c.Decompress(packed, len(in.data))
					if err != nil {
						t.Fatalf("decompressing %d bytes back from %d: %v", len(in.data), len(packed), err)
					}

					if !bytes.Equal(got, in.data) {
						t.Errorf("%d bytes went in and %d came out, and they differ", len(in.data), len(got))
					}
				})
			}
		})
	}
}

// LZ4 travels as a bare block. The frame format leads with a magic number, and the whole point of
// the change away from it is that the magic number must not be there: an SMB2 peer reads the
// payload as a block, so a frame header would be data as far as it is concerned.
func TestLZ4IsABlockAndNotAFrame(t *testing.T) {
	packed, err := New(smb2.COMPRESSION_LZ4).Compress([]byte(strings.Repeat("abc", 100)))
	if err != nil {
		t.Fatalf("compressing: %v", err)
	}

	if magic := []byte{0x04, 0x22, 0x4d, 0x18}; bytes.HasPrefix(packed, magic) {
		t.Errorf("the output starts with the LZ4 frame magic % x, want a bare block", magic)
	}
}

// LZ77+Huffman opens with 256 bytes of code lengths, whatever else it holds. Nothing shorter than
// that can be a conformant stream, which is what made the previous implementation - DEFLATE under
// the same algorithm number - recognisable at a glance.
func TestHuffmanCarriesItsTable(t *testing.T) {
	packed, err := New(smb2.COMPRESSION_LZ77_HUFFMAN).Compress([]byte("abcdefghijklmnopqrstuvwxyz"))
	if err != nil {
		t.Fatalf("compressing: %v", err)
	}

	if len(packed) <= 256 {
		t.Fatalf("the output is %d bytes, which cannot hold the 256-byte table", len(packed))
	}
}

func incompressible(n int) []byte {
	b := make([]byte, n)
	x := uint32(0x12345678)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}

	return b
}
