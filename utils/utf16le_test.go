package utils

import (
	"slices"
	"testing"
	"time"
	"unicode/utf8"
)

// TestEncodedStringLenMatchesWhatIsEncoded is the one thing this function is for: it is asked how
// much room a string will take before the room is set aside, and every caller then encodes into
// exactly that. A count that came out short would have the encoding write past the end of a
// buffer sized by it, and one that came out long would leave the message with bytes in it that
// nothing set.
func TestEncodedStringLenMatchesWhatIsEncoded(t *testing.T) {
	for _, tt := range []struct {
		name string
		s    string
	}{
		{"nothing", ""},
		{"plain ASCII", "sombrero"},
		{"a path", "\\\\server\\share\\dir\\file.txt"},
		{"accented letters", "Grüße über Straßen"},
		{"Cyrillic", "имя файла"},
		{"a character outside the basic plane", "a\U0001F600b"},
		{"nothing but characters outside it", "\U0001F600\U0001F601\U0001F602"},
		{"a character right at the boundary", "￿\U00010000"},
		{"the highest character there is", "\U0010FFFF"},
		{"a null in the middle", "before\x00after"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			said := EncodedStringLen(tt.s)

			if got := len(EncodeStringToBytes(tt.s)); got != said {
				t.Errorf("the string was said to take %d bytes and took %d", said, got)
			}

			// The buffer is given exactly the room that was asked for, so an encoding that
			// wanted more would be caught here rather than in a caller.
			dst := make([]byte, said)
			if got := EncodeString(dst, tt.s); got != said {
				t.Errorf("encoding into a buffer of the size asked for wrote %d bytes, want %d", got, said)
			}
		})
	}
}

// TestEncodedStringLenCountsBrokenInputTheWayItIsEncoded is a string that is not valid UTF-8,
// which a Go string is free to be. Ranging over one yields the replacement character for each bad
// byte and the encoder puts the same character out, so the two have to agree about it; this is
// the case where the count is worked out by walking runes and the encoding by another route.
func TestEncodedStringLenCountsBrokenInputTheWayItIsEncoded(t *testing.T) {
	for _, tt := range []struct {
		name string
		s    string
	}{
		{"a lone continuation byte", "a\x80b"},
		{"a truncated sequence", "a\xE2\x82"},
		{"a byte that starts nothing", "a\xFFb"},
		{"nothing but broken bytes", "\xFF\xFE\xFD"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if utf8.ValidString(tt.s) {
				t.Fatal("the string used for this case is valid after all")
			}

			if got, said := len(EncodeStringToBytes(tt.s)), EncodedStringLen(tt.s); got != said {
				t.Errorf("the string was said to take %d bytes and took %d", said, got)
			}
		})
	}
}

// TestEncodeAndDecodeComeBackToTheSameString is the round trip a filename makes: out to the client
// encoded and back in again on the next request naming it.
func TestEncodeAndDecodeComeBackToTheSameString(t *testing.T) {
	for _, s := range []string{
		"",
		"sombrero",
		"dir/file.txt",
		"Grüße",
		"имя файла",
		"a\U0001F600b",
	} {
		if got := DecodeToString(EncodeStringToBytes(s)); got != s {
			t.Errorf("%q came back as %q", s, got)
		}
	}
}

// TestEncodeStringToBytesGivesNothingForNothing is what the callers building a message check
// against: an empty name takes no room at all rather than an empty slice they have to reason
// about separately.
func TestEncodeStringToBytesGivesNothingForNothing(t *testing.T) {
	if got := EncodeStringToBytes(""); got != nil {
		t.Errorf("an empty string encoded to %v, want nothing", got)
	}
}

// TestDecodeToStringDropsOneTrailingNull is the name that arrives terminated. One null goes, and
// only one: a name that ends in a null of its own would otherwise lose it as well.
func TestDecodeToStringDropsOneTrailingNull(t *testing.T) {
	terminated := append(EncodeStringToBytes("file.txt"), 0, 0)
	if got := DecodeToString(terminated); got != "file.txt" {
		t.Errorf("a terminated name decoded to %q, want %q", got, "file.txt")
	}

	twice := append(EncodeStringToBytes("file.txt"), 0, 0, 0, 0)
	if got := DecodeToString(twice); got != "file.txt\x00" {
		t.Errorf("a name ending in a null decoded to %q, want it kept", got)
	}
}

// TestDecodeToStringSurvivesAnOddNumberOfBytes is a length that cannot be a whole number of
// sixteen-bit units. It comes off the wire, where a length field and the bytes behind it need not
// agree, so it has to be answered rather than read past the end of.
func TestDecodeToStringSurvivesAnOddNumberOfBytes(t *testing.T) {
	odd := append(EncodeStringToBytes("file"), 0x41)

	// Only the whole units can be read; the byte left over is not half of anything.
	if got := DecodeToString(odd); got != "file" {
		t.Errorf("an odd-length buffer decoded to %q, want %q", got, "file")
	}
}

// TestDecodeToStringGivesNothingForNothing is the empty buffer, which is what a name of no length
// arrives as.
func TestDecodeToStringGivesNothingForNothing(t *testing.T) {
	if got := DecodeToString(nil); got != "" {
		t.Errorf("an empty buffer decoded to %q", got)
	}
}

// TestNullTerminatedToStringsReadsADialectList is the list as a client actually sends it, each
// entry behind a byte that says a string follows. This is the shape the SMB1 negotiate carries
// and the one the server picks a dialect out of.
func TestNullTerminatedToStringsReadsADialectList(t *testing.T) {
	list := []byte("\x02PC NETWORK PROGRAM 1.0\x00\x02LANMAN1.0\x00\x02NT LM 0.12\x00\x02SMB 2.002\x00\x02SMB 2.???\x00")

	want := []string{"PC NETWORK PROGRAM 1.0", "LANMAN1.0", "NT LM 0.12", "SMB 2.002", "SMB 2.???"}
	if got := readList(t, list); !slices.Equal(got, want) {
		t.Errorf("the list read as %q, want %q", got, want)
	}
}

// TestNullTerminatedToStringsAlwaysComesBack is the reason this function is worth its own test.
// It is reached from the validation of an SMB1 negotiate, which is the first thing a connection
// sends and is read before anybody has said who they are, over bytes taken to the end of the
// packet with nothing checking their shape. Every one of these once went round for ever, holding
// a core at full tilt and never giving the goroutine back — a few short packets and the server
// has no capacity left at all.
//
// The point here is not the answer but that there is one, so each case is given a hard deadline
// rather than being left to the timeout of the whole run.
func TestNullTerminatedToStringsAlwaysComesBack(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   []byte
	}{
		{"nothing at all", nil},
		{"a lone marker byte", []byte{0x02}},
		{"a lone null", []byte{0x00}},
		{"nothing but markers", []byte{0x02, 0x02, 0x02}},
		{"a list cut off before the last terminator", []byte("\x02NT LM 0.12\x00\x02SMB 2.002")},
		{"a string with no terminator anywhere", []byte("SMB 2.002")},
		{"a marker with nothing behind it", []byte("\x02NT LM 0.12\x00\x02")},
		{"a terminator and then a stray byte", []byte("\x02NT LM 0.12\x00A")},
		{"every byte there is, unterminated", allBytes()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("read as %q", readList(t, tt.in))
		})
	}
}

// TestNullTerminatedToStringsKeepsAnUnterminatedTail is what is made of the last string when the
// byte that should end it is not there. What arrived is taken as it stands: the alternative is to
// look at the same bytes again having consumed none of them.
func TestNullTerminatedToStringsKeepsAnUnterminatedTail(t *testing.T) {
	want := []string{"NT LM 0.12", "SMB 2.002"}
	if got := readList(t, []byte("\x02NT LM 0.12\x00\x02SMB 2.002")); !slices.Equal(got, want) {
		t.Errorf("the list read as %q, want %q", got, want)
	}
}

// TestNullTerminatedToStringsFindsNothingInNothing is what the validation leans on. A list with
// no entries in it is turned away there by the count coming back zero, so a buffer that says
// nothing must not come back holding one empty string.
func TestNullTerminatedToStringsFindsNothingInNothing(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   []byte
	}{
		{"nothing at all", nil},
		{"an empty slice", []byte{}},
		{"nothing but markers", []byte{0x02, 0x02}},
		{"nothing but terminators", []byte{0x00, 0x00}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := readList(t, tt.in); len(got) != 0 {
				t.Errorf("a buffer with no entries read as %q", got)
			}
		})
	}
}

// FuzzNullTerminatedToStrings walks the bytes of a dialect list, which arrive from the far end
// before authentication and are handed here without their shape being checked. The property is
// that the walk finishes and accounts for what it was given: it is the finishing that a peer can
// otherwise take away, and go test runs the seeds of this target even without -fuzz.
func FuzzNullTerminatedToStrings(f *testing.F) {
	f.Add([]byte("\x02NT LM 0.12\x00\x02SMB 2.002\x00"))
	f.Add([]byte("\x02NT LM 0.12\x00\x02SMB 2.002"))
	f.Add([]byte{0x02})
	f.Add([]byte{0x00})
	f.Add([]byte(""))
	f.Add([]byte("SMB 2.002"))

	f.Fuzz(func(t *testing.T, b []byte) {
		got := NullTerminatedToStrings(b)

		// Nothing may be conjured up: every string handed back has to be somewhere in what was
		// given, and none of them may carry the byte that separates them.
		for _, s := range got {
			if len(s) > len(b) {
				t.Fatalf("a string of %d bytes came out of %d", len(s), len(b))
			}
			for i := 0; i < len(s); i++ {
				if s[i] == 0 {
					t.Fatalf("%q was handed back with a terminator inside it", s)
				}
			}
		}
	})
}

// readList walks a list under a deadline of its own. Every test here goes through it, because the
// way this function fails is by not coming back at all: called directly, one that regressed would
// hang the whole package until the run was killed and name nothing as the cause.
func readList(t *testing.T, b []byte) []string {
	t.Helper()

	done := make(chan []string, 1)
	go func() { done <- NullTerminatedToStrings(b) }()

	select {
	case got := <-done:
		return got
	case <-time.After(5 * time.Second):
		// The goroutine is still going round and will not stop; there is nothing to wait for.
		t.Fatal("reading the list never came back")
		return nil
	}
}

// allBytes is a buffer holding every byte value, which puts markers, terminators and text next to
// each other in every order the walk has to step through.
func allBytes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}
