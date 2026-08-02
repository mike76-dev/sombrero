package compress

import (
	"bytes"
	"testing"

	"github.com/mike76-dev/sombrero/compress/lznt1"
	"github.com/mike76-dev/sombrero/smb2"
)

// algorithms are the four this server compresses with, each of which decompresses input that
// arrived from the far end.
var algorithms = []struct {
	name string
	id   uint16
}{
	{"LZ77", smb2.COMPRESSION_LZ77},
	{"LZ77 with Huffman coding", smb2.COMPRESSION_LZ77_HUFFMAN},
	{"LZ4", smb2.COMPRESSION_LZ4},
	{"LZNT1", smb2.COMPRESSION_LZNT1},
}

// TestDecompressStaysUnderTheLimit is the bound every one of these has to keep. The input is a
// message from a peer and says nothing trustworthy about how far it expands, so the caller passes
// the size it is expecting and nothing longer than that may be built: a short message that
// expanded without a bound would take the memory of the machine before anybody could look at what
// came out of it.
//
// The input here is a long run of one byte, which every one of these algorithms packs into very
// little. It is then decompressed under a limit far below what it comes to.
func TestDecompressStaysUnderTheLimit(t *testing.T) {
	plain := bytes.Repeat([]byte{0x41}, 1<<20) // a megabyte that packs into almost nothing

	for _, algo := range algorithms {
		t.Run(algo.name, func(t *testing.T) {
			c := New(algo.id)

			packed, err := c.Compress(plain)
			if err != nil {
				t.Fatalf("the run would not pack: %v", err)
			}
			t.Logf("%d bytes packed into %d", len(plain), len(packed))

			const limit = 4096
			got, err := c.Decompress(packed, limit)
			if err == nil && len(got) > limit {
				t.Fatalf("a limit of %d was answered with %d bytes", limit, len(got))
			}
		})
	}
}

// TestDecompressUnderItsOwnLimitComesBackWhole is the control for the bound above. Without it a
// decompressor that refused everything would satisfy that test and pass however broken it was.
func TestDecompressUnderItsOwnLimitComesBackWhole(t *testing.T) {
	plain := bytes.Repeat([]byte("sombrero "), 4096)

	for _, algo := range algorithms {
		t.Run(algo.name, func(t *testing.T) {
			c := New(algo.id)

			packed, err := c.Compress(plain)
			if err != nil {
				t.Fatalf("the message would not pack: %v", err)
			}

			got, err := c.Decompress(packed, len(plain))
			if err != nil {
				t.Fatalf("what was packed would not come apart: %v", err)
			}
			if !bytes.Equal(got, plain) {
				t.Fatalf("what came back is %d bytes, want %d", len(got), len(plain))
			}
		})
	}
}

// TestLZNT1StaysUnderTheLimit goes at the one that had no bound at all, closer than through the
// Compressor above. Its siblings both took the size they were expected to come to and stopped
// there; this one was handed the same number and did not take it, so a stream of chunks each
// standing for far more than it is written in expanded as far as it liked.
func TestLZNT1StaysUnderTheLimit(t *testing.T) {
	plain := bytes.Repeat([]byte{0x41}, 1<<20)
	packed := lznt1.Compress(plain)

	for _, limit := range []int{1, 64, 4096, 1 << 16} {
		got, err := lznt1.Decompress(packed, limit)
		if err == nil && len(got) > limit {
			t.Errorf("a limit of %d was answered with %d bytes", limit, len(got))
		}
	}

	// Its own size is not a bound it trips over.
	got, err := lznt1.Decompress(packed, len(plain))
	if err != nil {
		t.Fatalf("the stream would not come apart under its own size: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("what came back is %d bytes, want %d", len(got), len(plain))
	}
}

// TestDecompressRefusesRubbish walks the shapes a compressed payload arrives in when it is not
// one. Each of these is read before anybody has authenticated, so an answer has to come back.
func TestDecompressRefusesRubbish(t *testing.T) {
	ones := bytes.Repeat([]byte{0xff}, 512)

	for _, algo := range algorithms {
		t.Run(algo.name, func(t *testing.T) {
			c := New(algo.id)

			for _, tt := range []struct {
				name string
				src  []byte
			}{
				{"nothing at all", nil},
				{"a single byte", []byte{0}},
				{"a handful of zeroes", make([]byte, 16)},
				{"nothing but ones", ones},
				{"text", []byte("this was never compressed")},
			} {
				t.Run(tt.name, func(t *testing.T) {
					got, err := c.Decompress(tt.src, 4096)
					if err == nil && len(got) > 4096 {
						t.Fatalf("%d bytes came out under a limit of 4096", len(got))
					}
				})
			}
		})
	}
}

// TestCompressAndDecompressAnUnknownAlgorithm is the identifier this server does not compress
// with. Nothing is done and nothing is complained about, which is what the caller reads as the
// message travelling as it stands.
func TestCompressAndDecompressAnUnknownAlgorithm(t *testing.T) {
	c := New(0xbeef)

	packed, err := c.Compress([]byte("sombrero"))
	if err != nil {
		t.Errorf("packing under an unknown algorithm was answered with %v", err)
	}
	if packed != nil {
		t.Errorf("packing under an unknown algorithm produced %d bytes", len(packed))
	}

	got, err := c.Decompress([]byte("sombrero"), 4096)
	if err != nil {
		t.Errorf("unpacking under an unknown algorithm was answered with %v", err)
	}
	if got != nil {
		t.Errorf("unpacking under an unknown algorithm produced %d bytes", len(got))
	}
}

// TestCompressAndDecompressNothing is the empty payload, which the framing above may hand down
// when a message has nothing left to compress.
func TestCompressAndDecompressNothing(t *testing.T) {
	for _, algo := range algorithms {
		t.Run(algo.name, func(t *testing.T) {
			c := New(algo.id)

			packed, err := c.Compress(nil)
			if err != nil {
				t.Fatalf("packing nothing was answered with %v", err)
			}

			got, err := c.Decompress(packed, 0)
			if err != nil {
				t.Fatalf("unpacking nothing was answered with %v", err)
			}
			if len(got) != 0 {
				t.Errorf("unpacking nothing produced %d bytes", len(got))
			}
		})
	}
}

// TestLZ4NeedsToBeToldTheSize is the one algorithm whose payload says nothing about how far it
// expands. The size comes from the transform header instead, and without it there is nothing to
// unpack into.
func TestLZ4NeedsToBeToldTheSize(t *testing.T) {
	c := New(smb2.COMPRESSION_LZ4)

	packed, err := c.Compress(bytes.Repeat([]byte("sombrero "), 64))
	if err != nil {
		t.Fatalf("the message would not pack: %v", err)
	}

	if _, err := c.Decompress(packed, 0); err == nil {
		t.Error("a payload was unpacked without being told what it comes to")
	}
	if _, err := c.Decompress(packed, -1); err == nil {
		t.Error("a payload was unpacked under a size below zero")
	}
}

// FuzzDecompress walks the bytes of a compressed payload under each algorithm. It arrives from a
// peer that has only negotiated, so the property is that an answer comes back and that it is
// never longer than the size the caller said to expect — the caller lays out what follows on the
// strength of that number.
func FuzzDecompress(f *testing.F) {
	f.Add(uint16(smb2.COMPRESSION_LZ77), []byte{}, 4096)
	for _, algo := range algorithms {
		packed, err := New(algo.id).Compress(bytes.Repeat([]byte("sombrero "), 64))
		if err != nil {
			f.Fatalf("could not build a seed: %v", err)
		}
		f.Add(algo.id, packed, 4096)
		f.Add(algo.id, packed, 0)
	}

	f.Fuzz(func(t *testing.T, algo uint16, src []byte, limit int) {
		// A limit is what a caller has room for, so only sane ones are worth walking; the
		// enormous ones only ask whether this machine can allocate them.
		if limit < 0 || limit > 1<<20 {
			t.Skip()
		}

		got, err := New(algo).Decompress(src, limit)
		if err != nil {
			return
		}

		if limit > 0 && len(got) > limit {
			t.Fatalf("a limit of %d was answered with %d bytes", limit, len(got))
		}
	})
}
