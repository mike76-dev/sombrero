// This is a port from a Rust implementation of LZNT1.
// https://github.com/xangelix/lznt1.
package lznt1

import (
	"encoding/binary"
	"errors"
)

var (
	// Decompression errors.
	ErrUnexpectedEOF = errors.New("lznt1: unexpected end of stream")
	ErrInvalidHeader = errors.New("lznt1: invalid block header")
	ErrInvalidOffset = errors.New("lznt1: lookback offset out of bounds")
	ErrInputTooShort = errors.New("lznt1: input buffer too short for expected data")
	ErrOutputTooLong = errors.New("lznt1: the stream expands past the size it was said to come to")
)

const (
	chunkSize        = 4096           // Standard chunk size for LZNT1 compression (4KB)
	minMatch         = 3              // Minimum match length required to encode a compression tuple
	maxMatch         = 4098           // Absolute hard limit for match length (12 bits + 3)
	maxSearchDepth   = 16             // Maximum number of hash chain entries to inspect per position
	hashMask         = 0xfff          // Hash mask for the 4096-entry table (12 bits)
	emptyEntry       = uint16(0xffff) // Marker for an empty hash table entry
	headerCompressed = uint16(0xb000) // Header flag for compressed chunks
	headerRaw        = uint16(0x3000) // Header flag for uncompressed chunks

	headerCompressedFlag = uint16(0x8000) // Bit flag indicating if the chunk is compressed (0xBxxx) or raw (0x3xxx)
	tagGroupSize         = 8              // Number of items (literals or tuples) in a single tag group
	initialSplit         = 12             // Initial bit width for the length component of a match tuple
	initialThreshold     = 16             // Initial threshold for the uncompressed size before adaptive state update
)

// Internal helper struct to manage the LZNT1 "Tag Group" logic.
// A Tag Group consists of 1 flag byte followed by up to 8 tokens (literals or tuples).
// The flag byte contains one bit per token (0=Literal, 1=Tuple).
type tagAccumulator struct {
	tagByte      byte
	itemCount    int
	buffer       [16]byte
	bufferLength int
}

// pushLiteral adds a literal byte to the current group.
func (ta *tagAccumulator) pushLiteral(b uint8, output *[]byte) {
	ta.buffer[ta.bufferLength] = b
	ta.bufferLength++
	ta.commitItem(output)
}

// pushTuple adds a compressed tuple (offset/length pair) to the current group.
func (ta *tagAccumulator) pushTuple(tuple uint16, output *[]byte) {
	ta.tagByte |= 1 << ta.itemCount
	ta.buffer[ta.bufferLength] = byte(tuple)
	ta.buffer[ta.bufferLength+1] = byte(tuple >> 8)
	ta.bufferLength += 2
	ta.commitItem(output)
}

// commitItem increments the item count and flushes the group if full (8 items).
func (ta *tagAccumulator) commitItem(output *[]byte) {
	ta.itemCount++
	if ta.itemCount == 8 {
		ta.flush(output)
	}
}

// flush writes the current tag group and resets the state.
func (ta *tagAccumulator) flush(output *[]byte) {
	if ta.itemCount > 0 {
		*output = append(*output, ta.tagByte)
		*output = append(*output, ta.buffer[:ta.bufferLength]...)
		ta.tagByte = 0
		ta.itemCount = 0
		ta.bufferLength = 0
	}
}

// context holds reusable memory for compression to avoid allocation churn.
type context struct {
	head [chunkSize]uint16 // Maps a 3-byte hash to the *most recent* position in the chunk
	next [chunkSize]uint16 // Maps a position to the *previous* position with the same hash
}

// newContext returns an initialized context.
func newContext() *context {
	c := &context{}
	for i := range c.head {
		c.head[i] = emptyEntry
		c.next[i] = emptyEntry
	}
	return c
}

// reset resets the hash table for a new chunk.
func (c *context) reset() {
	for i := range c.head {
		c.head[i] = emptyEntry
	}
}

// update updates the hash chain for the given index.
// This should be called for every byte processed (literal or matched) to allow
// overlapping matches in future searches.
func (c *context) update(input []byte, idx int) {
	if idx+minMatch <= len(input) {
		h := hash3(input[idx : idx+3])
		c.next[idx] = c.head[h]
		c.head[h] = uint16(idx)
	}
}

// Compress compresses input using an LZNT1-like chunk format.
func Compress(src []byte) (dst []byte) {
	ctx := newContext()
	srcPos := 0
	for srcPos < len(src) {
		chunkLen := min(len(src)-srcPos, chunkSize)
		chunk := src[srcPos : srcPos+chunkLen]
		startOut := len(dst)

		// Reserve space for Header (2 bytes).
		dst = append(dst, 0, 0)

		dst = compressChunk(chunk, dst, ctx)
		compressedLen := len(dst) - startOut - 2
		if compressedLen < len(chunk) {
			// Success: Overwrite header with Compressed flag + size.
			header := encodeHeader(headerCompressed, compressedLen)
			dst[startOut] = byte(header)
			dst[startOut+1] = byte(header >> 8)
		} else {
			// Failure: Expansion or no savings. Revert and store Raw.
			dst = dst[:startOut]
			header := encodeHeader(headerRaw, len(chunk))
			dst = append(dst, byte(header), byte(header>>8))
			dst = append(dst, chunk...)
		}

		srcPos += chunkLen
	}
	return dst
}

// compressChunk compresses a single chunk (max 4096 bytes).
func compressChunk(chunk []byte, output []byte, ctx *context) []byte {
	ctx.reset()
	ta := tagAccumulator{}

	// Adaptive state.
	blobOutLen := 0 // "Uncompressed" bytes represented so far.
	split := 12     // 12 bits Length, 4 bits Offset.
	threshold := 16 // When blobOutLen > threshold, shift parameters.

	inIdx := 0
	for inIdx < len(chunk) {
		// Current max bits allowed for offset based on adaptive split.
		offBits := 16 - split
		maxOffset := 1 << offBits

		bestLen := 0
		bestOff := 0

		// 1. Find best match.
		if inIdx+minMatch <= len(chunk) {
			hash := hash3(chunk[inIdx : inIdx+3])
			candidateIdx := ctx.head[hash]
			depth := 0

			for candidateIdx != emptyEntry && depth < maxSearchDepth {
				candidate := int(candidateIdx)
				if candidate >= inIdx {
					break // Should not happen with the correct logic
				}

				dist := inIdx - candidate
				if dist >= maxOffset {
					break // Too far from current adaptive window
				}

				// Optimization: Check the byte at `bestLen` to fail fast.
				if inIdx+bestLen < len(chunk) && chunk[candidate+bestLen] == chunk[inIdx+bestLen] {
					matchLen := commonPrefixLen(chunk[inIdx:], chunk[candidate:], maxMatch)
					if matchLen >= minMatch && matchLen > bestLen {
						bestLen = matchLen
						bestOff = dist
						if bestLen >= maxMatch {
							bestLen = maxMatch
							break
						}
					}
				}

				candidateIdx = ctx.next[candidate]
				depth++
			}
		}

		// 2. Encode Match or Literal.
		if bestLen >= minMatch {
			// Clamp length to fit in current `split` bits.
			// Max encodable length = (2^split) + 3 - 1.
			maxLenEncodable := (1 << split) + 2
			if bestLen > maxLenEncodable {
				bestLen = maxLenEncodable
			}

			// Tuple = ((off - 1) << split) | (len - 3).
			lenVal := bestLen - 3
			offVal := bestOff - 1
			tuple := uint16((offVal << split) | lenVal)
			ta.pushTuple(tuple, &output)

			// Update hash for all bytes covered by the match.
			for i := 0; i < bestLen; i++ {
				ctx.update(chunk, inIdx)
				inIdx++
			}
			blobOutLen += bestLen
		} else {
			// Literal.
			ta.pushLiteral(chunk[inIdx], &output)
			ctx.update(chunk, inIdx)
			inIdx++
			blobOutLen++
		}

		// 3. Adaptive Threshold update.
		for blobOutLen > threshold {
			if split > 0 {
				split--
			}
			threshold <<= 1
		}
	}

	// Flush any remaining items in the accumulator.
	ta.flush(&output)
	return output
}

// Decompress decompresses an entire LZNT1 stream.
// The input is processed in chunks (headers + data). The function manages
// output capacity reservation and validates the integrity of chunk headers.
// Decompress decompresses the input. limit is how many bytes it is expected to come to, and
// nothing longer than that is built: the input arrives from the far end, and a chunk of it stands
// for as much as it likes, so without a bound a short message expands until there is no memory
// left. A limit of zero or less asks for no bound, which is only for a caller that produced the
// input itself.
func Decompress(src []byte, limit int) (dst []byte, err error) {
	// Heuristic capacity reservation to reduce allocation churn.
	heuristicCap := len(src)
	if limit > 0 && limit < heuristicCap {
		heuristicCap = limit
	}
	if cap(dst) < len(dst)+heuristicCap {
		newDst := make([]byte, len(dst), len(dst)+heuristicCap)
		copy(newDst, dst)
		dst = newDst
	}

	inPos := 0
	end := len(src)
	for inPos < end {
		// LZNT1 streams may be null-terminated (single 0x00 byte at EOF).
		if inPos+1 == end && src[inPos] == 0 {
			break
		}

		// Ensure we can read the 2-byte header.
		if inPos+2 > end {
			return nil, ErrUnexpectedEOF
		}

		header := binary.LittleEndian.Uint16(src[inPos : inPos+2])
		inPos += 2

		if header == 0 {
			break // Standard End-of-Stream marker
		}

		size := int((header & hashMask) + 1)
		isCompressed := (header & headerCompressedFlag) != 0

		// Ensure the chunk body is within bounds.
		if inPos+size > end {
			return nil, ErrInputTooShort
		}

		blockSlice := src[inPos : inPos+size]
		if isCompressed {
			var err error
			dst, err = decompressBlock(blockSlice, dst, limit)
			if err != nil {
				return nil, err
			}
		} else {
			// Raw block: direct copy.
			dst = append(dst, blockSlice...)
		}

		if limit > 0 && len(dst) > limit {
			return nil, ErrOutputTooLong
		}

		inPos += size
	}

	return dst, nil
}

// decompressBlock decompresses a single compressed LZNT1 block.
func decompressBlock(src []byte, dst []byte, limit int) ([]byte, error) {
	inIdx := 0
	end := len(src)

	// Adaptive State.
	split := initialSplit
	mask := (1 << split) - 1
	threshold := initialThreshold
	startOutLen := len(dst)

	for inIdx < end {
		// 1. Load Tag Byte.
		tagByte := src[inIdx]
		inIdx++

		// All-Literals Fast Path.
		// If tag is 0, the next 8 items are literals.
		// We only take this path if we have enough bytes remaining to avoid EOF checks.
		if tagByte == 0 && inIdx+tagGroupSize <= end {
			dst = append(dst, src[inIdx:inIdx+tagGroupSize]...)
			inIdx += tagGroupSize

			// Update adaptive parameters for the 8 bytes just added.
			updateAdaptiveState(len(dst)-startOutLen, &threshold, &split, &mask)
			continue
		}

		// 2. Mixed Literals/Links Loop.
		for i := range tagGroupSize {
			if (tagByte>>i)&1 != 0 {
				// Ensure we have 2 bytes for the tuple.
				if inIdx+2 > end {
					return nil, ErrUnexpectedEOF
				}

				tuple := int(binary.LittleEndian.Uint16(src[inIdx : inIdx+2]))
				inIdx += 2

				// Decode Length/Offset using current adaptive split.
				length := (tuple & mask) + 3
				offset := (tuple >> split) + 1

				var err error
				dst, err = applyMatch(dst, length, offset)
				if err != nil {
					return nil, err
				}

				// A back reference is two bytes of input standing for up to four thousand of
				// output, so this is where a bound has to be noticed rather than at the end of
				// the chunk.
				if limit > 0 && len(dst) > limit {
					return nil, ErrOutputTooLong
				}
			} else {
				// Literal.
				if inIdx >= end {
					// Valid end of stream inside a literal tag group.
					// This is a permissive behavior required by LZNT1 specs.
					return dst, nil
				}

				dst = append(dst, src[inIdx])
				inIdx++
			}

			// Update adaptive parameters after *every* item.
			updateAdaptiveState(len(dst)-startOutLen, &threshold, &split, &mask)

			// Check EOF after processing item.
			if inIdx >= end {
				return dst, nil
			}
		}
	}

	return dst, nil
}

// Helper to format the 2-byte chunk header.
// Header format: `Flag | (Size - 1) & 0xFFF`.
func encodeHeader(flag uint16, size int) uint16 {
	return flag | (uint16(size-1) & hashMask)
}

// hash3 hashes the first 3 bytes of a slice for the LZNT1 dictionary lookup.
func hash3(b []byte) int {
	h := (int(b[0]) << 6) ^ (int(b[1]) << 3) ^ int(b[2])
	return h & hashMask
}

// commonPrefixLen finds the length of the common prefix between two slices, up to `max`.
func commonPrefixLen(a, b []byte, max int) int {
	limit := min(len(a), len(b), max)
	i := 0
	for i < limit && a[i] == b[i] {
		i++
	}
	return i
}

// updateAdaptiveState updates the adaptive window parameters (split, mask, threshold)
// based on the current uncompressed block size.
func updateAdaptiveState(currentLen int, threshold, split, mask *int) {
	for currentLen > *threshold {
		if *split > 0 {
			*split -= 1
			*mask = (1 << *split) - 1
		}
		*threshold <<= 1
	}
}

// applyMatch applies an LZ77 match to the output buffer.
// Handles data copying from the existing output history. Includes an optimization
// for Run-Length Encoding (RLE) where offset is 1.
func applyMatch(dst []byte, length, offset int) ([]byte, error) {
	if offset > len(dst) {
		return nil, ErrInvalidOffset
	}

	// Grow capacity if needed.
	if cap(dst) < len(dst)+length {
		newDst := make([]byte, len(dst), len(dst)+length)
		copy(newDst, dst)
		dst = newDst
	}

	// RLE Fast Path (Offset == 1).
	// Since offset > 0 (checked implicitly by offset > dst.len() if dst is empty),
	// and we know dst.len() >= offset, dst is not empty here.
	if offset == 1 {
		lastByte := dst[len(dst)-1]
		oldLen := len(dst)
		dst = dst[:oldLen+length]
		for i := oldLen; i < oldLen+length; i++ {
			dst[i] = lastByte
		}
		return dst, nil
	}

	// Standard LZ77 Copy (supports overlapping ranges).
	srcPos := len(dst) - offset
	for k := range length {
		dst = append(dst, dst[srcPos+k])
	}

	return dst, nil
}
