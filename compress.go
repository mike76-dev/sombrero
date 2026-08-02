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

	var output []byte
	start := 0
	chained := c.supportsChainedCompression && smb2.Header(msg).CompressionFlags() == smb2.COMPRESSION_CAPABILITIES_FLAG_CHAINED
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
			}

			offset += smb2.SMB2CompressionPayloadHeaderSize + int(length)
		}
	} else {
		start = int(smb2.Header(msg).Offset())
		if start > len(msg) {
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

// compress compresses the message before encrypting and putting on the wire,
func (c *connection) compress(msg []byte) []byte {
	if !c.server.compressionSupported || len(c.compressionIDs) == 0 {
		return msg
	}

	if c.supportsChainedCompression {
		remainingBytes := len(msg)
		var output []byte
		start, end := 0, len(msg)
		first := true

		var fwd, bck *smb2.PatternV1
		if remainingBytes > 32 && slices.Contains(c.compressionIDs, smb2.COMPRESSION_PATTERN_V1) {
			fwd, bck = compress.ScanForDataPatternsV1(msg)
			if fwd.Repetitions > 0 {
				ph := smb2.PayloadHeader(make([]byte, smb2.SMB2CompressionPayloadHeaderSize))
				ph.SetCompressionAlgorithm(smb2.COMPRESSION_PATTERN_V1)
				ph.SetLength(8)
				if first {
					ph.SetFlags(smb2.COMPRESSION_CAPABILITIES_FLAG_CHAINED)
					first = false
				}
				output = append(output, ph...)
				output = append(output, fwd.Marshal()...)
				start += int(fwd.Repetitions)
				remainingBytes -= int(fwd.Repetitions)
			}

			// The run at the end leaves the payload whenever a pattern is going to carry it,
			// which is not only when there is a run at the start as well. A real message opens
			// with the protocol ID, so the one at the start almost never fires; taking the end out
			// of the payload only then would send it twice, and the message would come apart
			// longer than the header beside it says it should.
			if bck != nil && bck.Repetitions > 0 {
				remainingBytes -= int(bck.Repetitions)
				end -= int(bck.Repetitions)
			}
		}

		if remainingBytes > 1024 {
			ph := smb2.PayloadHeader(make([]byte, smb2.SMB2CompressionPayloadHeaderSize))
			algo := uint16(smb2.COMPRESSION_NONE)
			for _, id := range c.compressionIDs {
				if id != smb2.COMPRESSION_PATTERN_V1 {
					algo = id
					break
				}
			}

			compressor := compress.New(uint16(algo))
			buf, err := compressor.Compress(msg[start:end])
			if err != nil {
				log.Printf("compression error: %v", err) // shouldn't happen
			}

			length := len(buf)
			if algo != smb2.COMPRESSION_NONE {
				length += 4
			}

			ph.SetCompressionAlgorithm(uint16(algo))
			ph.SetLength(uint32(length))
			if first {
				ph.SetFlags(smb2.COMPRESSION_CAPABILITIES_FLAG_CHAINED)
				first = false
			}

			output = append(output, ph...)
			if algo != smb2.COMPRESSION_NONE {
				output = binary.LittleEndian.AppendUint32(output, uint32(end-start))
			}

			output = append(output, buf...)
			remainingBytes -= len(msg[start:end])
		} else {
			ph := smb2.PayloadHeader(make([]byte, smb2.SMB2CompressionPayloadHeaderSize))
			ph.SetCompressionAlgorithm(smb2.COMPRESSION_NONE)
			ph.SetLength(uint32(remainingBytes))
			if first {
				ph.SetFlags(smb2.COMPRESSION_CAPABILITIES_FLAG_CHAINED)
				first = false
			}

			output = append(output, ph...)
			output = append(output, msg[start:end]...)
			remainingBytes -= (end - start)
		}

		if bck != nil && bck.Repetitions > 0 {
			ph := smb2.PayloadHeader(make([]byte, smb2.SMB2CompressionPayloadHeaderSize))
			ph.SetCompressionAlgorithm(smb2.COMPRESSION_PATTERN_V1)
			ph.SetLength(8)
			if first {
				ph.SetFlags(smb2.COMPRESSION_CAPABILITIES_FLAG_CHAINED)
				first = false
			}
			output = append(output, ph...)
			output = append(output, bck.Marshal()...)
		}

		if len(output)+8 < len(msg) {
			h := smb2.Header(make([]byte, smb2.SMB2CompressionTransformHeaderSize-smb2.SMB2CompressionPayloadHeaderSize))
			h.SetProtocolID(smb2.PROTOCOL_SMB2_COMPRESSED)
			h.SetOriginalCompressedSegmentSize(uint32(len(msg)))
			output = append(h, output...)
			return output
		} else {
			return msg
		}
	} else {
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

		compressor := compress.New(uint16(algo))
		output, err := compressor.Compress(msg)
		if err != nil {
			log.Printf("compression error: %v", err) // shouldn't happen
		}

		if len(output) < len(msg) {
			h := smb2.Header(make([]byte, smb2.SMB2CompressionTransformHeaderSize))
			h.SetProtocolID(smb2.PROTOCOL_SMB2_COMPRESSED)
			h.SetOriginalCompressedSegmentSize(uint32(len(msg)))
			h.SetCompressionAlgorithm(uint16(algo))
			return append(h, output...)
		} else {
			return msg
		}
	}
}
