package client

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"regexp"
	"strconv"
)

// Carving is what is left for the data whose names are gone: an object that says
// nothing about itself still holds the bytes of whatever was packed into it, and
// the files in it can be found by their structure.
//
// A find is as long as its own structure says and not a byte more, which is what
// keeps a header that turns up by chance inside a video from becoming a file, and
// a PDF with pictures in it from ending at the first picture. A kind of file whose
// structure cannot be walked to its end is not carved at all.

// A format knows how to recognize a file by its first bytes and, more to the
// point, how to walk its structure to where it ends.
type format struct {
	ext   string
	magic []byte

	// length walks the file that begins at the start of the data and returns how
	// long it is, or false where the data is not a whole file of this kind.
	length func(data []byte) (int, bool)
}

// formats are the kinds of file the carver knows. Each one both opens with
// something recognizable and can be walked to its end, which is what makes a find
// a whole file rather than a guess at one.
var formats = []format{
	{ext: "jpg", magic: []byte{0xff, 0xd8, 0xff}, length: jpegLength},
	{ext: "png", magic: []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, length: pngLength},
	{ext: "gif", magic: []byte("GIF89a"), length: gifLength},
	{ext: "gif", magic: []byte("GIF87a"), length: gifLength},
	{ext: "zip", magic: []byte{'P', 'K', 0x03, 0x04}, length: zipLength},
	{ext: "pdf", magic: []byte("%PDF-"), length: pdfLength},
}

// A Find is a file found inside an object: where it begins and ends in it, and
// what kind of file it turned out to be.
type Find struct {
	Offset uint64
	Length uint64
	Ext    string
}

// End returns where the find ends in the object.
func (f Find) End() uint64 {
	return f.Offset + f.Length
}

// carve returns the files to be found in the data, in the order they lie in it,
// and how many bytes belong to none of them.
func carve(data []byte) (finds []Find, leftover uint64) {
	var at int
	for at < len(data) {
		f, start := nextMagic(data, at)
		if f == nil {
			break
		}

		length, ok := f.length(data[start:])
		if !ok {
			// Something that opens like a file but does not go on like one is
			// whatever it is, and the search goes on past its first byte.
			at = start + 1
			continue
		}

		finds = append(finds, Find{Offset: uint64(start), Length: uint64(length), Ext: f.ext})
		at = start + length
	}

	var taken uint64
	for _, find := range finds {
		taken += find.Length
	}

	return finds, uint64(len(data)) - taken
}

// nextMagic returns the format whose magic appears first at or after from, and
// where it appears. The format is nil where none does.
func nextMagic(data []byte, from int) (*format, int) {
	if from >= len(data) {
		return nil, -1
	}

	var found *format
	at := -1
	for i := range formats {
		offset := bytes.Index(data[from:], formats[i].magic)
		if offset < 0 {
			continue
		}
		if at < 0 || from+offset < at {
			found = &formats[i]
			at = from + offset
		}
	}

	return found, at
}

// jpegLength walks the marker segments of a JPEG to its end-of-image marker. The
// entropy-coded data after a start-of-scan is skipped by its byte stuffing: a 0xff
// inside it is followed by 0x00 or by a restart marker, and anything else is the
// next segment. A start and an end of image with no frame and no scan between
// them is not a picture, however well-formed; such pairs turn up by chance.
func jpegLength(data []byte) (int, bool) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 0, false
	}

	i := 2
	var frame, scan bool
	for {
		if i+1 >= len(data) || data[i] != 0xff {
			return 0, false
		}
		marker := data[i+1]

		switch {
		case marker == 0xff:
			// Padding before a marker.
			i++

		case marker == 0xd8, marker == 0x01, marker >= 0xd0 && marker <= 0xd7:
			// A second start of image is not this file's; the others have no
			// length and no payload.
			if marker == 0xd8 {
				return 0, false
			}
			i += 2

		case marker == 0xd9:
			return i + 2, scan

		case marker == 0xda:
			// The scan header, and then the entropy-coded data up to the next
			// marker that is not stuffing or a restart. A scan needs a frame.
			if !frame {
				return 0, false
			}
			scan = true
			i += 2
			segment, ok := segmentLength(data, i)
			if !ok {
				return 0, false
			}
			i += segment

			for {
				j := bytes.IndexByte(data[i:], 0xff)
				if j < 0 || i+j+1 >= len(data) {
					return 0, false
				}
				i += j
				next := data[i+1]
				if next == 0x00 || (next >= 0xd0 && next <= 0xd7) {
					i += 2
					continue
				}
				break
			}

		default:
			// A marker between SOI and SOS is one of the headers, and anything
			// else here is not a JPEG at all.
			if !(marker >= 0xc0 && marker <= 0xfe) {
				return 0, false
			}
			if isFrameHeader(marker) {
				frame = true
			}
			i += 2
			segment, ok := segmentLength(data, i)
			if !ok {
				return 0, false
			}
			i += segment
		}
	}
}

// isFrameHeader reports whether the marker starts a frame: one of the SOF markers,
// which share their range with the Huffman and arithmetic table definitions.
func isFrameHeader(marker byte) bool {
	return marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc
}

// segmentLength reads the length of a JPEG segment, which counts itself.
func segmentLength(data []byte, at int) (int, bool) {
	if at+2 > len(data) {
		return 0, false
	}
	length := int(binary.BigEndian.Uint16(data[at:]))
	if length < 2 || at+length > len(data) {
		return 0, false
	}

	return length, true
}

// pngLength walks the chunks of a PNG to its IEND, checking every one of them
// against its own checksum: a PNG says where it ends and proves it on the way.
func pngLength(data []byte) (int, bool) {
	const signature = 8
	i := signature
	first := true
	for {
		if i+8 > len(data) {
			return 0, false
		}
		length := int(binary.BigEndian.Uint32(data[i:]))
		kind := data[i+4 : i+8]
		if length < 0 || i+12+length > len(data) {
			return 0, false
		}
		if first && (string(kind) != "IHDR" || length != 13) {
			return 0, false
		}
		first = false

		sum := crc32.ChecksumIEEE(data[i+4 : i+8+length])
		if sum != binary.BigEndian.Uint32(data[i+8+length:]) {
			return 0, false
		}

		i += 12 + length
		if string(kind) == "IEND" {
			return i, true
		}
	}
}

// gifLength walks the blocks of a GIF to its trailer.
func gifLength(data []byte) (int, bool) {
	const header = 6 + 7 // the signature and the logical screen descriptor
	if len(data) < header {
		return 0, false
	}

	i := header
	if data[10]&0x80 != 0 {
		// A global color table of 3 bytes per entry, its size in the low bits.
		i += 3 << ((int(data[10]) & 0x07) + 1)
	}

	for {
		if i >= len(data) {
			return 0, false
		}

		switch data[i] {
		case 0x3b:
			return i + 1, true

		case 0x21:
			// An extension: a label, then sub-blocks.
			i += 2
			end, ok := subBlocks(data, i)
			if !ok {
				return 0, false
			}
			i = end

		case 0x2c:
			// An image: its descriptor, a local color table where it has one,
			// the LZW minimum code size, then sub-blocks.
			if i+10 > len(data) {
				return 0, false
			}
			flags := data[i+9]
			i += 10
			if flags&0x80 != 0 {
				i += 3 << ((int(flags) & 0x07) + 1)
			}
			i++ // the LZW minimum code size
			end, ok := subBlocks(data, i)
			if !ok {
				return 0, false
			}
			i = end

		default:
			return 0, false
		}
	}
}

// subBlocks walks GIF sub-blocks, each a length and that many bytes, to the empty
// one that ends them.
func subBlocks(data []byte, at int) (int, bool) {
	for {
		if at >= len(data) {
			return 0, false
		}
		size := int(data[at])
		at++
		if size == 0 {
			return at, true
		}
		at += size
	}
}

// zipLength finds the end-of-central-directory record that belongs to the archive
// beginning at the start of the data. The record says where the central directory
// is relative to the archive's start, so the archive is its own witness: a record
// belonging to another archive, or to one nested inside this one, points elsewhere.
func zipLength(data []byte) (int, bool) {
	eocd := []byte{'P', 'K', 0x05, 0x06}
	const eocdSize = 22

	from := 0
	for {
		j := bytes.Index(data[from:], eocd)
		if j < 0 {
			return 0, false
		}
		at := from + j
		from = at + 1

		if at+eocdSize > len(data) {
			return 0, false
		}
		dirSize := int(binary.LittleEndian.Uint32(data[at+12:]))
		dirOffset := int(binary.LittleEndian.Uint32(data[at+16:]))
		comment := int(binary.LittleEndian.Uint16(data[at+20:]))

		// The directory ends where this record begins, and begins with a
		// directory entry, or this record is not the one for this archive.
		if dirOffset+dirSize != at || dirOffset+4 > len(data) {
			continue
		}
		if !bytes.Equal(data[dirOffset:dirOffset+4], []byte{'P', 'K', 0x01, 0x02}) {
			continue
		}

		end := at + eocdSize + comment
		if end > len(data) {
			return 0, false
		}

		return end, true
	}
}

// pdfHeader is what a PDF opens with, down to the version it names.
var pdfHeader = regexp.MustCompile(`^%PDF-[12]\.\d`)

// startxref is what precedes a PDF's %%EOF: where its cross-reference table is,
// as an offset from the start of the file.
var startxref = regexp.MustCompile(`startxref\s+(\d+)\s*$`)

// xrefAt is what a cross-reference offset has to point at: a table, or the object
// that holds one.
var xrefAt = regexp.MustCompile(`^\s*(xref|\d+\s+\d+\s+obj)`)

// pdfLength finds the %%EOF that ends the PDF beginning at the start of the data.
// Each %%EOF is preceded by startxref and an offset into the file; one that points
// at a cross-reference table relative to this file's start belongs to it, as every
// %%EOF of a file revised in place does. The first that does not is another file's,
// and the last that does is this one's end.
func pdfLength(data []byte) (int, bool) {
	if !pdfHeader.Match(data) {
		return 0, false
	}

	eof := []byte("%%EOF")
	end, from := 0, 0
	for {
		j := bytes.Index(data[from:], eof)
		if j < 0 {
			break
		}
		at := from + j
		from = at + len(eof)

		// The trailer before this %%EOF, within the few lines it takes.
		lookback := at - 64
		if lookback < 0 {
			lookback = 0
		}
		m := startxref.FindSubmatch(data[lookback:at])
		if m == nil {
			break
		}
		offset, err := strconv.Atoi(string(m[1]))
		if err != nil || offset <= 0 || offset >= at {
			break
		}
		if !xrefAt.Match(data[offset:]) {
			break
		}

		end = from
		for end < len(data) && (data[end] == '\r' || data[end] == '\n') {
			end++
		}
	}

	return end, end > 0
}
