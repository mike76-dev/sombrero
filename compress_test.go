package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/rand"
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
// decompression is only accepted if it is an SMB2 message, and it has to carry more than
// minCompressedSegment, or it is not compressed at all.
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

			msg := compressibleMessage(8192)
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

// TestCompressionIsNeverChained is the connection whose peer offered chaining and the pattern
// payload, and still gets the unchained header.
//
// Two reasons, and the first is the one the read responses turn on. The unchained header is the only
// one with an Offset field, and the head of a read response has to travel uncompressed; a chained
// message can only say that by carrying the head in a payload of its own, which makes two payloads.
// The walk over a chain in [MS-SMB2] 3.1.5.3 then comes apart: it counts only the Length field of
// each payload against the data it has left, never the eight bytes of the payload header in front of
// it, so every payload past the first leaves eight bytes over and the walk goes round again to read a
// payload header from past the end of the message. The second reason is that the pattern payload the
// chain exists for is worth almost nothing on top of the compression, which carries a run of a
// megabyte in a few dozen bytes.
func TestCompressionIsNeverChained(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(true, smb2.COMPRESSION_PATTERN_V1, smb2.COMPRESSION_LZ77)

	// A message with a run at the end, which is what a pattern payload used to be spent on.
	msg := writeRequest(1, 1, 1, make([]byte, 16), 0,
		append(bytes.Repeat([]byte("sombrero "), 8192), make([]byte, 4096)...))

	buf := compressedWith(t, c.compress(msg), msg, smb2.COMPRESSION_LZ77)

	// The flags of the transform header are where a peer reads whether the message is chained, and
	// they lie where the first payload header of a chained message keeps its own.
	if flags := smb2.Header(buf).CompressionFlags(); flags != smb2.COMPRESSION_CAPABILITIES_FLAG_NONE {
		t.Errorf("the message carries flags %#04x, want none of them: a chained message is read differently", flags)
	}

	got, err := c.decompress(buf)
	if err != nil {
		t.Fatalf("the message did not come back: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("what came back is %d bytes long, want the %d that went in", len(got), len(msg))
	}
}

// TestCompressionLeavesTheHeadOfAReadResponseAlone is the shape of a compressed read response: the
// SMB2 header, the response structure and the padding in front of the data travel as they are, and
// only the data the read asked for is compressed.
//
// The size of the segment is what this is about. A client posts a buffer for the read before it sends
// the read and decompresses the segment into it, so a segment that carries the eighty-byte head of
// the message as well as the data comes apart to eighty bytes more than the read asked for. A
// Windows server sends the head in the clear for the same reason, and a download of a compressible
// file stops on the first response that does not.
func TestCompressionLeavesTheHeadOfAReadResponseAlone(t *testing.T) {
	h := newSMBTest(t)
	c := h.compressing(true, smb2.COMPRESSION_PATTERN_V1, smb2.COMPRESSION_LZ77)

	data := bytes.Repeat([]byte("sombrero rides again "), 4096)
	msg := readResponse(t, data)
	head := smb2.SMB2HeaderSize + smb2.SMB2ReadResponseMinSize
	if len(msg) != head+len(data) {
		t.Fatalf("the response is %d bytes long, want the %d of a head and %d of data", len(msg), head, len(data))
	}

	buf := c.compress(msg)
	if len(buf) >= len(msg) {
		t.Fatalf("the response came back %d bytes long, no shorter than the %d it went in as", len(buf), len(msg))
	}

	if off := int(smb2.Header(buf).Offset()); off != head {
		t.Errorf("the header says %d bytes travel uncompressed, want the %d of the head", off, head)
	}
	if ocss := smb2.Header(buf).OriginalCompressedSegmentSize(); ocss != uint32(len(data)) {
		t.Errorf("the segment comes apart to %d bytes, want the %d of the data alone", ocss, len(data))
	}
	if !bytes.Equal(buf[smb2.SMB2CompressionTransformHeaderSize:smb2.SMB2CompressionTransformHeaderSize+head], msg[:head]) {
		t.Error("the head of the response did not travel as it stands")
	}

	got, err := c.decompress(buf)
	if err != nil {
		t.Fatalf("the response did not come back: %v", err)
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
	msg := compressibleMessage(8192)

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

// TestCompressionIsSkippedWithOnlyPatternsToSend is the client that offered nothing but the pattern
// payload. A pattern carries a run of one byte and nothing else, so there is no algorithm here to
// compress a message with, chained or not. The message has to go as it is: a payload header naming
// an algorithm this end cannot run would claim to hold the message and hold nothing.
func TestCompressionIsSkippedWithOnlyPatternsToSend(t *testing.T) {
	h := newSMBTest(t)

	for _, chained := range []bool{false, true} {
		c := h.compressing(chained, smb2.COMPRESSION_PATTERN_V1)

		msg := compressibleMessage(8192)
		if buf := c.compress(msg); !bytes.Equal(buf, msg) {
			t.Errorf("chained=%v: the message was rewritten, want it left alone", chained)
		}
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

	msg := compressibleMessage(8192)
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
	msg := compressibleMessage(8192)
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

	// The offset that fits the message only when the transform header is left out of the sum. The
	// head it names begins after that header, so the bytes it asks to be taken as they stand run
	// past the end of what arrived — and the slice that takes them is what the server would panic
	// in, on a message from whoever is on the far end.
	for _, payload := range [][]byte{make([]byte, 64), make([]byte, 8), nil} {
		msg = unchained(smb2.COMPRESSION_LZ77, 64, uint32(len(payload)+1), payload)
		if _, err := c.decompress(msg); !errors.Is(err, smb2.ErrInvalidParameter) {
			t.Errorf("a message of %d bytes with an offset of %d was answered with %v, want it refused",
				len(msg), len(payload)+1, err)
		}
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

	msg := compressibleMessage(8192)
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

// readResponse is the response the server sends for a read that came back with the given data. It
// is the message a download is made of, and the one whose compression a client refused.
func readResponse(t *testing.T, data []byte) []byte {
	t.Helper()

	reqs, err := smb2.GetRequests(readRequest(1, 1, 1, make([]byte, 16), 0, uint32(len(data))), 0, 0, false)
	if err != nil || len(reqs) != 1 {
		t.Fatalf("could not build the read request: %v", err)
	}

	resp := &smb2.ReadResponse{}
	resp.FromRequest(smb2.ReadRequest{Request: *reqs[0]})
	resp.Generate(data, smb2.SMB2HeaderSize+smb2.SMB2ReadResponseMinSize)

	return resp.Encode()
}

// TestCompressionOfEveryShapeOfRead sends read responses of every shape through the compressor,
// because what a message ends in used to decide how many payloads it came out as, and only one of
// those shapes is one a client reads. The payload counts are what this checks; the round trip is
// only there to say that the one payload holds everything.
func TestCompressionOfEveryShapeOfRead(t *testing.T) {
	h := newSMBTest(t)
	rnd := rand.New(rand.NewSource(7))

	// Data made of runs and blocks, so that the runs at the ends and the lengths a pattern payload
	// used to be worth are all hit sooner or later.
	data := func(n int) []byte {
		b := make([]byte, 0, n)
		for len(b) < n {
			switch rnd.Intn(4) {
			case 0:
				b = append(b, bytes.Repeat([]byte{byte(rnd.Intn(256))}, rnd.Intn(200)+1)...)
			case 1:
				b = append(b, bytes.Repeat([]byte{byte(rnd.Intn(256))}, rnd.Intn(70000))...)
			case 2:
				block := make([]byte, rnd.Intn(2000))
				rnd.Read(block)
				b = append(b, block...)
			case 3:
				b = append(b, bytes.Repeat([]byte("sombrero "), rnd.Intn(300))...)
			}
		}
		return b[:n]
	}

	// The sizes around the thresholds the framing turns on, and then whatever comes up.
	sizes := []int{0, 1, 8, 31, 32, 33, 63, 64, 65, 100, 943, 944, 945, 1023, 1024, 1025,
		2048, 4096, 65535, 65536, 65537, 100000}

	for i := range 200 {
		var buf []byte
		if i < len(sizes) {
			buf = data(sizes[i])
		} else {
			buf = data(rnd.Intn(200000))
		}

		// Half of them end in a run, which is the case that used to come out as two payloads.
		if rnd.Intn(2) == 0 {
			buf = append(buf, bytes.Repeat([]byte{byte(rnd.Intn(256))}, rnd.Intn(130))...)
		}

		msg := readResponse(t, buf)

		for _, chained := range []bool{true, false} {
			c := h.compressing(chained, smb2.COMPRESSION_PATTERN_V1, smb2.COMPRESSION_LZ77)

			out := c.compress(msg)
			if bytes.Equal(out, msg) {
				continue // went as it was
			}
			if len(out) >= len(msg) {
				t.Fatalf("case %d chained=%v: %d bytes of data came out as %d, want fewer than the %d it went in as",
					i, chained, len(buf), len(out), len(msg))
			}

			// The head travels uncompressed and the segment comes apart to exactly the data the
			// read asked for, whatever the data looks like and whichever framing the peer offered.
			head := smb2.SMB2HeaderSize + smb2.SMB2ReadResponseMinSize
			if off := int(smb2.Header(out).Offset()); off != head {
				t.Fatalf("case %d chained=%v: %d bytes of data came out with offset %d, want the %d of the head",
					i, chained, len(buf), off, head)
			}
			if ocss := smb2.Header(out).OriginalCompressedSegmentSize(); ocss != uint32(len(msg)-head) {
				t.Fatalf("case %d chained=%v: the segment comes apart to %d bytes, want the %d of the data",
					i, chained, ocss, len(msg)-head)
			}
			if flags := smb2.Header(out).CompressionFlags(); flags != smb2.COMPRESSION_CAPABILITIES_FLAG_NONE {
				t.Fatalf("case %d chained=%v: the message carries flags %#04x, want none", i, chained, flags)
			}

			got, err := c.decompress(out)
			if err != nil {
				t.Fatalf("case %d chained=%v: %d bytes of data did not come back: %v", i, chained, len(buf), err)
			}
			if !bytes.Equal(got, msg) {
				t.Fatalf("case %d chained=%v: %d bytes of data came back as %d bytes of message, want %d",
					i, chained, len(buf), len(got), len(msg))
			}
		}
	}
}

// TestCompressionLeavesSmallMessagesAlone is the message with too little in it to compress, which a
// client will not take compressed however well formed it is. See minCompressedSegment.
//
// A read of a single page is what this is for. The tail of a package holds its directory, a few
// kilobytes of repeated names that compress three to one, and a client reads it a page at a time.
func TestCompressionLeavesSmallMessagesAlone(t *testing.T) {
	h := newSMBTest(t)

	for _, chained := range []bool{false, true} {
		c := h.compressing(chained, smb2.COMPRESSION_PATTERN_V1, smb2.COMPRESSION_LZ77)

		// Read responses of a page and of two, and a message that is nothing but a header and a
		// structure. All three are as compressible as anything the server sends.
		for _, msg := range [][]byte{
			readResponse(t, bytes.Repeat([]byte("sombrero "), 4096/9)),
			readResponse(t, bytes.Repeat([]byte("sombrero "), 8192/9)),
			writeRequest(1, 1, 1, make([]byte, 16), 0, nil),
		} {
			if buf := c.compress(msg); !bytes.Equal(buf, msg) {
				t.Errorf("chained=%v: a message of %d bytes came out as %d, want it left alone",
					chained, len(msg), len(buf))
			}
		}

		// One byte over the floor is compressed, so the floor is what leaves the others alone and
		// not something else about them.
		msg := readResponse(t, bytes.Repeat([]byte("sombrero "), minCompressedSegment/9+1))
		if buf := c.compress(msg); bytes.Equal(buf, msg) {
			t.Errorf("chained=%v: a message of %d bytes was left alone, want it compressed", chained, len(msg))
		}
	}
}
