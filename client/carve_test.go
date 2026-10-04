package client

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/rand"
	"testing"
)

// The files the tests carve are built the way real ones are, since the carver
// walks their structure rather than looking for a header and a trailer.

// jpegFile is a JPEG with the given amount of entropy-coded data, with the byte
// stuffing and a restart marker that real scan data has.
func jpegFile(payload int) []byte {
	var b bytes.Buffer
	segment := func(marker byte, body []byte) {
		b.Write([]byte{0xff, marker})
		_ = binary.Write(&b, binary.BigEndian, uint16(len(body)+2))
		b.Write(body)
	}

	b.Write([]byte{0xff, 0xd8})
	segment(0xe0, append([]byte("JFIF\x00\x01\x01"), make([]byte, 7)...))
	segment(0xdb, make([]byte, 65))
	segment(0xc0, make([]byte, 15))
	segment(0xc4, make([]byte, 20))
	segment(0xda, make([]byte, 10))

	// Scan data: a 0xff inside it is stuffed with 0x00, and a restart marker is
	// not the end.
	data := bytes.Repeat([]byte{0x5a, 0xff, 0x00}, payload/3)
	data = append(data, 0xff, 0xd0)
	b.Write(data)

	b.Write([]byte{0xff, 0xd9})

	return b.Bytes()
}

// pngFile is a PNG of one IDAT chunk of the given size, every chunk with its
// checksum.
func pngFile(payload int) []byte {
	var b bytes.Buffer
	chunk := func(kind string, body []byte) {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(body)))
		b.WriteString(kind)
		b.Write(body)
		_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(kind), body...)))
	}

	b.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	chunk("IHDR", make([]byte, 13))
	chunk("IDAT", bytes.Repeat([]byte{'i'}, payload))
	chunk("IEND", nil)

	return b.Bytes()
}

// gifFile is a GIF with a global color table, one extension and one image.
func gifFile() []byte {
	var b bytes.Buffer
	b.WriteString("GIF89a")
	b.Write([]byte{0x10, 0x00, 0x10, 0x00, 0x80 | 0x01, 0x00, 0x00}) // 16x16, a 4-entry table
	b.Write(make([]byte, 3*4))
	b.Write([]byte{0x21, 0xf9, 0x04, 0, 0, 0, 0, 0x00})                   // a graphic control extension
	b.Write([]byte{0x2c, 0, 0, 0, 0, 0x10, 0x00, 0x10, 0x00, 0x00, 0x02}) // the image descriptor and LZW size
	b.Write([]byte{0x05, 1, 2, 3, 4, 5, 0x03, 6, 7, 8, 0x00})             // two sub-blocks and the end of them
	b.WriteByte(0x3b)

	return b.Bytes()
}

// zipFile is a real archive of the given entries.
func zipFile(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()

	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, content := range entries {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip.Create(%s): %v", name, err)
		}
		if _, err := f.Write(content); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip.Close: %v", err)
	}

	return b.Bytes()
}

// pdfFile is a PDF whose cross-reference table is where startxref says, holding
// the given bytes in a stream, with as many revisions appended as asked for.
func pdfFile(stream []byte, revisions int) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	b.WriteString("1 0 obj\n<< /Type /Catalog >>\nendobj\n")
	fmt.Fprintf(&b, "2 0 obj\n<< /Length %d >>\nstream\n", len(stream))
	b.Write(stream)
	b.WriteString("\nendstream\nendobj\n")

	xref := b.Len()
	b.WriteString("xref\n0 3\n0000000000 65535 f \n0000000009 00000 n \n0000000050 00000 n \n")
	fmt.Fprintf(&b, "trailer\n<< /Size 3 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)

	for i := 0; i < revisions; i++ {
		b.WriteString("3 0 obj\n<< /Type /Annot >>\nendobj\n")
		xref := b.Len()
		b.WriteString("xref\n3 1\n0000000000 00000 n \n")
		fmt.Fprintf(&b, "trailer\n<< /Size 4 >>\nstartxref\n%d\n%%%%EOF\n", xref)
	}

	return b.Bytes()
}

// atom is one ISO base media atom: its size, its type and its contents.
func atom(kind string, body []byte) []byte {
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], kind)

	return append(b, body...)
}

// isoFile is an MP4, MOV or HEIC of the given brand, with this much data and its
// index either side of it: a camera writes the index last, a web encoder first.
func isoFile(brand string, payload int, indexFirst bool) []byte {
	ftyp := atom("ftyp", append([]byte(brand+"\x00\x00\x02\x00isom"), brand...))
	index := atom("moov", atom("mvhd", make([]byte, 100)))
	if brand == "heic" {
		index = atom("meta", atom("hdlr", make([]byte, 32)))
	}
	mdat := atom("mdat", bytes.Repeat([]byte{0x7f}, payload))

	b := append([]byte{}, ftyp...)
	if indexFirst {
		return append(append(b, index...), mdat...)
	}

	return append(append(b, mdat...), index...)
}

// riffFile is a RIFF file of the given form, made of the given chunks.
func riffFile(form string, chunks ...[]byte) []byte {
	var body []byte
	for _, c := range chunks {
		body = append(body, c...)
	}
	b := make([]byte, 12, 12+len(body))
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(4+len(body)))
	copy(b[8:], form)

	return append(b, body...)
}

// chunk is one RIFF chunk, padded to an even length as they are.
func chunk(id string, body []byte) []byte {
	b := make([]byte, 8, 8+len(body)+1)
	copy(b, id)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(body)))
	b = append(b, body...)
	if len(body)%2 == 1 {
		b = append(b, 0)
	}

	return b
}

// wavFile is a WAV of this many bytes of samples.
func wavFile(samples int) []byte {
	return riffFile("WAVE", chunk("fmt ", make([]byte, 16)), chunk("data", bytes.Repeat([]byte{1}, samples)))
}

// aviFile is an AVI: its header list, its frames, and its index.
func aviFile() []byte {
	return riffFile("AVI ",
		chunk("LIST", append([]byte("hdrl"), make([]byte, 56)...)),
		chunk("LIST", append([]byte("movi"), bytes.Repeat([]byte{2}, 3001)...)),
		chunk("idx1", make([]byte, 16)),
	)
}

// TestCarveFindsWholeFiles verifies what is taken out of an object that holds
// several files end to end, which is what a packed slab is.
func TestCarveFindsWholeFiles(t *testing.T) {
	files := []struct {
		ext  string
		data []byte
	}{
		{"jpg", jpegFile(3000)},
		{"png", pngFile(2000)},
		{"gif", gifFile()},
		{"zip", zipFile(t, map[string][]byte{"notes.txt": []byte("hello there")})},
		{"pdf", pdfFile([]byte("plain text in a stream"), 0)},
		{"mp4", isoFile("isom", 5000, false)},
		{"mov", isoFile("qt  ", 2000, true)},
		{"heic", isoFile("heic", 1500, false)},
		{"wav", wavFile(1001)},
		{"avi", aviFile()},
	}

	var object bytes.Buffer
	object.Write(bytes.Repeat([]byte{0}, 16)) // bytes of nobody's file
	for _, f := range files {
		object.Write(f.data)
	}

	finds, leftover := carve(object.Bytes())
	if len(finds) != len(files) {
		t.Fatalf("want the %d files that were packed, got %d: %+v", len(files), len(finds), finds)
	}

	at := uint64(16)
	for i, f := range files {
		if finds[i].Ext != f.ext || finds[i].Offset != at || finds[i].Length != uint64(len(f.data)) {
			t.Errorf("find %d: got %+v, want %s of %d at %d", i, finds[i], f.ext, len(f.data), at)
		}
		got := object.Bytes()[finds[i].Offset:finds[i].End()]
		if !bytes.Equal(got, f.data) {
			t.Errorf("the %s does not read back as itself", f.ext)
		}
		at += uint64(len(f.data))
	}

	if leftover != 16 {
		t.Errorf("leftover bytes: want the 16 of nobody's file, got %d", leftover)
	}
}

// TestCarveSkipsEmbeddedPictures verifies that a file holding pictures is taken
// whole, rather than cut off at the first picture inside it.
func TestCarveSkipsEmbeddedPictures(t *testing.T) {
	picture := jpegFile(600)

	pdf := pdfFile(picture, 0)
	finds, _ := carve(pdf)
	if len(finds) != 1 || finds[0].Ext != "pdf" || finds[0].Length != uint64(len(pdf)) {
		t.Errorf("a PDF with a picture in it: want the whole PDF, got %+v", finds)
	}

	archive := zipFile(t, map[string][]byte{"photo.jpg": picture, "logo.png": pngFile(300)})
	finds, _ = carve(archive)
	if len(finds) != 1 || finds[0].Ext != "zip" || finds[0].Length != uint64(len(archive)) {
		t.Errorf("an archive with pictures in it: want the whole archive, got %+v", finds)
	}
}

// TestCarveTakesEveryRevision verifies that a PDF revised in place, which has one
// %%EOF per revision, ends at the last of its own and not at the first — and not
// at the next PDF's either.
func TestCarveTakesEveryRevision(t *testing.T) {
	revised := pdfFile([]byte("first"), 2)
	next := pdfFile([]byte("second"), 0)

	finds, leftover := carve(append(append([]byte{}, revised...), next...))
	if len(finds) != 2 {
		t.Fatalf("want the two PDFs, got %+v", finds)
	}
	if finds[0].Length != uint64(len(revised)) {
		t.Errorf("the revised PDF is %d bytes, want all %d", finds[0].Length, len(revised))
	}
	if finds[1].Offset != uint64(len(revised)) || finds[1].Length != uint64(len(next)) {
		t.Errorf("the next PDF: got %+v", finds[1])
	}
	if leftover != 0 {
		t.Errorf("leftover bytes: want none, got %d", leftover)
	}
}

// TestCarveIgnoresChanceHeaders verifies that the bytes of a video, which hold
// what looks like the start and the end of a JPEG every so often, are not taken
// for thousands of JPEGs.
func TestCarveIgnoresChanceHeaders(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	video := make([]byte, 4<<20)
	r.Read(video)

	// A start of image every so often, and ends of image everywhere, with the
	// odd atom and chunk name thrown in.
	for i := 1000; i < len(video)-4; i += 65536 {
		copy(video[i:], []byte{0xff, 0xd8, 0xff, 0xe0})
	}
	for i := 3000; i < len(video)-2; i += 4096 {
		copy(video[i:], []byte{0xff, 0xd9})
	}
	for i := 5000; i < len(video)-4; i += 100000 {
		copy(video[i:], "ftyp")
		copy(video[i+50000:], "RIFF")
	}

	finds, leftover := carve(video)
	if len(finds) != 0 {
		t.Errorf("%d file(s) were found in noise: %+v", len(finds), finds[:min(3, len(finds))])
	}
	if leftover != uint64(len(video)) {
		t.Errorf("leftover bytes: want all %d, got %d", len(video), leftover)
	}
}

// TestCarveLeavesBrokenFilesAlone verifies that a file whose structure does not
// reach its end is not handed back as whole: a piece of one is not a file.
func TestCarveLeavesBrokenFilesAlone(t *testing.T) {
	jpeg := jpegFile(3000)
	cut := jpeg[:len(jpeg)-100]
	if finds, _ := carve(cut); len(finds) != 0 {
		t.Errorf("a JPEG cut before its end was taken: %+v", finds)
	}

	png := pngFile(2000)
	png[len(png)-1] ^= 0xff // the IEND checksum
	if finds, _ := carve(png); len(finds) != 0 {
		t.Errorf("a PNG with a wrong checksum was taken: %+v", finds)
	}

	archive := zipFile(t, map[string][]byte{"a": []byte("b")})
	if finds, _ := carve(archive[:len(archive)-10]); len(finds) != 0 {
		t.Errorf("an archive without its directory record was taken: %+v", finds)
	}

	pdf := pdfFile([]byte("x"), 0)
	if finds, _ := carve(pdf[:len(pdf)-20]); len(finds) != 0 {
		t.Errorf("a PDF cut before its %%%%EOF was taken: %+v", finds)
	}

	// A video is whole only with both its data and its index, whichever came
	// last.
	camera := isoFile("isom", 4000, false)
	if finds, _ := carve(camera[:len(camera)-50]); len(finds) != 0 {
		t.Errorf("an MP4 cut before its index was taken: %+v", finds)
	}
	web := isoFile("isom", 4000, true)
	if finds, _ := carve(web[:len(web)-50]); len(finds) != 0 {
		t.Errorf("an MP4 cut inside its data was taken: %+v", finds)
	}

	wav := wavFile(1000)
	if finds, _ := carve(wav[:len(wav)-1]); len(finds) != 0 {
		t.Errorf("a WAV a byte short of what it says was taken: %+v", finds)
	}
	wav[4]++ // a RIFF that claims more than there is
	if finds, _ := carve(wav); len(finds) != 0 {
		t.Errorf("a RIFF claiming more than there is was taken: %+v", finds)
	}
}

// TestCarveEndsAnAtomFileWithoutATrailer verifies that an MP4, which has nothing
// to mark its end, ends where its atoms do and not a byte into whatever follows,
// even where that happens to read like an atom.
func TestCarveEndsAnAtomFileWithoutATrailer(t *testing.T) {
	video := isoFile("isom", 3000, false)

	for _, trailing := range [][]byte{
		bytes.Repeat([]byte{0}, 100),
		[]byte("some other file's bytes"),
		append([]byte{0xff, 0xff, 0xff, 0xff}, "free"...), // an atom too large to be here
		isoFile("isom", 500, false),                       // the next video
	} {
		finds, _ := carve(append(append([]byte{}, video...), trailing...))
		if len(finds) == 0 || finds[0].Ext != "mp4" || finds[0].Length != uint64(len(video)) {
			t.Errorf("followed by %q: got %+v, want the video of %d bytes", trailing[:min(8, len(trailing))], finds, len(video))
		}
	}
}

// TestCarveFindsNothingInNothing verifies that an object of no known files is
// reported as such rather than guessed at.
func TestCarveFindsNothingInNothing(t *testing.T) {
	for _, data := range [][]byte{
		nil,
		bytes.Repeat([]byte{0}, 1024),
		[]byte("just some text, which is a file nobody can recognize"),
		[]byte("%PDF-1.4\n%%EOF"), // a header and a trailer with no file between them
		{0xff, 0xd8, 0xff, 0xd9},  // a start and an end of image with no picture between them
		{0xff, 0xd8, 0xff, 0xd0, 0xff, 0xff, 0xd9},
		{0xff, 0xd8, 0xff, 0xc0, 0x00, 0x04, 0, 0, 0xff, 0xd9}, // a frame header, but no scan
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
