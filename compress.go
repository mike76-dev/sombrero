package main

import (
	"encoding/binary"
	"errors"
	"log"
	"slices"

	"github.com/mike76-dev/sombrero/compress"
	"github.com/mike76-dev/sombrero/smb2"
)

var errDecompressionError = errors.New("decompression failed")

// minCompressedSegment is the least data a message has to carry to be worth compressing at all.
//
// [MS-SMB2] 3.1.4.4 puts the floor at 1024 bytes, and a Windows client will not have it: every
// response carrying 64 KiB or more is taken, at every ratio, and every response carrying 8 KiB or
// less is answered with a reset. It is no loss either way. What compression is worth having is on
// the large reads a transfer is made of; below this a message saves a kilobyte or two and costs the
// peer a transform header to take apart.
const minCompressedSegment = 64 * 1024

// decompress decompresses the received message.
func (c *connection) decompress(msg []byte) ([]byte, error) {
	if !c.server.compressionSupported || len(c.compressionIDs) == 0 {
		return nil, smb2.ErrCompressedMessage
	}

	if len(msg) < smb2.SMB2CompressionTransformHeaderSize {
		return nil, smb2.ErrWrongLength
	}

	ocss := smb2.Header(msg).OriginalCompressedSegmentSize()

	if uint64(ocss) > 256+smb2.SMB2CompressionTransformHeaderSize+max(c.maxReadSize, c.maxWriteSize, c.maxTransactSize) {
		return nil, smb2.ErrInvalidParameter
	}

	// Which of the two shapes the message is in is the client's to say, but not the client's to
	// choose: a chained message is only a chained message on a connection that negotiated chaining.
	// Taken as unchained instead, its payload header is read as the head of an unchained segment and
	// the message comes apart as something nobody sent - so it is turned away rather than guessed at.
	// The flags field holds one flag and no others.
	flags := smb2.Header(msg).CompressionFlags()
	if flags&^smb2.COMPRESSION_CAPABILITIES_FLAG_CHAINED != 0 {
		return nil, smb2.ErrInvalidParameter
	}
	chained := flags == smb2.COMPRESSION_CAPABILITIES_FLAG_CHAINED
	if chained && !c.supportsChainedCompression {
		return nil, smb2.ErrInvalidParameter
	}

	var output []byte
	start := 0
	if chained {
		offset := smb2.SMB2CompressionPayloadHeaderOffset
		for {
			if offset == len(msg) {
				break
			}

			if offset+smb2.SMB2CompressionPayloadHeaderSize > len(msg) {
				return nil, smb2.ErrWrongFormat
			}

			ph := smb2.PayloadHeader(msg[offset:])
			algo := ph.CompressionAlgorithm()
			if algo != smb2.COMPRESSION_NONE && !slices.Contains(c.compressionIDs, algo) {
				return nil, smb2.ErrInvalidParameter
			}

			length := ph.Length()
			if offset+smb2.SMB2CompressionPayloadHeaderSize+int(length) > len(msg) {
				return nil, smb2.ErrInvalidParameter
			}

			switch algo {
			case smb2.COMPRESSION_NONE:
				if int(length) > len(msg)-(offset+smb2.SMB2CompressionPayloadHeaderSize) || length > ocss {
					return nil, smb2.ErrInvalidParameter
				}
				output = append(output, msg[offset+smb2.SMB2CompressionPayloadHeaderSize:offset+smb2.SMB2CompressionPayloadHeaderSize+int(length)]...)
				if uint64(len(output)) > uint64(ocss) {
					return nil, smb2.ErrInvalidParameter
				}

			case smb2.COMPRESSION_PATTERN_V1:
				var v1 smb2.PatternV1
				if err := v1.Unmarshal(ph[smb2.SMB2CompressionPayloadHeaderSize:]); err != nil {
					return nil, err
				}
				if v1.Repetitions > ocss {
					return nil, smb2.ErrInvalidParameter
				}
				chunk := make([]byte, v1.Repetitions)
				for i := range v1.Repetitions {
					chunk[i] = v1.Pattern
				}
				output = append(output, chunk...)
				if uint64(len(output)) > uint64(ocss) {
					return nil, smb2.ErrInvalidParameter
				}

			default:
				compressor := compress.New(algo)
				ops := binary.LittleEndian.Uint32(ph[smb2.SMB2CompressionPayloadHeaderSize : smb2.SMB2CompressionPayloadHeaderSize+4])
				chunk, err := compressor.Decompress(ph[smb2.SMB2CompressionPayloadHeaderSize+4:smb2.SMB2CompressionPayloadHeaderSize+length], int(ocss))
				if err != nil {
					return nil, err
				}
				if uint32(len(chunk)) != ops {
					return nil, smb2.ErrWrongLength
				}
				output = append(output, chunk...)
				if uint64(len(output)) > uint64(ocss) {
					return nil, smb2.ErrInvalidParameter
				}
			}

			offset += smb2.SMB2CompressionPayloadHeaderSize + int(length)
		}
	} else {
		// The offset says how much of the message in front of the compressed segment is to be taken
		// as it stands, and it is a field a peer fills in. The head it names has to lie inside the
		// message with the transform header counted in: an offset that only fits when the header is
		// left out of the sum still leaves the slice below reaching past the end of what arrived.
		start = int(smb2.Header(msg).Offset())
		if uint64(smb2.SMB2CompressionTransformHeaderSize)+uint64(start) > uint64(len(msg)) {
			return nil, smb2.ErrInvalidParameter
		}

		if start > 0 {
			output = append(output, msg[smb2.SMB2CompressionTransformHeaderSize:smb2.SMB2CompressionTransformHeaderSize+start]...)
		}

		algo := smb2.Header(msg).CompressionAlgorithm()
		if !slices.Contains(c.compressionIDs, algo) {
			return nil, smb2.ErrInvalidParameter
		}

		var buf []byte
		switch algo {
		case smb2.COMPRESSION_PATTERN_V1:
			var v1 smb2.PatternV1
			if err := v1.Unmarshal(msg[smb2.SMB2CompressionTransformHeaderSize+start:]); err != nil {
				return nil, err
			}

			// The run is as long as the message says, and the message says so in a thirty-two
			// bit field. Room for it is taken before anything is looked at, so eight bytes
			// asking for the largest value the field holds is four gigabytes claimed on the
			// strength of a message from a peer that has not authenticated. The size of the
			// whole segment was checked against what this connection allows, and no run inside
			// it can be longer than that; the chained path already turns this away here.
			if v1.Repetitions > ocss {
				return nil, smb2.ErrInvalidParameter
			}

			buf = make([]byte, v1.Repetitions)
			for i := range v1.Repetitions {
				buf[i] = v1.Pattern
			}

		default:
			compressor := compress.New(algo)
			var err error
			buf, err = compressor.Decompress(msg[smb2.SMB2CompressionTransformHeaderSize+start:], int(ocss))
			if err != nil {
				return nil, err
			}
		}

		output = append(output, buf...)
	}

	if uint32(len(output)-start) != ocss {
		return nil, smb2.ErrWrongLength
	}

	if smb2.Header(output).ProtocolID() != smb2.PROTOCOL_SMB2 {
		return nil, smb2.ErrWrongProtocol
	}

	return output, nil
}

// compressOffset is how much of a message travels uncompressed in front of the compressed segment.
//
// For a read response it is the head of the message: the SMB2 header, the response structure, and
// whatever padding the client asked the data to begin at. Only the data behind it is compressed, so
// the segment is exactly the bytes the read asked for and nothing besides.
//
// This is what a Windows server sends — a compressed read response of its own leaves the first
// eighty bytes in the clear — and the reason to do the same is what the client on the other end
// does with what arrives. It posted a buffer for the read before it sent the read, and the segment
// is decompressed into that buffer. A segment carrying the head of the message as well as the data
// is eighty bytes longer than the read it answers, which is a segment that does not fit where it is
// going, however well formed it is.
func compressOffset(msg []byte) int {
	if len(msg) < smb2.SMB2HeaderSize+smb2.SMB2ReadResponseMinSize {
		return 0
	}

	h := smb2.Header(msg)
	if h.Command() != smb2.SMB2_READ || h.Status() != smb2.STATUS_OK || h.NextCommand() != 0 {
		return 0
	}

	// DataOffset of the read response, which is where the data the client asked for begins.
	off := int(msg[smb2.SMB2HeaderSize+2])
	if off < smb2.SMB2HeaderSize+smb2.SMB2ReadResponseMinSize || off >= len(msg) {
		return 0
	}

	return off
}

// compress compresses the message before encrypting and putting on the wire.
//
// What goes out is one segment under the unchained transform header of [MS-SMB2] 2.2.42.1, whatever
// framing the peer offered. The unchained header is the only one with an Offset field, and the
// offset is the point of it: the head of a read response has to travel uncompressed, and a chained
// message has no way to say so except by carrying the head in a payload of its own. That would make
// two payloads, and the walk over a chain in [MS-SMB2] 3.1.5.3 counts only the Length field of each
// payload against the data it has left, never the eight bytes of the payload header in front of it,
// so every payload past the first leaves eight bytes over: the walk goes round again and reads a
// payload header from past the end of the message.
//
// Nothing is lost by leaving the chain alone. The pattern payload it exists for carries a run of
// equal bytes in eight bytes, which is worth almost nothing on top of what the compression already
// does with a run — LZ77 carries a megabyte of one byte in a few dozen.
//
// The receiving side keeps taking chains of any length, pattern payloads included: it is not for
// this end to decide what a peer may send.
func (c *connection) compress(msg []byte) []byte {
	if !c.server.compressionSupported || len(c.compressionIDs) == 0 {
		return msg
	}

	// A pattern payload cannot carry a message that is not all one byte, so there has to be an
	// algorithm among what the peer offered to compress with.
	algo := uint16(smb2.COMPRESSION_NONE)
	for _, id := range c.compressionIDs {
		if id != smb2.COMPRESSION_PATTERN_V1 {
			algo = id
			break
		}
	}
	if algo == smb2.COMPRESSION_NONE {
		return msg
	}

	offset := compressOffset(msg)

	// Nothing under minCompressedSegment is compressed at all.
	if len(msg)-offset < minCompressedSegment {
		return msg
	}

	buf, err := compress.New(algo).Compress(msg[offset:])
	if err != nil {
		log.Printf("compression error: %v", err) // shouldn't happen
		return msg
	}

	// Nothing came back to send, which is what an algorithm this build does not implement answers
	// with. Framing it would claim a segment that decompresses to the tail of the message and hold
	// none of it.
	if len(buf) == 0 {
		return msg
	}

	// The whole of what would go out, head and header included, has to be shorter than the message
	// it replaces.
	if smb2.SMB2CompressionTransformHeaderSize+offset+len(buf) >= len(msg) {
		return msg
	}

	h := smb2.Header(make([]byte, smb2.SMB2CompressionTransformHeaderSize))
	h.SetProtocolID(smb2.PROTOCOL_SMB2_COMPRESSED)

	// The size the segment comes apart to, which is the part of the message that was compressed and
	// not the whole of it. [MS-SMB2] 3.1.4.4: "Set OriginalCompressedSegmentSize to the uncompressed
	// length, in bytes, of the portion of the message that is being compressed."
	h.SetOriginalCompressedSegmentSize(uint32(len(msg) - offset))
	h.SetCompressionAlgorithm(algo)
	h.SetOffset(uint32(offset))

	output := append([]byte(h), msg[:offset]...)

	return append(output, buf...)
}
