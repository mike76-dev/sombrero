package lz77

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// The round trip in lz77_test.go holds this compressor to what this decompressor makes of it, and
// the decompressor here is a forgiving one: it stops when the input runs out, whatever the flag
// word still says is coming. A peer is not forgiving. It fills a buffer of the size the transform
// header gave it and treats everything else as a stream it cannot read, so a stream that only this
// package can read is a stream no client can, and the round trip would never say so.
//
// strictDecompress is that peer. It is written from the decompression pseudocode of [MS-XCA] 2.4.1
// and stops on the output rather than on the input: it produces the number of bytes it was told to
// expect and nothing else, and reports how much of the stream it took to do it.
func strictDecompress(src []byte, expected int) ([]byte, int, error) {
	out := make([]byte, 0, expected)
	in := 0
	var flags uint32
	flagCount := 0

	// The position of the byte whose low nibble was taken by the last extended length, and whose
	// high nibble the next one takes. [MS-XCA] keeps it as an index for exactly this reason: the
	// byte lies behind whatever has been written since.
	nibblePos := -1

	for len(out) < expected {
		if flagCount == 0 {
			if in+4 > len(src) {
				return out, in, fmt.Errorf("no flag word left at %d bytes of %d", len(out), expected)
			}
			flags = binary.LittleEndian.Uint32(src[in:])
			in += 4
			flagCount = 32
		}
		flagCount--

		if (flags>>uint(flagCount))&1 == 0 {
			if in >= len(src) {
				return out, in, fmt.Errorf("no literal left at %d bytes", len(out))
			}
			out = append(out, src[in])
			in++
			continue
		}

		if in+2 > len(src) {
			return out, in, fmt.Errorf("no match token left at %d bytes", len(out))
		}
		tok := binary.LittleEndian.Uint16(src[in:])
		in += 2
		length := int(tok & 7)
		offset := int(tok>>3) + 1
		if offset > len(out) {
			return out, in, fmt.Errorf("a match at %d bytes reaches %d back, past the start", len(out), offset)
		}

		if length == 7 {
			if nibblePos < 0 {
				if in >= len(src) {
					return out, in, fmt.Errorf("no nibble left at %d bytes", len(out))
				}
				nibblePos = in
				length = int(src[in] & 0x0f)
				in++
			} else {
				length = int(src[nibblePos] >> 4)
				nibblePos = -1
			}

			if length == 15 {
				if in >= len(src) {
					return out, in, fmt.Errorf("no length byte left at %d bytes", len(out))
				}
				length = int(src[in])
				in++

				if length == 255 {
					if in+2 > len(src) {
						return out, in, fmt.Errorf("no 16-bit length left at %d bytes", len(out))
					}
					l := int(binary.LittleEndian.Uint16(src[in:]))
					in += 2
					if l == 0 {
						if in+4 > len(src) {
							return out, in, fmt.Errorf("no 32-bit length left at %d bytes", len(out))
						}
						l = int(binary.LittleEndian.Uint32(src[in:]))
						in += 4
					}

					// The escaped form carries the whole length, and the shorter forms already
					// cover everything under 22 of it.
					if l < 15+7 {
						return out, in, fmt.Errorf("an escaped length of %d, under the 22 the escape is for", l)
					}
					length = l - (15 + 7)
				}
				length += 15
			}
			length += 7
		}
		length += 3

		if len(out)+length > expected {
			return out, in, fmt.Errorf("a match of %d at %d bytes runs past the %d expected", length, len(out), expected)
		}
		for range length {
			out = append(out, out[len(out)-offset])
		}
	}

	return out, in, nil
}

// strictly compresses the input and reads it back as a peer would.
func strictly(t *testing.T, name string, plain []byte) {
	t.Helper()

	packed := Compress(plain)
	got, taken, err := strictDecompress(packed, len(plain))
	if err != nil {
		t.Errorf("%s (%d bytes): %v", name, len(plain), err)
		return
	}
	if !bytes.Equal(got, plain) {
		at := 0
		for at < len(got) && at < len(plain) && got[at] == plain[at] {
			at++
		}
		t.Errorf("%s (%d bytes): came back different from %d bytes in", name, len(plain), at)
		return
	}

	// The stream has to end where the output does. Bytes left over are bytes the peer would go on
	// to read as another token once its own buffer was full, or on a later message.
	if taken != len(packed) {
		t.Errorf("%s (%d bytes): %d bytes of the stream left over", name, len(plain), len(packed)-taken)
	}
}

func TestAPeerCanReadWhatIsWritten(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))

	// Runs of one byte, at the lengths where the encoding of a match changes form: the three-bit
	// length in the token, then the nibble, then the byte, then the sixteen- and thirty-two-bit
	// forms behind the escape.
	for _, n := range []int{1, 2, 3, 4, 9, 10, 11, 12, 24, 25, 26, 27, 278, 279, 280, 281,
		65535, 65536, 65537, 65538, 65539, 65540, 65541, 131078, 131079, 200000} {
		strictly(t, fmt.Sprintf("a run of %d", n), bytes.Repeat([]byte{0x00}, n))
	}

	// The same runs with something on either side, so that the match is not the first thing in the
	// stream and the run is not the last.
	for _, n := range []int{65535, 65539, 65540, 131079} {
		plain := append([]byte("sombrero"), bytes.Repeat([]byte{0xaa}, n)...)
		strictly(t, fmt.Sprintf("a run of %d in the middle", n), append(plain, []byte("sombrero")...))
	}

	// Repeats of a few periods, which is what fills the window with matches of every length.
	for _, unit := range []string{"ab", "abc", "abcd", "0123456789"} {
		for _, n := range []int{100, 5000, 70000, 200000} {
			strictly(t, fmt.Sprintf("%q over %d bytes", unit, n),
				[]byte(strings.Repeat(unit, n/len(unit))))
		}
	}

	// Random bytes, which is almost all literals, and random bytes out of a small alphabet, which
	// is short matches everywhere. The extended lengths of the latter are what share nibbles, and
	// a nibble taken by the wrong match is a length the peer reads differently.
	for _, n := range []int{1, 31, 32, 33, 64, 1000, 65536, 1 << 20} {
		plain := make([]byte, n)
		rnd.Read(plain)
		strictly(t, fmt.Sprintf("%d random bytes", n), plain)

		for _, alpha := range []int{2, 4, 16} {
			for i := range plain {
				plain[i] = byte(rnd.Intn(alpha))
			}
			strictly(t, fmt.Sprintf("%d bytes out of %d values", n, alpha), plain)
		}
	}

	// A file as it comes off a share: blocks of data with runs of nothing between them.
	for _, n := range []int{4096, 65536, 1 << 20} {
		plain := make([]byte, 0, n)
		for len(plain) < n {
			block := make([]byte, rnd.Intn(4096))
			rnd.Read(block)
			plain = append(plain, block...)
			plain = append(plain, bytes.Repeat([]byte{0}, rnd.Intn(8192))...)
		}
		strictly(t, fmt.Sprintf("%d bytes of a sparse file", n), plain[:n])
	}
}
