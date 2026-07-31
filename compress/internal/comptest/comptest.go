// Package comptest holds what the tests of the compressors have in common: the inputs each of them
// is held to, and the round trip that is the only thing any of them promises.
//
// The compressors are a package apiece with one shape between them, so the alternative is the same
// test written twice and left to drift.
package comptest

import (
	"bytes"
	"flag"
	"fmt"
	"math/rand/v2"
	"testing"
)

var (
	// size is how much data the randomized round trip runs through.
	//
	// It used to be drawn at random, anywhere from one byte to 2 GiB, which is why the compressors
	// were the one part of this module that could never be checked for races: the detector
	// instruments every byte touched, and a buffer of that size compressed, decompressed and
	// compared took the machine into swap rather than finishing. The default here covers many
	// chunks and windows over and never leaves cache.
	//
	// The old scale is still reachable, deliberately rather than by accident:
	//
	//	go test ./compress/... -compress.size=2147483648
	size = flag.Int("compress.size", 1<<20, "bytes of data the randomized round trip compresses")

	// seed shapes that data. A round trip that fails on one input in a million has to be repeatable
	// to be worth anything, so the seed is logged whether it was given or drawn, and a failing run
	// can be replayed with the one it printed:
	//
	//	go test ./compress/... -compress.seed=8965380576935260373
	seed = flag.Uint64("compress.seed", 0, "seed for the randomized round trip; 0 draws a fresh one")
)

// Codec is a compressor and the decompressor that undoes it.
//
// Decompress is given the length of the original input because LZ77 needs it to size its output.
// LZNT1 reads the length out of the stream and ignores what it is passed; holding both to one
// signature is what lets them be held to one test.
type Codec struct {
	Compress   func(src []byte) []byte
	Decompress func(src []byte, limit int) ([]byte, error)
}

// Run holds a codec to everything in this package: the fixed corpus first, so that a failure names
// the shape it failed on, and then a randomized round trip at whatever size was asked for.
func Run(t *testing.T, c Codec) {
	t.Helper()

	for _, in := range corpus() {
		t.Run(in.name, func(t *testing.T) {
			roundTrip(t, c, in.data)
		})
	}

	t.Run("random", func(t *testing.T) {
		s := *seed
		if s == 0 {
			s = rand.Uint64()
		}

		src := runs(newRNG(s), *size)
		packed := c.Compress(src)
		t.Logf("seed %d: %d bytes in, %d out, %.0f%% smaller", s, len(src), len(packed),
			float64(len(src)-len(packed))/float64(len(src))*100)

		roundTrip(t, c, src)
	})
}

// roundTrip checks that what comes back out is what went in. That is the whole of what a
// compressor promises: how small the output is, and what it looks like, are its own business.
func roundTrip(t *testing.T, c Codec, src []byte) {
	t.Helper()

	packed := c.Compress(src)

	got, err := c.Decompress(packed, len(src))
	if err != nil {
		t.Fatalf("%d bytes compressed to %d would not decompress: %v", len(src), len(packed), err)
	}

	if !bytes.Equal(got, src) {
		t.Fatalf("what came back is not what went in: %s", difference(src, got))
	}
}

// difference says where two buffers part company. Neither is printed: they run into megabytes, and
// the offset of the first byte that differs is what says where to look.
func difference(want, got []byte) string {
	for i := range min(len(want), len(got)) {
		if want[i] != got[i] {
			return fmt.Sprintf("%d bytes back for %d, first differing at offset %d: %#02x, want %#02x",
				len(got), len(want), i, got[i], want[i])
		}
	}

	if len(want) != len(got) {
		return fmt.Sprintf("%d bytes back for %d, alike as far as they both go", len(got), len(want))
	}

	return "no difference found"
}

type input struct {
	name string
	data []byte
}

// corpus returns the fixed inputs every codec is held to.
//
// They are the shapes the two formats turn on. LZNT1 works in chunks of 4096 bytes and decides for
// each whether to store it packed or raw; LZ77 matches within a window of 8192. So either side of
// both is worth having, along with the two ends of the compressible range, which are what that
// packed-or-raw decision comes down to.
//
// Every one of them is fixed rather than drawn, so that a failure means the same thing on every
// run and on every machine.
func corpus() []input {
	r := newRNG(0x5b1ec0de)

	var inputs []input
	add := func(name string, data []byte) {
		inputs = append(inputs, input{name: name, data: data})
	}

	// A compressor is at its most breakable where there is nearly nothing to work on.
	add("empty", nil)
	add("one byte", []byte{0x2a})
	add("two bytes", []byte{0x00, 0xff})
	add("shorter than a match", []byte{0x01, 0x02})

	for _, n := range []int{4095, 4096, 4097, 8191, 8192, 8193, 1 << 16} {
		add(fmt.Sprintf("zeros/%d", n), make([]byte, n))
		add(fmt.Sprintf("one value/%d", n), bytes.Repeat([]byte{0xff}, n))
		add(fmt.Sprintf("pattern/%d", n), pattern(n))
		add(fmt.Sprintf("incompressible/%d", n), random(r, n))
		add(fmt.Sprintf("runs/%d", n), runs(r, n))
	}

	return inputs
}

// newRNG returns a generator that gives the same numbers for the same seed. Nothing here wants
// unpredictability - it wants a lot of bytes and the ability to get the same ones back.
func newRNG(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

// pattern returns a short cycle repeated, which is about as compressible as data gets while still
// holding more than one value.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 7)
	}

	return b
}

// random returns bytes with nothing in them to find, which is what drives a format that can fall
// back to storing a chunk raw into doing so.
func random(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	fill(r, b)

	return b
}

// runs returns runs of random bytes alternating with runs of one repeated byte, so that a single
// input holds both what compresses and what does not, and the boundaries between them fall in
// arbitrary places rather than on the chunk lines.
//
// It is the shape the old test used, which is worth keeping: the interesting part of a chunked
// format is what it does where the data changes character partway through a chunk.
func runs(r *rand.Rand, n int) []byte {
	const maxRun = 10000

	b := make([]byte, n)
	for i := 0; i < n; {
		i += fillRun(r, b[i:min(i+maxRun, n)], func(part []byte) { fill(r, part) })
		i += fillRun(r, b[i:min(i+maxRun, n)], func(part []byte) {
			for j := range part {
				part[j] = 0xff
			}
		})
	}

	return b
}

// fillRun writes over a random-length prefix of the space it is given, and reports how much it
// took. A run of nothing would leave the caller going round forever, so it is always at least one
// byte where there is one to be had.
func fillRun(r *rand.Rand, space []byte, write func([]byte)) int {
	if len(space) == 0 {
		return 0
	}

	n := r.IntN(len(space)) + 1
	write(space[:n])

	return n
}

// fill writes random bytes, eight at a time: math/rand/v2 has no Read, and going a byte at a time
// is what made this the slowest part of the test.
func fill(r *rand.Rand, b []byte) {
	for len(b) >= 8 {
		v := r.Uint64()
		for i := range 8 {
			b[i] = byte(v >> (8 * i))
		}
		b = b[8:]
	}

	if len(b) > 0 {
		v := r.Uint64()
		for i := range b {
			b[i] = byte(v >> (8 * i))
		}
	}
}
