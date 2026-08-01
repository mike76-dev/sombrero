package rpc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// bindBody builds the body of a Bind packet claiming the given number of contexts, followed by
// whatever context bytes are handed in. The count and the contexts are written separately on
// purpose: what a packet says it carries and what it actually carries are the two things a
// decoder must not confuse.
func bindBody(count uint32, contexts []byte) []byte {
	var buf []byte
	buf = binary.LittleEndian.AppendUint16(buf, 0x10b8) // max transmit fragment
	buf = binary.LittleEndian.AppendUint16(buf, 0x10b8) // max receive fragment
	buf = binary.LittleEndian.AppendUint32(buf, 0)      // association group
	buf = binary.LittleEndian.AppendUint32(buf, count)
	return append(buf, contexts...)
}

// presentationContext builds one presentation context offering the given transfer syntaxes.
func presentationContext(id uint16, syntaxes ...[]byte) []byte {
	var buf []byte
	buf = binary.LittleEndian.AppendUint16(buf, id)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(syntaxes)))
	buf = append(buf, syntax(NDR32)...) // the abstract syntax
	for _, s := range syntaxes {
		buf = append(buf, syntax(s)...)
	}
	return buf
}

// syntax builds a syntax identifier: sixteen bytes of interface and a version in two halves.
func syntax(uuid []byte) []byte {
	buf := make([]byte, 16)
	copy(buf, uuid)
	buf = binary.LittleEndian.AppendUint16(buf, 3) // major
	buf = binary.LittleEndian.AppendUint16(buf, 0) // minor
	return buf
}

// header builds a packet header of the given type in front of a body.
func header(packetType uint8, body []byte) []byte {
	var buf []byte
	buf = append(buf, 5, 0, packetType, PFC_FIRST_FRAG|PFC_LAST_FRAG)
	buf = binary.LittleEndian.AppendUint32(buf, 0x00000010)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(body)+HeaderSize))
	buf = binary.LittleEndian.AppendUint16(buf, 0) // authentication length
	buf = binary.LittleEndian.AppendUint32(buf, 42)
	return append(buf, body...)
}

// TestHeaderRoundTrip writes a header out and reads it back, which is the shape every packet in
// either direction opens with.
func TestHeaderRoundTrip(t *testing.T) {
	want := &Header{
		RPCVersionMajor:    5,
		RPCVersionMinor:    0,
		PacketType:         PACKET_TYPE_BIND,
		PacketFlags:        PFC_FIRST_FRAG | PFC_LAST_FRAG,
		DataRepresentation: 0x00000010,
		FragLength:         72,
		AuthLength:         0,
		CallID:             1,
	}

	var buf bytes.Buffer
	want.Encode(&buf)

	if buf.Len() != HeaderSize {
		t.Fatalf("the header went out %d bytes long, want %d", buf.Len(), HeaderSize)
	}

	got := &Header{}
	if err := got.Decode(&buf); err != nil {
		t.Fatalf("could not read back what was written: %v", err)
	}
	if *got != *want {
		t.Errorf("the header came back %+v, want %+v", got, want)
	}
}

// TestHeaderDecodeRefusesWhatDidNotArrive is a header cut short. Reading is allowed to hand back
// fewer bytes than were asked for without saying anything is wrong, so a decoder that takes the
// first read as the whole of it fills the rest of the fields with zeroes nobody sent — and a
// packet type of zero is a request, which the caller then reaches into the body of.
func TestHeaderDecodeRefusesWhatDidNotArrive(t *testing.T) {
	for _, n := range []int{0, 1, 8, 15} {
		h := &Header{}
		if err := h.Decode(bytes.NewReader(make([]byte, n))); err == nil {
			t.Errorf("a header of %d bytes was read as a whole one", n)
		}
	}
}

// TestHeaderDecodeWaitsForTheWholeHeader is the header arriving a byte at a time, which is what a
// stream is free to do. What is decoded must not depend on how it was delivered.
func TestHeaderDecodeWaitsForTheWholeHeader(t *testing.T) {
	whole := header(PACKET_TYPE_BIND, nil)[:HeaderSize]

	h := &Header{}
	if err := h.Decode(&dribble{data: whole}); err != nil {
		t.Fatalf("a header delivered a byte at a time would not read: %v", err)
	}

	if h.PacketType != PACKET_TYPE_BIND {
		t.Errorf("the packet type came out %d, want %d", h.PacketType, PACKET_TYPE_BIND)
	}
	if h.CallID != 42 {
		t.Errorf("the call ID came out %d, want 42", h.CallID)
	}
}

// TestInboundPacketReadsABind is the packet a client opens a pipe conversation with, read the way
// the server reads it.
func TestInboundPacketReadsABind(t *testing.T) {
	packet := header(PACKET_TYPE_BIND, bindBody(1, presentationContext(0, NDR32)))

	ip := &InboundPacket{}
	if err := ip.Read(bytes.NewReader(packet)); err != nil {
		t.Fatalf("a bind would not read: %v", err)
	}

	if ip.Header.PacketType != PACKET_TYPE_BIND {
		t.Fatalf("the packet type came out %d, want a bind", ip.Header.PacketType)
	}

	body, ok := ip.Body.(*Bind)
	if !ok {
		t.Fatalf("the body came out %T, want a bind", ip.Body)
	}
	if len(body.ContextList) != 1 {
		t.Fatalf("the bind came back with %d contexts, want 1", len(body.ContextList))
	}
	if len(body.ContextList[0].TransferSyntaxes) != 1 {
		t.Fatalf("the context came back with %d syntaxes, want 1", len(body.ContextList[0].TransferSyntaxes))
	}
	if body.ContextList[0].TransferSyntaxes[0].IfUUID != [16]byte(NDR32) {
		t.Error("the syntax offered did not come back as the one that was sent")
	}
}

// TestInboundPacketReadsARequestAndItsPayload is the packet carrying a call. What follows the
// body is the payload the call is decoded from, and it must come through whole.
func TestInboundPacketReadsARequestAndItsPayload(t *testing.T) {
	payload := []byte("the marshalled arguments of the call")

	var body []byte
	body = binary.LittleEndian.AppendUint32(body, uint32(len(payload))) // allocation hint
	body = binary.LittleEndian.AppendUint16(body, 0)                    // context
	body = binary.LittleEndian.AppendUint16(body, NET_SHARE_ENUM_ALL)
	body = append(body, payload...)

	ip := &InboundPacket{}
	if err := ip.Read(bytes.NewReader(header(PACKET_TYPE_REQUEST, body))); err != nil {
		t.Fatalf("a request would not read: %v", err)
	}

	req, ok := ip.Body.(*Request)
	if !ok {
		t.Fatalf("the body came out %T, want a request", ip.Body)
	}
	if req.OpNum != NET_SHARE_ENUM_ALL {
		t.Errorf("the operation came out %d, want %d", req.OpNum, NET_SHARE_ENUM_ALL)
	}
	if !bytes.Equal(ip.Payload, payload) {
		t.Errorf("the payload came out %q, want %q", ip.Payload, payload)
	}
}

// TestInboundPacketRefusesAPacketThatDidNotArrive is the whole reason reading reports what it
// could not do. The caller switches on the packet type and reaches straight into the body, so a
// packet that was never read must not come back looking like one that was: a header that did not
// arrive leaves the type at zero, which is a request, and the payload at nothing — and an
// operation number of zero is a live call on two of the three pipes this server answers.
func TestInboundPacketRefusesAPacketThatDidNotArrive(t *testing.T) {
	for _, tt := range []struct {
		name string
		data []byte
	}{
		{"nothing at all", nil},
		{"half a header", make([]byte, 8)},
		{"a header and no body", header(PACKET_TYPE_BIND, nil)},
		{"a bind whose body stops short", header(PACKET_TYPE_BIND, []byte{1, 2, 3})},
		{"a request whose body stops short", header(PACKET_TYPE_REQUEST, []byte{1, 2, 3})},
		{"a bind promising a context that never came", header(PACKET_TYPE_BIND, bindBody(1, nil))},
		{"a bind whose context stops short", header(PACKET_TYPE_BIND, bindBody(1, []byte{0, 0, 1, 0}))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ip := &InboundPacket{}
			if err := ip.Read(bytes.NewReader(tt.data)); err == nil {
				t.Fatalf("a packet that did not arrive was read as %+v", ip.Body)
			}
		})
	}
}

// TestInboundPacketRefusesATypeItCannotRead is a packet of a type with no body decoder behind it.
// Reading used to stop and say nothing, leaving the body unset while the header named a type; the
// caller reads the type and asserts the body against it.
func TestInboundPacketRefusesATypeItCannotRead(t *testing.T) {
	for _, packetType := range []uint8{
		PACKET_TYPE_FAULT,
		PACKET_TYPE_BIND_NAK,
		PACKET_TYPE_ALTER_CONTEXT,
		PACKET_TYPE_AUTH3,
		PACKET_TYPE_SHUTDOWN,
		PACKET_TYPE_CANCEL,
		PACKET_TYPE_ORPHANED,
		0x7f,
	} {
		ip := &InboundPacket{}
		err := ip.Read(bytes.NewReader(header(packetType, make([]byte, 32))))
		if !errors.Is(err, ErrUnsupportedPacketType) {
			t.Errorf("a packet of type %#02x was answered with %v, want it refused", packetType, err)
		}
		if ip.Body != nil {
			t.Errorf("a packet of type %#02x came back with a body of %T", packetType, ip.Body)
		}
	}
}

// TestBindDecodeDoesNotTrustTheNumberOfContexts is the packet that took the server off the
// machine it was running on. The count of contexts is a thirty-two bit field, and a list laid out
// to the length it names is thirty-two gigabytes of pointers for a body of twelve bytes — claimed
// before a single context is read, so nothing about the packet being nonsense is ever noticed.
// The process is killed for it, taking every other client's sessions with it.
//
// The list is grown as contexts arrive now, so what can be asked for is bounded by what was sent.
func TestBindDecodeDoesNotTrustTheNumberOfContexts(t *testing.T) {
	for _, count := range []uint32{0xffffffff, 0x7fffffff, 1 << 20, 1000} {
		b := &Bind{}
		err := b.Decode(bytes.NewReader(bindBody(count, nil)))

		if err == nil {
			t.Errorf("a bind promising %d contexts and carrying none was read all the same", count)
		}
		if len(b.ContextList) != 0 {
			t.Errorf("a bind promising %d contexts left %d behind", count, len(b.ContextList))
		}
	}
}

// TestBindDecodeTakesTheContextsThatArrive is the same guard from the other side: a bind whose
// count matches what it carries reads through in full.
func TestBindDecodeTakesTheContextsThatArrive(t *testing.T) {
	body := bindBody(2, append(presentationContext(0, NDR32), presentationContext(1, NDR64, BIND_TIME_FEATURES)...))

	b := &Bind{}
	if err := b.Decode(bytes.NewReader(body)); err != nil {
		t.Fatalf("a bind carrying what it promised would not read: %v", err)
	}

	if len(b.ContextList) != 2 {
		t.Fatalf("the bind came back with %d contexts, want 2", len(b.ContextList))
	}
	if got := len(b.ContextList[1].TransferSyntaxes); got != 2 {
		t.Fatalf("the second context came back with %d syntaxes, want 2", got)
	}
	if b.ContextList[1].ContextID != 1 {
		t.Errorf("the second context came back with ID %d, want 1", b.ContextList[1].ContextID)
	}
}

// TestContextDecodeDoesNotTrustTheNumberOfSyntaxes is the same claim one level down. The count is
// only sixteen bits here, so the reach is shorter, but a context promising sixty-five thousand
// syntaxes and carrying none should still not lay out room for them.
func TestContextDecodeDoesNotTrustTheNumberOfSyntaxes(t *testing.T) {
	var body []byte
	body = binary.LittleEndian.AppendUint16(body, 0)      // context ID
	body = binary.LittleEndian.AppendUint16(body, 0xffff) // the number of syntaxes
	body = append(body, syntax(NDR32)...)                 // the abstract syntax, and nothing after it

	c := &Context{}
	if err := c.Decode(bytes.NewReader(body)); err == nil {
		t.Error("a context promising syntaxes it did not carry was read all the same")
	}
	if len(c.TransferSyntaxes) != 0 {
		t.Errorf("%d syntaxes were left behind by a context that carried none", len(c.TransferSyntaxes))
	}
}

// TestBindAckRefusesAnAddressOfNoLength is the length that counts its own terminator and says
// there is none. Taking one off it wraps the count round to its largest value, and the whole of a
// slice holding nothing is then asked for.
func TestBindAckRefusesAnAddressOfNoLength(t *testing.T) {
	body := make([]byte, 10) // the address length is the last two bytes, left at zero

	ba := &BindAck{}
	if err := ba.Decode(bytes.NewReader(body)); err == nil {
		t.Error("an address shorter than the terminator it must carry was read all the same")
	}
}

// TestBindAckDecodeDoesNotTrustTheNumberOfResults is the allocation of a bind read the other way.
// This packet is a response, but the reader takes it from a client all the same.
func TestBindAckDecodeDoesNotTrustTheNumberOfResults(t *testing.T) {
	var body []byte
	body = binary.LittleEndian.AppendUint16(body, 0xffff)
	body = binary.LittleEndian.AppendUint16(body, 0xffff)
	body = binary.LittleEndian.AppendUint32(body, 0)
	body = binary.LittleEndian.AppendUint16(body, 1) // an address of one byte: just its terminator
	body = append(body, 0)
	// Ten bytes of fixed fields and one of address come to eleven, which the padding takes up to
	// twelve. Getting this wrong puts the count where the padding is, and it reads as nothing.
	body = append(body, 0)
	body = binary.LittleEndian.AppendUint32(body, 1<<30) // the number of results

	ba := &BindAck{}
	if err := ba.Decode(bytes.NewReader(body)); err == nil {
		t.Error("a bind ack promising results it did not carry was read all the same")
	}
	if len(ba.ResultList) != 0 {
		t.Errorf("%d results were left behind by a packet that carried none", len(ba.ResultList))
	}
}

// TestBindAckRoundTrip writes the answer to a bind and reads it back, which is the one packet
// this server both builds and can be handed.
func TestBindAckRoundTrip(t *testing.T) {
	want := &BindAck{
		MaxXmitFrag:  0xffff,
		MaxRecvFrag:  0xffff,
		AssocGroupID: 0x1234,
		PortSpec:     "\\pipe\\srvsvc",
		ResultList: []*Result{
			{TransferSyntax: &SyntaxID{IfUUID: [16]byte(NDR32), IfVersionMajor: 2}},
		},
	}

	var buf bytes.Buffer
	want.Encode(&buf)

	got := &BindAck{}
	if err := got.Decode(&buf); err != nil {
		t.Fatalf("could not read back what was written: %v", err)
	}

	if got.PortSpec != want.PortSpec {
		t.Errorf("the address came back %q, want %q", got.PortSpec, want.PortSpec)
	}
	if got.AssocGroupID != want.AssocGroupID {
		t.Errorf("the association group came back %#x, want %#x", got.AssocGroupID, want.AssocGroupID)
	}
	if len(got.ResultList) != 1 {
		t.Fatalf("the result list came back with %d entries, want 1", len(got.ResultList))
	}
	if got.ResultList[0].TransferSyntax.IfUUID != [16]byte(NDR32) {
		t.Error("the syntax agreed on did not come back as the one that was sent")
	}
}

// TestOutboundPacketWriteFillsInTheLength is the field a client reads to know where one packet
// ends and the next begins.
func TestOutboundPacketWriteFillsInTheLength(t *testing.T) {
	op := &OutboundPacket{
		Header: NewHeader(PACKET_TYPE_BIND_ACK, PFC_FIRST_FRAG|PFC_LAST_FRAG, 1),
		Body:   &BindAck{PortSpec: "\\pipe\\srvsvc"},
	}

	var buf bytes.Buffer
	if err := op.Write(&buf); err != nil {
		t.Fatalf("the packet would not go out: %v", err)
	}

	out := buf.Bytes()
	if got := binary.LittleEndian.Uint16(out[8:10]); int(got) != len(out) {
		t.Errorf("the packet says it is %d bytes long and is %d", got, len(out))
	}
}

// TestOutboundPacketWriteRefusesABodyTooLongToDescribe is the length field overflowing. It is
// sixteen bits, so a body over a little under sixty-five thousand bytes cannot be described by
// the header in front of it; counted out regardless the length wraps round, and the packet goes
// out saying it is a handful of bytes long while carrying tens of thousands. A client reads the
// number it was given, takes that many bytes as the packet and starts reading the next one in the
// middle of this one. A share list long enough is the way in.
func TestOutboundPacketWriteRefusesABodyTooLongToDescribe(t *testing.T) {
	// Enough shares to carry the body past what the length field can describe.
	shares := make([]NetShareInfo1, 2000)
	for i := range shares {
		shares[i] = NetShareInfo1{Share: "a share with a reasonably long name", Comment: "and a remark"}
	}

	op := NewNetShareEnumAllResponse(1, shares, 0)

	var buf bytes.Buffer
	err := op.Write(&buf)
	if !errors.Is(err, ErrTooLongToSend) {
		t.Fatalf("a response too long to describe was answered with %v, want it refused", err)
	}
}

// TestOutboundPacketWriteTakesWhatFits is the boundary from the other side: a response just
// inside what the length can describe still goes out.
func TestOutboundPacketWriteTakesWhatFits(t *testing.T) {
	shares := make([]NetShareInfo1, 50)
	for i := range shares {
		shares[i] = NetShareInfo1{Share: "share", Comment: "remark"}
	}

	var buf bytes.Buffer
	if err := NewNetShareEnumAllResponse(1, shares, 0).Write(&buf); err != nil {
		t.Fatalf("a response that fits would not go out: %v", err)
	}
	if got := binary.LittleEndian.Uint16(buf.Bytes()[8:10]); int(got) != buf.Len() {
		t.Errorf("the packet says it is %d bytes long and is %d", got, buf.Len())
	}
}

// TestOutboundPacketWriteOnNothing is the packet the caller never filled in, which is what a call
// it did not recognise leaves behind. It is written unconditionally.
func TestOutboundPacketWriteOnNothing(t *testing.T) {
	var buf bytes.Buffer

	var op *OutboundPacket
	if err := op.Write(&buf); err != nil {
		t.Errorf("writing nothing was answered with %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("writing nothing put %d bytes out", buf.Len())
	}

	if err := (&OutboundPacket{Header: NewHeader(0, 0, 0)}).Write(&buf); err != nil {
		t.Errorf("writing a packet with no body was answered with %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("writing a packet with no body put %d bytes out", buf.Len())
	}
}

// FuzzInboundPacketRead walks the bytes of a packet as they arrive from a client that has opened
// one of the named pipes. The property is that reading answers rather than ending the process,
// and that a packet said to have been read carries the body its type calls for — the caller
// asserts the body against the type without looking first.
func FuzzInboundPacketRead(f *testing.F) {
	f.Add(header(PACKET_TYPE_BIND, bindBody(1, presentationContext(0, NDR32))))
	f.Add(header(PACKET_TYPE_BIND, bindBody(0xffffffff, nil)))
	f.Add(header(PACKET_TYPE_REQUEST, make([]byte, 8)))
	f.Add(header(PACKET_TYPE_BIND_ACK, make([]byte, 32)))
	f.Add(header(PACKET_TYPE_RESPONSE, make([]byte, 8)))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		ip := &InboundPacket{}
		if err := ip.Read(bytes.NewReader(data)); err != nil {
			return
		}

		// A packet that was read is one the caller will reach into, so the body has to be there
		// and has to be of the type the header names.
		var ok bool
		switch ip.Header.PacketType {
		case PACKET_TYPE_BIND:
			_, ok = ip.Body.(*Bind)
		case PACKET_TYPE_BIND_ACK:
			_, ok = ip.Body.(*BindAck)
		case PACKET_TYPE_REQUEST:
			_, ok = ip.Body.(*Request)
		case PACKET_TYPE_RESPONSE:
			_, ok = ip.Body.(*Response)
		}

		if !ok {
			t.Fatalf("a packet of type %#02x was read with a body of %T", ip.Header.PacketType, ip.Body)
		}
	})
}

// dribble hands over one byte at a time, which is what a reader is free to do and what a decoder
// taking the first read as the whole of a field would get wrong.
type dribble struct {
	data []byte
	pos  int
}

func (d *dribble) Read(p []byte) (int, error) {
	if d.pos >= len(d.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	p[0] = d.data[d.pos]
	d.pos++

	return 1, nil
}
