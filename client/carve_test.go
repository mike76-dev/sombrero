package client

import (
	"bytes"
	"testing"
)

// pdf is a PDF of the given size, as the carver recognizes one.
func pdf(size int) []byte {
	body := bytes.Repeat([]byte("p"), size-len("%PDF-")-len("%%EOF"))

	return append(append([]byte("%PDF-"), body...), []byte("%%EOF")...)
}

// png is a PNG of the given size, closing with the IEND chunk and its checksum.
func png(size int) []byte {
	head := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	tail := append([]byte("IEND"), 0xae, 0x42, 0x60, 0x82)
	body := bytes.Repeat([]byte{'i'}, size-len(head)-len(tail))

	return append(append(head, body...), tail...)
}

// jpg is a JPEG of the given size.
func jpg(size int) []byte {
	head := []byte{0xff, 0xd8, 0xff}
	body := bytes.Repeat([]byte{'j'}, size-len(head)-2)

	return append(append(head, body...), 0xff, 0xd9)
}

// TestCarveFindsWholeFiles verifies what is taken out of an object that holds
// several small files end to end, which is what a packed slab is.
func TestCarveFindsWholeFiles(t *testing.T) {
	one, two, three := pdf(200), png(300), jpg(400)

	var object bytes.Buffer
	object.Write(bytes.Repeat([]byte{0}, 16)) // bytes of nobody's file
	object.Write(one)
	object.Write(two)
	object.Write(three)

	finds, leftover := carve(object.Bytes())
	if len(finds) != 3 {
		t.Fatalf("want the 3 files that were packed, got %d: %+v", len(finds), finds)
	}

	for i, want := range []struct {
		ext    string
		offset uint64
		length uint64
	}{
		{"pdf", 16, uint64(len(one))},
		{"png", 16 + uint64(len(one)), uint64(len(two))},
		{"jpg", 16 + uint64(len(one)+len(two)), uint64(len(three))},
	} {
		if finds[i].Ext != want.ext || finds[i].Offset != want.offset || finds[i].Length != want.length {
			t.Errorf("find %d: got %+v, want %s of %d at %d", i, finds[i], want.ext, want.length, want.offset)
		}
	}

	// The bytes of no file are reported rather than passed off as part of one.
	if leftover != 16 {
		t.Errorf("leftover bytes: want the 16 of nobody's file, got %d", leftover)
	}

	// What was found reads back as what went in.
	data := object.Bytes()
	if got := data[finds[0].Offset:finds[0].End()]; !bytes.Equal(got, one) {
		t.Error("the first file does not read back as itself")
	}
}

// TestCarveLeavesPiecesAlone verifies that a piece of a file larger than the
// object is not handed back as a file: a header with no ending is a fifth of
// something, not a fifth of a file.
func TestCarveLeavesPiecesAlone(t *testing.T) {
	// A PDF that was cut off at the end of the object it began in.
	head := append([]byte("%PDF-"), bytes.Repeat([]byte("p"), 4096)...)

	finds, leftover := carve(head)
	if len(finds) != 0 {
		t.Errorf("a file that does not end in here was taken: %+v", finds)
	}
	if leftover != uint64(len(head)) {
		t.Errorf("leftover bytes: want all %d of them, got %d", len(head), leftover)
	}

	// The tail of one, which begins with nothing recognizable at all.
	tail := append(bytes.Repeat([]byte("p"), 4096), []byte("%%EOF")...)
	if finds, _ := carve(tail); len(finds) != 0 {
		t.Errorf("the tail of a file was taken for one: %+v", finds)
	}
}

// TestCarveStopsAtTheNextFile verifies that a file with no ending does not
// swallow the one after it.
func TestCarveStopsAtTheNextFile(t *testing.T) {
	cut := append([]byte("%PDF-"), bytes.Repeat([]byte("p"), 500)...) // no %%EOF
	whole := png(300)

	finds, _ := carve(append(cut, whole...))
	if len(finds) != 1 {
		t.Fatalf("want the one whole file, got %+v", finds)
	}
	if finds[0].Ext != "png" || finds[0].Offset != uint64(len(cut)) {
		t.Errorf("the find: got %+v, want the png at %d", finds[0], len(cut))
	}
}

// TestCarveTakesTheLastEnding verifies that a file holding what looks like its
// own ending is taken whole, up to the last of them.
func TestCarveTakesTheLastEnding(t *testing.T) {
	// A PDF whose body quotes its own trailer, as one holding an attachment may.
	inner := append([]byte("%PDF-"), bytes.Repeat([]byte("p"), 100)...)
	inner = append(inner, []byte("%%EOF")...)
	whole := append(inner, bytes.Repeat([]byte("q"), 100)...)
	whole = append(whole, []byte("%%EOF")...)

	finds, leftover := carve(whole)
	if len(finds) != 1 {
		t.Fatalf("want one file, got %+v", finds)
	}
	if finds[0].Length != uint64(len(whole)) {
		t.Errorf("the find is %d of the %d bytes, want all of them", finds[0].Length, len(whole))
	}
	if leftover != 0 {
		t.Errorf("leftover bytes: want none, got %d", leftover)
	}
}

// TestCarveFindsNothingInNothing verifies that an object of no known files is
// reported as such rather than guessed at.
func TestCarveFindsNothingInNothing(t *testing.T) {
	for _, data := range [][]byte{
		nil,
		bytes.Repeat([]byte{0}, 1024),
		[]byte("just some text, which is a file nobody can recognize"),

		// A header followed at once by its trailer is too small to be a file.
		append([]byte("%PDF-"), []byte("%%EOF")...),
	} {
		finds, leftover := carve(data)
		if len(finds) != 0 {
			t.Errorf("something was taken out of %d bytes of nothing: %+v", len(data), finds)
		}
		if leftover != uint64(len(data)) {
			t.Errorf("leftover bytes: want all %d, got %d", len(data), leftover)
		}
	}
}
