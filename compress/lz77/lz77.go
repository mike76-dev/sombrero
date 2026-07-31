// This is the LZ77 compression algorithm implementation
// as specified in MS-XCA.
package lz77

import (
	"encoding/binary"
	"errors"
)

var (
	ErrInvalidFormat = errors.New("lz77: invalid compressed stream")
	ErrUnexpectedEOF = errors.New("lz77: unexpected end of stream")
	ErrInvalidOffset = errors.New("lz77: invalid match offset")
)

// LZ77 uses a 13-bit offset => 1..8192.
const (
	windowSize = 8192
	minMatch   = 3
	hashBits   = 15
	hashSize   = 1 << hashBits
	maxChain   = 32 // tune for speed vs. ratio
)

// Compress compresses the input stream using LZ77.
func Compress(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}

	// Hash chains over the whole input; we only accept matches within last 8192 bytes.
	head := make([]int, hashSize)
	next := make([]int, len(src))
	for i := range head {
		head[i] = -1
	}

	for i := range next {
		next[i] = -1
	}

	hash3 := func(i int) int {
		// Caller ensures i+2 < len(src).
		h := (uint32(src[i]) << 16) ^ (uint32(src[i+1]) << 8) ^ uint32(src[i+2])

		// Simple mix to 15 bits.
		h ^= h >> 7
		h *= 0x9e3779b1

		return int((h >> (32 - hashBits)) & (hashSize - 1))
	}

	update := func(i int) {
		if i+2 >= len(src) {
			return
		}

		h := hash3(i)
		next[i] = head[h]
		head[h] = i
	}

	// Output with 4-byte flags placeholder. The capacity is a guess at the output being no bigger
	// than the input, and has to leave room for the placeholder whatever that guess is worth: an
	// input of one, two or three bytes would otherwise ask for a slice longer than its capacity,
	// which panics.
	dst := make([]byte, 4, len(src)+4)
	flagPos := 0
	var flags uint32
	flagCount := 0

	// Nibble packing for extended lengths.
	pendingNibblePos := -1 // position of byte in dst whose high nibble is free

	flushFlags := func(final bool) {
		if flagCount == 0 && !final {
			return
		}

		if final && flagCount > 0 {
			// Finalize flags word (pad with 1s).
			shift := 32 - flagCount
			flags <<= uint(shift)
			flags |= (uint32(1) << uint(shift)) - 1
		}

		binary.LittleEndian.PutUint32(dst[flagPos:flagPos+4], flags)
		flags = 0
		flagCount = 0
		if !final {
			flagPos = len(dst)
			dst = append(dst, 0, 0, 0, 0) // new placeholder
		}
	}

	writeMatch := func(offset, length int) {
		// length >= 3, offset 1..8192.
		ml := length - minMatch // MatchLength - 3
		tokLen := ml
		if tokLen > 7 {
			tokLen = 7
		}

		tok := uint16(((offset - 1) << 3) | tokLen)
		dst = append(dst, byte(tok), byte(tok>>8))

		if ml < 7 {
			return
		}

		// Extended: extra = (MatchLength-3) - 7 == length - 10.
		extra := ml - 7

		// Write 4-bit nibble (possibly 15).
		nib := extra
		if nib > 15 {
			nib = 15
		}

		if pendingNibblePos < 0 {
			dst = append(dst, byte(nib)) // low nibble; high nibble free
			pendingNibblePos = len(dst) - 1
		} else {
			dst[pendingNibblePos] |= byte(nib << 4)
			pendingNibblePos = -1
		}

		if extra < 15 {
			return
		}

		extra -= 15

		// Then 8-bit extra, or 255 + 16/32-bit length.
		if extra < 255 {
			dst = append(dst, byte(extra))
			return
		}

		dst = append(dst, 0xff)

		// Spec stores (MatchLength-3) as 16-bit if it fits, else 0 + 32-bit.
		total := uint32(length - minMatch)
		if total < 1<<16 {
			var tmp [2]byte
			binary.LittleEndian.PutUint16(tmp[:], uint16(total))
			dst = append(dst, tmp[:]...)
		} else {
			dst = append(dst, 0, 0)
			var tmp [4]byte
			binary.LittleEndian.PutUint32(tmp[:], total)
			dst = append(dst, tmp[:]...)
		}
	}

	findBest := func(pos int) (bestOff, bestLen int) {
		bestOff, bestLen = 0, 0
		if pos+2 >= len(src) {
			return
		}

		h := hash3(pos)
		c := head[h]
		depth := 0

		for c >= 0 && depth < maxChain {
			dist := pos - c
			if dist <= 0 {
				break
			}

			if dist > windowSize {
				break // chains are older than the window
			}

			// Fast reject.
			if src[c] != src[pos] || src[c+1] != src[pos+1] || src[c+2] != src[pos+2] {
				c = next[c]
				depth++
				continue
			}

			// Extend match.
			l := 3
			max := len(src) - pos
			if max > (1<<16)+minMatch { // keep it reasonable; decoder supports more via u32
				max = (1 << 16) + minMatch
			}

			for l < max && src[c+l] == src[pos+l] {
				l++
			}

			if l > bestLen {
				bestLen = l
				bestOff = dist
				if bestLen >= 258 { // good enough stop (tunable)
					break
				}
			}

			c = next[c]
			depth++
		}

		return
	}

	for i := 0; i < len(src); {
		// Ensure flags space.
		if flagCount == 32 {
			flushFlags(false)
		}

		bestOff, bestLen := findBest(i)
		if bestLen >= minMatch {
			// Emit match.
			flags = (flags << 1) | 1
			flagCount++

			writeMatch(bestOff, bestLen)

			// Update dictionary for each consumed byte (allows overlaps).
			for k := 0; k < bestLen; k++ {
				update(i)
				i++
				if i >= len(src) {
					break
				}
			}
		} else {
			// Emit literal.
			flags <<= 1
			flagCount++

			dst = append(dst, src[i])
			update(i)
			i++
		}
	}

	flushFlags(true)
	return dst
}

// Decompress decompresses the LZ77-compressed data into dst.
// If `limit` > 0, it enforces a maximum output size.
func Decompress(src []byte, limit int) (dst []byte, err error) {
	if limit > 0 {
		dst = make([]byte, 0, limit)
	}

	in := 0
	var flags uint32
	flagCount := 0

	// Nibble state for the "4-bit length" extension.
	// When we read a new byte for low nibble, the high nibble is saved for the *next* use.
	haveHighNible := false
	var savedNibbleByte byte

	readU8 := func() (byte, error) {
		if in >= len(src) {
			return 0, ErrUnexpectedEOF
		}
		b := src[in]
		in++
		return b, nil
	}

	readU16 := func() (uint16, error) {
		if in+2 > len(src) {
			return 0, ErrUnexpectedEOF
		}
		v := binary.LittleEndian.Uint16(src[in : in+2])
		in += 2
		return v, nil
	}

	readU32 := func() (uint32, error) {
		if in+4 > len(src) {
			return 0, ErrUnexpectedEOF
		}
		v := binary.LittleEndian.Uint32(src[in : in+4])
		in += 4
		return v, nil
	}

	for {
		// Refill flags if empty.
		if flagCount == 0 {
			// If there's no more input, we're done (permissive end).
			if in >= len(src) {
				return
			}

			// Need 4 bytes for flags.
			if in+4 > len(src) {
				return nil, ErrUnexpectedEOF
			}

			flags = binary.LittleEndian.Uint32(src[in : in+4])
			in += 4
			flagCount = 32
		}

		flagCount--
		isMatch := ((flags >> uint(flagCount)) & 1) != 0

		if !isMatch {
			// Literal.
			b, err := readU8()
			if err != nil {
				// If we ran out exactly at a literal, treat as truncation.
				return nil, err
			}

			dst = append(dst, b)
			if limit > 0 && len(dst) > limit {
				return nil, ErrInvalidFormat
			}

			continue
		}

		// Match: if we're exactly at end, decompression complete (allowed by spec).
		if in == len(src) {
			return
		}

		tok, err := readU16()
		if err != nil {
			return nil, err
		}

		baseLen := int(tok & 0x7) // 0..7
		offset := int(tok>>3) + 1 // 1..8192
		if offset < 1 || offset > windowSize || offset > len(dst) {
			return nil, ErrInvalidOffset
		}

		// Decode length.
		length := 0
		if baseLen < 7 {
			length = baseLen + minMatch
		} else {
			// Read 4-bit value (packed into nibbles).
			var nib int
			if !haveHighNible {
				b, err := readU8()
				if err != nil {
					return nil, err
				}

				savedNibbleByte = b
				nib = int(b & 0x0f)
				haveHighNible = true
			} else {
				nib = int(savedNibbleByte >> 4)
				haveHighNible = false
			}

			if nib < 15 {
				// length = (nib) + 7 + 3.
				length = nib + 7 + minMatch
			} else {
				// nib == 15 => read more.
				b, err := readU8()
				if err != nil {
					return nil, err
				}

				if b < 255 {
					// length = b + 15 + 7 + 3.
					length = int(b) + 15 + 7 + minMatch
				} else {
					// b == 255 => read 16-bit length, and if 0 then 32-bit length.
					v16, err := readU16()
					if err != nil {
						return nil, err
					}

					var m uint32
					if v16 != 0 {
						m = uint32(v16)
					} else {
						v32, err := readU32()
						if err != nil {
							return nil, err
						}

						m = v32
					}

					// m encodes (MatchLength - 3). Must be >= 22 for this path.
					if m < 15+7 {
						return nil, ErrInvalidFormat
					}

					// length = m + 3.
					length = int(m) + minMatch
				}
			}
		}

		// Apply match (byte-at-a-time; overlaps allowed).
		if limit > 0 && len(dst)+length > limit {
			return nil, ErrInvalidFormat
		}

		srcPos := len(dst) - offset
		for i := 0; i < length; i++ {
			dst = append(dst, dst[srcPos+i])
		}
	}
}
