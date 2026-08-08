// Package huffman implements the LZ77+Huffman variant of the Xpress compression algorithm, as
// specified in [MS-XCA]. It is one of the algorithms an SMB2 peer may negotiate for compressing
// messages, where it goes under the name COMPRESSION_LZ77_HUFFMAN.
//
// A compressed stream is a run of blocks, each covering at most 64 KiB of the original data. A
// block is a 256-byte table giving the code length of every one of the 512 symbols, four bits
// apiece, followed by the symbols themselves as a bit stream. The last block ends with the
// end-of-file symbol.
package huffman

import (
	"errors"
	"math/bits"
)

var (
	ErrInvalidFormat = errors.New("huffman: invalid compressed stream")
	ErrUnexpectedEOF = errors.New("huffman: unexpected end of stream")
	ErrInvalidOffset = errors.New("huffman: invalid match offset")
)

const (
	// blockSize is how much of the original data one block covers.
	blockSize = 64 * 1024

	// tableSize is the 256 bytes of code lengths every block opens with: 512 symbols at two to
	// the byte.
	tableSize = symbolCount / 2

	// minMatch is the shortest run worth pointing back to, and is taken off every match length
	// before it is written down.
	minMatch = 3

	// maxDistance is as far back as a match may reach. The symbol carries the position of the top
	// bit of the distance, and the specification works that out on the understanding that the
	// distance is under 1 << 16.
	maxDistance = 65535
)

// Compress compresses the input.
func Compress(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}

	var dst []byte
	for start := 0; start < len(src); start += blockSize {
		end := min(start+blockSize, len(src))
		dst = compressBlock(dst, src[start:end], end == len(src))
	}

	return dst
}

// item is one thing to be encoded: a literal if length is zero, and otherwise a match.
type item struct {
	literal  byte
	length   int
	distance int
}

// compressBlock appends one block covering the whole of src.
//
// Matches never reach outside the block. A decompressor keeps everything it has produced and would
// follow one that did, but staying inside is what makes a block self-contained, which is what the
// specification describes.
func compressBlock(dst []byte, src []byte, last bool) []byte {
	items, freq := parse(src, last)

	lengths := codeLengths(freq)
	codes := canonicalCodes(lengths)

	// The table, two symbols to the byte: the even one low, the odd one high.
	table := make([]byte, tableSize)
	for sym := 0; sym < symbolCount; sym += 2 {
		table[sym/2] = lengths[sym] | lengths[sym+1]<<4
	}
	dst = append(dst, table...)

	w := newWriter(dst)
	for _, it := range items {
		if it.length == 0 {
			w.writeBits(uint(lengths[it.literal]), uint32(codes[it.literal]))
			continue
		}

		high := uint(bits.Len32(uint32(it.distance))) - 1
		sym := eofSymbol + min(it.length-minMatch, 15) + 16*int(high)
		w.writeBits(uint(lengths[sym]), uint32(codes[sym]))

		// A length that the symbol could not hold is written outside the bit stream, as plain
		// bytes, widening as far as it has to.
		if n := it.length - minMatch; n >= 15 {
			if n -= 15; n < 255 {
				w.writeByte(byte(n))
			} else {
				w.writeByte(255)
				if n += 15; n < 65536 {
					w.writeUint16(uint16(n))
				} else {
					w.writeUint16(0)
					w.writeUint32(uint32(n))
				}
			}
		}

		w.writeBits(high, uint32(it.distance)-(1<<high))
	}

	if last {
		w.writeBits(uint(lengths[eofSymbol]), uint32(codes[eofSymbol]))
	}

	return w.flush()
}

// parse runs the LZ77 pass: what the block breaks down into, and how often each symbol is used.
func parse(src []byte, last bool) ([]item, []uint32) {
	freq := make([]uint32, symbolCount)
	items := make([]item, 0, len(src)/2+1)

	// head maps the hash of three bytes to the most recent place they were seen, and chain maps a
	// position to the one before it with the same hash.
	const hashBits = 15
	head := make([]int32, 1<<hashBits)
	for i := range head {
		head[i] = -1
	}
	chain := make([]int32, len(src))

	hash := func(i int) uint32 {
		h := uint32(src[i])<<16 ^ uint32(src[i+1])<<8 ^ uint32(src[i+2])
		h ^= h >> 7
		h *= 0x9e3779b1
		return h >> (32 - hashBits)
	}

	for i := 0; i < len(src); {
		if i+minMatch > len(src) {
			items = append(items, item{literal: src[i]})
			freq[src[i]]++
			i++
			continue
		}

		h := hash(i)

		// How far back to look. Thirty-two candidates is the same limit the plain LZ77 encoder in
		// this module uses, and buys most of what an unbounded search would.
		bestLen, bestDist := 0, 0
		const maxChain = 32
		for p, n := head[h], 0; p >= 0 && n < maxChain; p, n = chain[p], n+1 {
			dist := i - int(p)
			if dist > maxDistance {
				break
			}

			// Only a candidate that beats what is already in hand is worth measuring, so the byte
			// that would have to match is checked before the rest of it.
			if bestLen > 0 && (i+bestLen >= len(src) || src[int(p)+bestLen] != src[i+bestLen]) {
				continue
			}

			l := 0
			for i+l < len(src) && src[int(p)+l] == src[i+l] {
				l++
			}

			if l > bestLen {
				bestLen, bestDist = l, dist
			}
		}

		chain[i] = head[h]
		head[h] = int32(i)

		// A match of the shortest length at a distance of one has nowhere to be written down. The
		// symbol of a match is the end-of-file symbol plus the length above the shortest, plus
		// sixteen for every bit of the distance below its highest - so the shortest match at the
		// nearest distance adds nothing to either, and is the end-of-file symbol itself. A decoder
		// reading it stops there, which is what four bytes of the same value did to this: the
		// literal, then three of it at a distance of one, and the block ended after one byte.
		//
		// Written out as a literal instead, it costs a few bits on a run of exactly four and
		// nothing anywhere else: a longer run at that distance carries a length above the shortest,
		// and any other distance carries a bit above the lowest.
		if bestLen < minMatch || (bestLen == minMatch && bestDist == 1) {
			items = append(items, item{literal: src[i]})
			freq[src[i]]++
			i++
			continue
		}

		items = append(items, item{length: bestLen, distance: bestDist})
		high := uint(bits.Len32(uint32(bestDist))) - 1
		freq[eofSymbol+min(bestLen-minMatch, 15)+16*int(high)]++

		// The positions the match covers still go into the table, so that a later match can start
		// inside what this one wrote.
		for j := i + 1; j < i+bestLen; j++ {
			if j+minMatch <= len(src) {
				h := hash(j)
				chain[j] = head[h]
				head[h] = int32(j)
			}
		}
		i += bestLen
	}

	if last {
		freq[eofSymbol]++
	}

	return items, freq
}

// Decompress decompresses the input. limit is how many bytes it is expected to come to, which is
// what the SMB2 transform header carries alongside the data.
func Decompress(src []byte, limit int) ([]byte, error) {
	if len(src) == 0 {
		return nil, nil
	}
	if limit < 0 {
		return nil, ErrInvalidFormat
	}

	dst := make([]byte, 0, limit)
	for pos := 0; len(dst) < limit; {
		if pos+tableSize >= len(src) {
			return nil, ErrUnexpectedEOF
		}

		var lengths [symbolCount]uint8
		for i, b := range src[pos : pos+tableSize] {
			lengths[2*i] = b & 0x0f
			lengths[2*i+1] = b >> 4
		}

		// A block covers 64 KiB unless what is left of the expected output is less, in which case
		// this is the last one and the end-of-file symbol closes it.
		want := min(limit-len(dst), blockSize)
		last := limit-len(dst) <= blockSize

		out, used, err := decompressBlock(newDecodeTable(lengths[:]), src[pos+tableSize:], dst, want, last)
		if err != nil {
			return nil, err
		}

		dst = out
		pos += tableSize + used
	}

	return dst, nil
}

// decompressBlock decodes one block of want bytes onto the end of dst, and returns how much of src
// the block took up. Only the last block of a stream ends with the end-of-file symbol.
func decompressBlock(table *decodeTable, src []byte, dst []byte, want int, last bool) ([]byte, int, error) {
	r := newReader(src)
	produced := 0

	for produced < want {
		sym, ok := table.symbol(r)
		if !ok {
			return nil, 0, ErrInvalidFormat
		}

		// The stream ending before it has produced what it promised means it was cut short, or
		// that the length it was decompressed against was not its own.
		if sym == eofSymbol {
			return nil, 0, ErrUnexpectedEOF
		}

		if sym < eofSymbol {
			dst = append(dst, byte(sym))
			produced++
			continue
		}

		n := (sym - eofSymbol) & 15
		high := uint((sym - eofSymbol) >> 4)

		// A length of 15 in the symbol means it did not fit there, and the real one is outside the
		// bit stream as plain bytes. Each width is only reached when the one below it is full: a
		// byte of 255 says to read a further two, and those being zero says to read four more.
		if n == 15 {
			b, ok := r.readByte()
			if !ok {
				return nil, 0, ErrUnexpectedEOF
			}

			if b < 255 {
				n = 15 + int(b)
			} else {
				v, ok := r.readUint16()
				if !ok {
					return nil, 0, ErrUnexpectedEOF
				}

				if n = int(v); v == 0 {
					u, ok := r.readUint32()
					if !ok {
						return nil, 0, ErrUnexpectedEOF
					}
					n = int(u)
				}
			}
		}
		length := n + minMatch

		// The symbol holds the position of the top bit of the distance; the rest of it follows in
		// the bit stream, after any of those bytes.
		distance := 1<<high + int(r.take(high))

		if distance > len(dst) {
			return nil, 0, ErrInvalidOffset
		}
		if produced+length > want {
			return nil, 0, ErrInvalidFormat
		}

		// A match may be longer than the distance it reaches back, in which case it repeats what
		// it is still producing. Copying a byte at a time is what makes that come out right.
		from := len(dst) - distance
		for i := 0; i < length; i++ {
			dst = append(dst, dst[from+i])
		}
		produced += length
	}

	// The end-of-file symbol is part of the last block's stream, so it has to be taken before the
	// block can be measured. Leaving it would put the count of bits short and, with it, the place
	// the next block was looked for.
	if last {
		if sym, ok := table.symbol(r); !ok || sym != eofSymbol {
			return nil, 0, ErrInvalidFormat
		}
	}

	// Nothing in the stream says where the block ended, so it is worked out: the bits went into
	// whole 16-bit words, one more word was written to close the stream and another left empty
	// behind it, and the bytes taken from outside the stream sit among them.
	words := (r.consumed+15)/16 + 1
	used := 2*words + r.extra
	if used > len(src) {
		return nil, 0, ErrUnexpectedEOF
	}

	return dst, used, nil
}
