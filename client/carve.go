package client

import "bytes"

// Carving is what is left for the data whose names are gone: an object that says
// nothing about itself still holds the bytes of whatever was packed into it, and
// the files in it can often be found by what they begin and end with.
//
// Only a file that ends inside the object it began in is taken. A large file is
// held in slab-sized pieces, so its first piece begins with a header and ends
// nowhere: calling that a file would hand back a fifth of a video and say it was
// whole.

// A signature is how one kind of file is recognized: what it opens with, what it
// closes with, and what to call it.
type signature struct {
	ext    string
	header []byte

	// trailer closes the file, and trailerExtra is what follows it that belongs
	// to the file as well: a checksum, a record length, or nothing.
	trailer      []byte
	trailerExtra int

	// minSize is the least a file of this kind can measure, which keeps a stray
	// header followed at once by a trailer from being taken for a file.
	minSize int
}

// signatures are the kinds of file the carver knows. They are the ones that both
// open and close with something recognizable, which is what makes a find whole
// rather than a guess at where it ended.
var signatures = []signature{
	{ext: "pdf", header: []byte("%PDF-"), trailer: []byte("%%EOF"), minSize: 64},
	{ext: "jpg", header: []byte{0xff, 0xd8, 0xff}, trailer: []byte{0xff, 0xd9}, minSize: 128},
	{
		ext:          "png",
		header:       []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a},
		trailer:      []byte("IEND"),
		trailerExtra: 4, // the CRC of the empty IEND chunk
		minSize:      64,
	},
	{ext: "gif", header: []byte("GIF89a"), trailer: []byte{0x3b}, minSize: 64},
	{ext: "gif", header: []byte("GIF87a"), trailer: []byte{0x3b}, minSize: 64},
	{
		ext:          "zip",
		header:       []byte{'P', 'K', 0x03, 0x04},
		trailer:      []byte{'P', 'K', 0x05, 0x06},
		trailerExtra: 18, // the end-of-central-directory record, without a comment
		minSize:      64,
	},
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
//
// A find runs from a header to the last matching trailer before the next header
// of any kind, which is as much of a file as there can be certainty about: what
// lies between two headers was written by whoever wrote the first of them.
func carve(data []byte) (finds []Find, leftover uint64) {
	var at int
	for at < len(data) {
		sig, start := nextHeader(data, at)
		if sig == nil {
			break
		}

		// What a header is followed by is only this file's as far as the next
		// header, whatever kind that one is.
		limit := len(data)
		if _, next := nextHeader(data, start+1); next > 0 {
			limit = next
		}

		end, ok := closeOf(*sig, data, start, limit)
		if !ok {
			// A file that does not end in here is a piece of one that is longer
			// than this object, and a piece is not a file.
			at = start + len(sig.header)
			continue
		}

		finds = append(finds, Find{Offset: uint64(start), Length: uint64(end - start), Ext: sig.ext})
		at = end
	}

	var taken uint64
	for _, find := range finds {
		taken += find.Length
	}

	return finds, uint64(len(data)) - taken
}

// nextHeader returns the first signature whose header appears at or after from,
// and where it appears. The signature is nil where none does.
func nextHeader(data []byte, from int) (*signature, int) {
	if from >= len(data) {
		return nil, -1
	}

	var found *signature
	at := -1
	for i := range signatures {
		offset := bytes.Index(data[from:], signatures[i].header)
		if offset < 0 {
			continue
		}
		if at < 0 || from+offset < at {
			found = &signatures[i]
			at = from + offset
		}
	}

	return found, at
}

// closeOf returns where the file that begins at start ends, looking no further
// than limit. It takes the last trailer in that span, since a file of this kind
// may hold something that looks like its own ending.
func closeOf(sig signature, data []byte, start, limit int) (int, bool) {
	span := data[start:limit]
	offset := bytes.LastIndex(span, sig.trailer)
	if offset < 0 {
		return 0, false
	}

	end := start + offset + len(sig.trailer) + sig.trailerExtra
	if end > limit || end-start < sig.minSize {
		return 0, false
	}

	return end, true
}
