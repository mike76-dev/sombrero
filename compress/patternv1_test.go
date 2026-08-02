package compress

import (
	"bytes"
	"testing"
)

// run builds a buffer of n copies of b.
func run(b byte, n int) []byte {
	return bytes.Repeat([]byte{b}, n)
}

// TestScanForDataPatternsV1 walks the buffer for the runs at either end, which are what the
// compression sends as a pattern instead of as data. A run has to be at least sixty-four bytes to
// be worth a pattern of its own; anything shorter is reported as nothing rather than as a short
// run, and the caller reads the count to decide whether there is one at all.
func TestScanForDataPatternsV1(t *testing.T) {
	for _, tt := range []struct {
		name           string
		buf            []byte
		forward        uint32
		backward       uint32
		noBackwardRun  bool
		noForwardAtAll bool
	}{
		{
			name:           "nothing at all",
			buf:            nil,
			noForwardAtAll: true,
			noBackwardRun:  true,
		},
		{
			name:          "a run at the start and nothing else",
			buf:           run('A', 100),
			forward:       100,
			noBackwardRun: true, // the whole buffer is the one run, so there is no other end
		},
		{
			name:     "a run at each end",
			buf:      append(run('A', 100), run('B', 100)...),
			forward:  100,
			backward: 100,
		},
		{
			name:     "a run at the start only",
			buf:      append(run('A', 100), []byte("sombrero and some other text")...),
			forward:  100,
			backward: 0,
		},
		{
			name:     "a run at the end only",
			buf:      append([]byte("sombrero and some other text"), run('B', 100)...),
			forward:  0,
			backward: 100,
		},
		{
			name:     "runs too short to be worth sending as patterns",
			buf:      append(append(run('A', 63), []byte("middle")...), run('B', 63)...),
			forward:  0,
			backward: 0,
		},
		{
			name:     "runs of exactly the length that is worth it",
			buf:      append(append(run('A', 64), []byte("middle")...), run('B', 64)...),
			forward:  64,
			backward: 64,
		},
		{
			name:     "nothing repeated anywhere",
			buf:      []byte("abcdefghijklmnop"),
			forward:  0,
			backward: 0,
		},
		{
			name:          "a single byte",
			buf:           []byte{'A'},
			forward:       0,
			backward:      0,
			noBackwardRun: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			forward, backward := ScanForDataPatternsV1(tt.buf)

			if tt.noForwardAtAll {
				if forward != nil {
					t.Errorf("a run was reported at the start of nothing: %+v", forward)
				}
			} else {
				if forward == nil {
					t.Fatal("nothing came back for the start of the buffer")
				}
				if forward.Repetitions != tt.forward {
					t.Errorf("the run at the start came out %d, want %d", forward.Repetitions, tt.forward)
				}
				if tt.forward > 0 && forward.Pattern != tt.buf[0] {
					t.Errorf("the run at the start is of %q, want %q", forward.Pattern, tt.buf[0])
				}
			}

			if tt.noBackwardRun {
				if backward != nil {
					t.Errorf("a run was reported at the end when there is no other end: %+v", backward)
				}
				return
			}

			if backward == nil {
				t.Fatal("nothing came back for the end of the buffer")
			}
			if backward.Repetitions != tt.backward {
				t.Errorf("the run at the end came out %d, want %d", backward.Repetitions, tt.backward)
			}
			if tt.backward > 0 && backward.Pattern != tt.buf[len(tt.buf)-1] {
				t.Errorf("the run at the end is of %q, want %q", backward.Pattern, tt.buf[len(tt.buf)-1])
			}
		})
	}
}

// TestScanForDataPatternsV1DoesNotCountABytetwice is what the caller depends on to keep a message
// the length it says it is. Both runs are taken out of the payload, so a byte counted at both ends
// would leave the payload short and the message would come apart wrong.
func TestScanForDataPatternsV1DoesNotCountAByteTwice(t *testing.T) {
	for _, buf := range [][]byte{
		run('A', 64),
		run('A', 100),
		run('A', 1000),
		append(run('A', 64), run('B', 64)...),
		append(run('A', 512), run('B', 512)...),
		append(append(run('A', 64), 'x'), run('A', 64)...),
	} {
		forward, backward := ScanForDataPatternsV1(buf)

		var taken uint32
		if forward != nil {
			taken += forward.Repetitions
		}
		if backward != nil {
			taken += backward.Repetitions
		}

		if taken > uint32(len(buf)) {
			t.Errorf("a buffer of %d bytes had %d taken out of it as runs", len(buf), taken)
		}
	}
}

// TestScanForDataPatternsV1OnAWholeBufferOfOneByte is the message that is nothing but a run. It is
// reported at the start only: reporting it at both ends would have the caller take it out twice.
func TestScanForDataPatternsV1OnAWholeBufferOfOneByte(t *testing.T) {
	buf := run('A', 4096)

	forward, backward := ScanForDataPatternsV1(buf)
	if forward == nil || forward.Repetitions != uint32(len(buf)) {
		t.Fatalf("the run at the start came out %+v, want all %d bytes", forward, len(buf))
	}
	if backward != nil {
		t.Errorf("the same run was reported at the end as well: %+v", backward)
	}
}

// FuzzScanForDataPatternsV1 walks any buffer at all. The runs it reports are taken out of the
// payload by the caller, so they have to be really there and must not between them account for
// more of the buffer than there is.
func FuzzScanForDataPatternsV1(f *testing.F) {
	f.Add(run('A', 100))
	f.Add(append(run('A', 100), run('B', 100)...))
	f.Add([]byte("sombrero"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, buf []byte) {
		forward, backward := ScanForDataPatternsV1(buf)

		var taken uint32

		if forward != nil && forward.Repetitions > 0 {
			taken += forward.Repetitions
			if int(forward.Repetitions) > len(buf) {
				t.Fatalf("a run of %d was reported in a buffer of %d", forward.Repetitions, len(buf))
			}
			for _, b := range buf[:forward.Repetitions] {
				if b != forward.Pattern {
					t.Fatal("the run reported at the start is not all the one byte")
				}
			}
		}

		if backward != nil && backward.Repetitions > 0 {
			taken += backward.Repetitions
			if int(backward.Repetitions) > len(buf) {
				t.Fatalf("a run of %d was reported in a buffer of %d", backward.Repetitions, len(buf))
			}
			for _, b := range buf[len(buf)-int(backward.Repetitions):] {
				if b != backward.Pattern {
					t.Fatal("the run reported at the end is not all the one byte")
				}
			}
		}

		if taken > uint32(len(buf)) {
			t.Fatalf("a buffer of %d bytes had %d taken out of it as runs", len(buf), taken)
		}
	})
}
