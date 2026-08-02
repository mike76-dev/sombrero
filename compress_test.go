package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/mike76-dev/sombrero/compress"
	"github.com/mike76-dev/sombrero/smb2"
)

// squeeze compresses a buffer under the named algorithm, for the tests that frame a payload by
// hand rather than letting the connection do it.
func squeeze(t *testing.T, algo uint16, src []byte) []byte {
	t.Helper()

	buf, err := compress.New(algo).Compress(src)
	if err != nil {
		t.Fatalf("could not compress under algorithm %d: %v", algo, err)
	}

	return buf
}

// compressing brings up a connection that has settled on compression, the way a negotiate leaves
// one whose client offered the compression capabilities context. The algorithms are given in the
// order the client named them, which is the order the server picks from.
func (h *smbTest) compressing(chained bool, algos ...uint16) *connection {
	h.t.Helper()

	h.srv.compressionSupported = true

	c := h.newTestConnection("compressing")
	c.compressionIDs = algos
	c.supportsChainedCompression = chained

	return c
}

// compressibleMessage builds a real SMB2 message worth compressing: a write request carrying the
// same short phrase over and over. It has to be a real one, because what comes out of the
// decompression is only accepted if it is an SMB2 message.
func compressibleMessage(repeats int) []byte {
	return writeRequest(1, 1, 1, make([]byte, 16), 0, bytes.Repeat([]byte("sombrero "), repeats))
}

// unchained frames a payload under an SMB2_COMPRESSION_TRANSFORM_HEADER, the way a client that
// did not negotiate chaining sends one.
func unchained(algo uint16, ocss, offset uint32, payload []byte) []byte {
	h := smb2.Header(make([]byte, smb2.SMB2CompressionTransformHeaderSize))
	h.SetProtocolID(smb2.PROTOCOL_SMB2_COMPRESSED)
	h.SetOriginalCompressedSegmentSize(ocss)
	h.SetCompressionAlgorithm(algo)
	h.SetOffset(offset)

	return append([]byte(h), payload...)
}

// chainedPayload frames one SMB2_COMPRESSION_CHAINED_PAYLOAD_HEADER and the data behind it.
func chainedPayload(algo, flags uint16, data []byte) []byte {
	ph := smb2.PayloadHeader(make([]byte, smb2.SMB2CompressionPayloadHeaderSize))
	ph.SetCompressionAlgorithm(algo)
	ph.SetFlags(flags)
	ph.SetLength(uint32(len(data)))

	return append([]byte(ph), data...)
}

// chainedMessage frames chained payloads under the eight-byte head of the transform header.
//
// The flags that say the message is chained are read out of where the unchained header keeps its
// own, which in a chained message is inside the first payload header rather than the head. So the
// first payload is the one that has to carry the flag, and the head is only eight bytes long.
func chainedMessage(ocss uint32, payloads ...[]byte) []byte {
	h := smb2.Header(make([]byte, smb2.SMB2CompressionPayloadHeaderOffset))
	h.SetProtocolID(smb2.PROTOCOL_SMB2_COMPRESSED)
	h.SetOriginalCompressedSegmentSize(ocss)

	msg := []byte(h)
	for _, payload := range payloads {
		msg = append(msg, payload...)
	}

	return msg
}

// compressedWith fails the test unless the message came back compressed under the named
// algorithm, and returns it.
func compressedWith(t *testing.T, buf, msg []byte, algo uint16) []byte {
	t.Helper()

	if len(buf) >= len(msg) {
		t.Fatalf("the message came back %d bytes long, no shorter than the %d it went in as", len(buf), len(msg))
	}
	if id := smb2.Header(buf).ProtocolID(); id != smb2.PROTOCOL_SMB2_COMPRESSED {
		t.Fatalf("the message carries protocol ID %#x, want a compressed one", id)
	}
	if got := smb2.Header(buf).CompressionAlgorithm(); got != algo {
		t.Fatalf("the message is marked as algorithm %d, want %d", got, algo)
	}
	if got := smb2.Header(buf).OriginalCompressedSegmentSize(); got != uint32(len(msg)) {
		t.Fatalf("the header says the message was %d bytes long, want %d", got, len(msg))
	}

	return buf
}

// TestCompressionRoundTrip walks a message through every algorithm the server compresses with.
// What one connection sends is what the other has to be able to read back.
func TestCompressionRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name string
		algo uint16
	}{
		{"LZNT1", smb2.COMPRESSION_LZNT1},
		{"LZ77", smb2.COMPRESSION_LZ77},
		{"LZ77+Huffman", smb2.COMPRESSION_LZ77_HUFFMAN},
		{"LZ4", smb2.COMPRESSION_LZ4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			c := h.compressing(false, tt.algo)

			msg := compressibleMessage(256)
			buf := compressedWith(t, c.compress(msg), msg, tt.algo)

			got, err := c.decompress(buf)
			if err != nil {
				t.Fatalf("the message did not come back: %v", err)
			}
			if !bytes.Equal(got, msg) {
				t.Fatal("what came back is not what went in")
			}
		})
	}
}

// TestChainedCompressionRoundTrip is the same walk over a connection that negotiated chaining, so
// that the payload travels behind a chained payload header rather than the unchained one.
func TestChainedCompressionRoundTrip(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(true, smb2.COMPRESSION_LZ77)

	msg := compressibleMessage(256)
	buf := c.compress(msg)
	if len(buf) >= len(msg) {
		t.Fatalf("the message came back %d bytes long, no shorter than the %d it went in as", len(buf), len(msg))
	}

	got, err := c.decompress(buf)
	if err != nil {
		t.Fatalf("the message did not come back: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("what came back is not what went in")
	}
}

// TestChainedCompressionSendsATrailingRunOnce is the run of equal bytes at the end of a message,
// which the pattern payload is there to carry in eight bytes rather than in as many as the run is
// long.
//
// The run has to leave the compressed payload when the pattern takes it over. A message that
// carries it in both comes apart longer than it went in, which is the one thing the peer will not
// take: the size it decompresses to is written in the header beside it.
func TestChainedCompressionSendsATrailingRunOnce(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(true, smb2.COMPRESSION_PATTERN_V1, smb2.COMPRESSION_LZ77)

	// A real message never opens with a run — it opens with the protocol ID — so the run at the
	// end is the one a pattern payload gets to carry.
	msg := writeRequest(1, 1, 1, make([]byte, 16), 0,
		append(bytes.Repeat([]byte("sombrero "), 256), make([]byte, 512)...))

	buf := c.compress(msg)
	if len(buf) >= len(msg) {
		t.Fatalf("the message came back %d bytes long, no shorter than the %d it went in as", len(buf), len(msg))
	}

	got, err := c.decompress(buf)
	if err != nil {
		t.Fatalf("the message did not come back: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("what came back is %d bytes long, want the %d that went in", len(got), len(msg))
	}
}

// TestCompressionIsSkippedWhenItWouldNotHelp is the message that has nothing in it to compress.
// Sending it under a transform header would only add the header.
func TestCompressionIsSkippedWhenItWouldNotHelp(t *testing.T) {
	h := newSMBTest(t)

	for _, chained := range []bool{false, true} {
		c := h.compressing(chained, smb2.COMPRESSION_LZ77)

		// Hashes of counting numbers: as far from a pattern as anything the server will be asked
		// to send, and the same on every run.
		var data []byte
		for i := range 64 {
			sum := sha256.Sum256([]byte{byte(i)})
			data = append(data, sum[:]...)
		}

		msg := writeRequest(1, 1, 1, make([]byte, 16), 0, data)
		if buf := c.compress(msg); !bytes.Equal(buf, msg) {
			t.Errorf("chained=%v: the message was rewritten, want it left alone", chained)
		}
	}
}

// TestCompressionIsSkippedWhenNotNegotiated is what the server does when compression never came
// up: it sends what it was given.
func TestCompressionIsSkippedWhenNotNegotiated(t *testing.T) {
	h := newSMBTest(t)
	msg := compressibleMessage(256)

	c := h.compressing(false, smb2.COMPRESSION_LZ77)
	h.srv.compressionSupported = false
	if buf := c.compress(msg); !bytes.Equal(buf, msg) {
		t.Error("the server compressed although it does not support compression")
	}

	h.srv.compressionSupported = true
	c.compressionIDs = nil
	if buf := c.compress(msg); !bytes.Equal(buf, msg) {
		t.Error("the server compressed although the client named no algorithm")
	}
}

// TestCompressionIsSkippedWithOnlyPatternsToSendUnchained is the client that offered nothing but
// the pattern payload. A pattern is a chained payload; there is no unchained message to put one
// in, so an unchained connection has nothing left to compress with.
func TestCompressionIsSkippedWithOnlyPatternsToSendUnchained(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(false, smb2.COMPRESSION_PATTERN_V1)

	msg := compressibleMessage(256)
	if buf := c.compress(msg); !bytes.Equal(buf, msg) {
		t.Error("the message was rewritten, want it left alone")
	}
}

// TestDecompressPatternV1 is the pattern payload arriving unchained, behind a prefix the header
// says to take as it stands. The prefix is what makes the message an SMB2 one; the run behind it
// is what the eight bytes of the payload stand for.
func TestDecompressPatternV1(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(false, smb2.COMPRESSION_PATTERN_V1)

	prefix := make([]byte, smb2.SMB2HeaderSize)
	smb2.NewHeader(prefix)

	pattern := smb2.PatternV1{Pattern: 0x7e, Repetitions: 4096}
	msg := unchained(smb2.COMPRESSION_PATTERN_V1, pattern.Repetitions, uint32(len(prefix)),
		append(prefix, pattern.Marshal()...))

	got, err := c.decompress(msg)
	if err != nil {
		t.Fatalf("the message did not come apart: %v", err)
	}

	want := append(prefix, bytes.Repeat([]byte{pattern.Pattern}, int(pattern.Repetitions))...)
	if !bytes.Equal(got, want) {
		t.Fatalf("the pattern came apart as %d bytes, want %d", len(got), len(want))
	}
}

// TestDecompressChainedPayloads is a message whose payloads are of different kinds, which is what
// chaining is for: the part that compressed goes under one algorithm, the run goes under a
// pattern, and the part that is worth neither travels as it stands.
func TestDecompressChainedPayloads(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(true, smb2.COMPRESSION_LZ77, smb2.COMPRESSION_PATTERN_V1)

	head := make([]byte, smb2.SMB2HeaderSize)
	smb2.NewHeader(head)

	body := bytes.Repeat([]byte("sombrero "), 128)
	squeezed := squeeze(t, smb2.COMPRESSION_LZ77, body)

	pattern := smb2.PatternV1{Pattern: 0, Repetitions: 256}
	want := append(append(head, body...), bytes.Repeat([]byte{pattern.Pattern}, int(pattern.Repetitions))...)

	// A compressed payload leads with the size it comes apart into, which the pattern and the
	// untouched payloads have no use for.
	msg := chainedMessage(uint32(len(want)),
		chainedPayload(smb2.COMPRESSION_NONE, smb2.COMPRESSION_CAPABILITIES_FLAG_CHAINED, head),
		chainedPayload(smb2.COMPRESSION_LZ77, 0,
			append(binary.LittleEndian.AppendUint32(nil, uint32(len(body))), squeezed...)),
		chainedPayload(smb2.COMPRESSION_PATTERN_V1, 0, pattern.Marshal()),
	)

	got, err := c.decompress(msg)
	if err != nil {
		t.Fatalf("the message did not come apart: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the message came apart as %d bytes, want %d", len(got), len(want))
	}
}

// TestDecompressRefusesWhatWasNotNegotiated is the compressed message nobody agreed to send. A
// connection that never settled on compression has no algorithm to read it with.
func TestDecompressRefusesWhatWasNotNegotiated(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(false, smb2.COMPRESSION_LZ77)

	msg := compressibleMessage(256)
	buf := c.compress(msg)

	h.srv.compressionSupported = false
	if _, err := c.decompress(buf); !errors.Is(err, smb2.ErrCompressedMessage) {
		t.Errorf("the server answered %v, want it to refuse compression it does not support", err)
	}

	h.srv.compressionSupported = true
	c.compressionIDs = nil
	if _, err := c.decompress(buf); !errors.Is(err, smb2.ErrCompressedMessage) {
		t.Errorf("the server answered %v, want it to refuse an algorithm nobody named", err)
	}
}

// TestDecompressRefusesAShortMessage is the message that stops before the header it needs to be
// read with is over.
func TestDecompressRefusesAShortMessage(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(false, smb2.COMPRESSION_LZ77)

	msg := make([]byte, smb2.SMB2CompressionTransformHeaderSize-1)
	if _, err := c.decompress(msg); !errors.Is(err, smb2.ErrWrongLength) {
		t.Errorf("the server answered %v, want it to refuse a message shorter than the header", err)
	}
}

// TestDecompressRefusesAnOversizeSegment is the header that claims more than the connection ever
// agreed to carry. It is refused on what it says rather than on what it holds, so that nothing is
// set aside for it first.
func TestDecompressRefusesAnOversizeSegment(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(false, smb2.COMPRESSION_LZ77)

	msg := unchained(smb2.COMPRESSION_LZ77, ^uint32(0), 0, []byte{0})
	if _, err := c.decompress(msg); !errors.Is(err, smb2.ErrInvalidParameter) {
		t.Errorf("the server answered %v, want it to refuse a segment larger than it carries", err)
	}
}

// TestDecompressRefusesAnUnnegotiatedAlgorithm is the message compressed with something the
// client never offered.
func TestDecompressRefusesAnUnnegotiatedAlgorithm(t *testing.T) {
	h := newSMBTest(t)

	sender := h.compressing(false, smb2.COMPRESSION_LZ77)
	msg := compressibleMessage(256)
	buf := sender.compress(msg)

	receiver := h.compressing(false, smb2.COMPRESSION_LZNT1)
	if _, err := receiver.decompress(buf); !errors.Is(err, smb2.ErrInvalidParameter) {
		t.Errorf("the server answered %v, want it to refuse an algorithm nobody named", err)
	}
}

// TestDecompressRefusesAnOffsetPastTheEnd is the header that points the untouched part of the
// message beyond where the message stops.
func TestDecompressRefusesAnOffsetPastTheEnd(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(false, smb2.COMPRESSION_LZ77)

	msg := unchained(smb2.COMPRESSION_LZ77, 64, 4096, []byte{0})
	if _, err := c.decompress(msg); !errors.Is(err, smb2.ErrInvalidParameter) {
		t.Errorf("the server answered %v, want it to refuse an offset past the end", err)
	}
}

// TestDecompressRefusesAChainedPayloadPastTheEnd is the payload header whose length reaches
// further than the message it arrived in.
func TestDecompressRefusesAChainedPayloadPastTheEnd(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(true, smb2.COMPRESSION_LZ77)

	payload := chainedPayload(smb2.COMPRESSION_NONE, smb2.COMPRESSION_CAPABILITIES_FLAG_CHAINED, []byte("short"))
	smb2.PayloadHeader(payload).SetLength(4096)

	msg := chainedMessage(4096, payload)
	if _, err := c.decompress(msg); !errors.Is(err, smb2.ErrInvalidParameter) {
		t.Errorf("the server answered %v, want it to refuse a payload longer than the message", err)
	}
}

// TestDecompressRefusesATruncatedChainedHeader is the message that stops in the middle of the
// header of the payload after the one just read.
func TestDecompressRefusesATruncatedChainedHeader(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(true, smb2.COMPRESSION_LZ77)

	msg := chainedMessage(64,
		chainedPayload(smb2.COMPRESSION_NONE, smb2.COMPRESSION_CAPABILITIES_FLAG_CHAINED, []byte("body")))
	msg = append(msg, 0, 0, 0)

	if _, err := c.decompress(msg); !errors.Is(err, smb2.ErrWrongFormat) {
		t.Errorf("the server answered %v, want it to refuse a payload header that stops short", err)
	}
}

// TestDecompressRefusesAWrongOriginalSize is the header whose word on how big the message
// decompresses to is not what it decompresses to. The size is what the peer sets memory aside on,
// so it is checked against the result rather than taken.
func TestDecompressRefusesAWrongOriginalSize(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(false, smb2.COMPRESSION_LZ77)

	msg := compressibleMessage(256)
	buf := c.compress(msg)
	smb2.Header(buf).SetOriginalCompressedSegmentSize(uint32(len(msg)) + 1)

	if _, err := c.decompress(buf); !errors.Is(err, smb2.ErrWrongLength) {
		t.Errorf("the server answered %v, want it to refuse a size that is not the one it got", err)
	}
}

// TestDecompressRefusesANonSMB2Payload is the message that comes apart into something that is not
// a request at all. Whatever it decompressed to, nothing further is done with it.
func TestDecompressRefusesANonSMB2Payload(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(false, smb2.COMPRESSION_LZ77)

	junk := bytes.Repeat([]byte("not a request "), 64)
	msg := unchained(smb2.COMPRESSION_LZ77, uint32(len(junk)), 0, squeeze(t, smb2.COMPRESSION_LZ77, junk))
	if _, err := c.decompress(msg); !errors.Is(err, smb2.ErrWrongProtocol) {
		t.Errorf("the server answered %v, want it to refuse a payload that is not an SMB2 message", err)
	}
}

// TestDecompressRefusesAPatternLongerThanTheSegment is eight bytes claiming four gigabytes. The
// run is as long as the payload says, and it says so in a thirty-two bit field; room for the whole
// of it was taken before anything about the message was looked at, so the largest value the field
// holds asked for four gigabytes and the process was killed for it. Nothing here has
// authenticated: compression is settled during the negotiate, and a compressed message is taken
// apart before the requests inside it are so much as read.
//
// The size of the whole segment is already checked against what the connection allows, and no run
// inside it can be longer than that. The chained path turns this away; the unchained one, which is
// the one a client reaches without negotiating chaining, did not.
func TestDecompressRefusesAPatternLongerThanTheSegment(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(false, smb2.COMPRESSION_PATTERN_V1)

	prefix := make([]byte, smb2.SMB2HeaderSize)
	smb2.NewHeader(prefix)

	for _, repetitions := range []uint32{0xffffffff, 0x7fffffff, 1 << 24, 8192} {
		// The segment size is small and inside what the connection allows, so the guard on it
		// lets the message through to the run behind it.
		pattern := smb2.PatternV1{Pattern: 0x7e, Repetitions: repetitions}
		msg := unchained(smb2.COMPRESSION_PATTERN_V1, 4096, uint32(len(prefix)),
			append(prefix, pattern.Marshal()...))

		got, err := c.decompress(msg)
		if err == nil {
			t.Errorf("a run of %d in a segment of 4096 came apart into %d bytes", repetitions, len(got))
		}
	}
}

// TestDecompressTakesAPatternThatFitsTheSegment is the control for the refusals above: a run that
// the segment size accounts for still comes apart.
func TestDecompressTakesAPatternThatFitsTheSegment(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(false, smb2.COMPRESSION_PATTERN_V1)

	prefix := make([]byte, smb2.SMB2HeaderSize)
	smb2.NewHeader(prefix)

	pattern := smb2.PatternV1{Pattern: 0x7e, Repetitions: 4096}
	msg := unchained(smb2.COMPRESSION_PATTERN_V1, pattern.Repetitions, uint32(len(prefix)),
		append(prefix, pattern.Marshal()...))

	if _, err := c.decompress(msg); err != nil {
		t.Fatalf("a run the segment accounts for would not come apart: %v", err)
	}
}
