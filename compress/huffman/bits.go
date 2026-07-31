package huffman

import "encoding/binary"

// The bit stream of [MS-XCA] is read and written in 16-bit little-endian words, with the bits of
// each word taken from the top down. Two things about it are unusual enough to be worth stating
// before the code that implements them.
//
// The first is that the words run two behind. The encoder keeps the next two word slots reserved
// and appends everything else past them, so a completed word is written four bytes back from where
// the buffer has reached. What that buys is the second thing: the extra bytes that carry a long
// match length are not part of the bit stream at all. They are plain bytes, dropped in at the
// point the buffer has reached - which, because of the two-word delay, is exactly where a decoder
// reading the stream two bytes at a time is about to read next.
//
// So a decoder holds two words in hand and reads the third from a position that the extra bytes
// share. Both sides therefore keep a single position and take words and stray bytes from it in the
// order they were written.

// writer builds the bit stream. It is not usable zero: newWriter reserves the two word slots.
type writer struct {
	buf []byte

	// pos1 and pos2 are the two reserved word slots, the older first. A completed word goes to
	// pos1, and both slots then move up: pos2 becomes the older, and a new slot is appended.
	pos1, pos2 int

	next     uint32 // the word being filled, held in the low bits
	freeBits uint   // how much of it is still empty
}

func newWriter(buf []byte) *writer {
	w := &writer{buf: buf, freeBits: 16}

	w.pos1 = len(w.buf)
	w.buf = append(w.buf, 0, 0)
	w.pos2 = len(w.buf)
	w.buf = append(w.buf, 0, 0)

	return w
}

// writeBits appends the low n bits of v, most significant first. n is never more than 16.
func (w *writer) writeBits(n uint, v uint32) {
	if n == 0 {
		return
	}

	if w.freeBits >= n {
		w.freeBits -= n
		w.next = (w.next << n) | v
		return
	}

	// The word fills up partway through the value: the top of the value completes it, and what is
	// left over starts the next one.
	w.next = (w.next << w.freeBits) | (v >> (n - w.freeBits))
	w.putWord(uint16(w.next))

	w.freeBits += 16 - n
	w.next = v
}

// putWord writes a completed word to the older reserved slot and reserves another.
func (w *writer) putWord(v uint16) {
	binary.LittleEndian.PutUint16(w.buf[w.pos1:], v)
	w.pos1 = w.pos2
	w.pos2 = len(w.buf)
	w.buf = append(w.buf, 0, 0)
}

// writeByte appends a byte outside the bit stream, at the point the buffer has reached.
func (w *writer) writeByte(b byte) {
	w.buf = append(w.buf, b)
}

func (w *writer) writeUint16(v uint16) {
	w.buf = binary.LittleEndian.AppendUint16(w.buf, v)
}

func (w *writer) writeUint32(v uint32) {
	w.buf = binary.LittleEndian.AppendUint32(w.buf, v)
}

// flush pads the word being filled and closes the stream. The second reserved slot is left as the
// zeroes it was appended as: a decoder reads two words ahead, so the stream has to hold two words
// past its last real bits for that read to land inside the buffer.
func (w *writer) flush() []byte {
	binary.LittleEndian.PutUint16(w.buf[w.pos1:], uint16(w.next<<w.freeBits))

	return w.buf
}

// reader takes the stream back apart.
type reader struct {
	buf []byte
	pos int // the next byte to be read, shared by the words and the stray bytes

	bits  uint32 // the next bits, held at the top
	count uint   // how many of them are real

	// consumed is how many bits have been taken, and extra how many bytes have been read from
	// outside the stream. Together they say where the block ends: the bits went into whole words,
	// so the number of words follows from the count, and the stray bytes are the only other thing
	// in there. Nothing in the stream itself marks the end.
	consumed int
	extra    int
}

func newReader(buf []byte) *reader {
	r := &reader{buf: buf}
	r.refill()

	return r
}

// refill tops the register up to 16 bits, so that any code and any run of extra distance bits can
// be taken from it without a second look.
//
// Sixteen and not seventeen, and the difference matters. The position this reads words from is the
// same one the stray bytes are read from, so it has to sit exactly where the encoder's did: after
// writing n bits the encoder has reserved ceil(n/16)+1 word slots, and stopping at 16 is what puts
// this at the same place. Taking one word more would agree with it everywhere except where n comes
// to a whole number of words, and there it would read a stray byte as though it were part of the
// stream.
//
// A stream that has run out is padded with zeroes rather than reported. Nothing is decoded from
// them: the caller stops at the end-of-file symbol or when it has produced the bytes it was told
// to expect, and a stream that reaches neither is caught there rather than here.
func (r *reader) refill() {
	for r.count < 16 {
		var word uint16
		if r.pos+2 <= len(r.buf) {
			word = binary.LittleEndian.Uint16(r.buf[r.pos:])
		}
		r.pos += 2

		r.bits |= uint32(word) << (16 - r.count)
		r.count += 16
	}
}

// peek returns the next n bits without consuming them, n never more than 16.
func (r *reader) peek(n uint) uint32 {
	return r.bits >> (32 - n)
}

// skip consumes n bits.
func (r *reader) skip(n uint) {
	r.bits <<= n
	r.count -= n
	r.consumed += int(n)
	r.refill()
}

// take consumes n bits and returns them.
func (r *reader) take(n uint) uint32 {
	if n == 0 {
		return 0
	}

	v := r.peek(n)
	r.skip(n)

	return v
}

// readByte takes a byte from outside the bit stream. ok is false past the end, which is a stream
// that promised a long match length and then stopped short of saying what it was.
func (r *reader) readByte() (byte, bool) {
	if r.pos >= len(r.buf) {
		return 0, false
	}

	b := r.buf[r.pos]
	r.pos++
	r.extra++

	return b, true
}

func (r *reader) readUint16() (uint16, bool) {
	if r.pos+2 > len(r.buf) {
		return 0, false
	}

	v := binary.LittleEndian.Uint16(r.buf[r.pos:])
	r.pos += 2
	r.extra += 2

	return v, true
}

func (r *reader) readUint32() (uint32, bool) {
	if r.pos+4 > len(r.buf) {
		return 0, false
	}

	v := binary.LittleEndian.Uint32(r.buf[r.pos:])
	r.pos += 4
	r.extra += 4

	return v, true
}
