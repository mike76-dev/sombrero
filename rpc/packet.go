package rpc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

const (
	HeaderSize = 16
)

var (
	// ErrUnsupportedPacketType is returned for a packet of a type this implementation does not
	// decode. The body is left unset, so the caller must not reach into it.
	ErrUnsupportedPacketType = errors.New("rpc: packet type not supported")

	// ErrTruncated is returned when a field runs past the end of what arrived.
	ErrTruncated = errors.New("rpc: the packet ends in the middle of a field")

	// ErrTooLongToSend is returned for a response that will not fit in one fragment.
	ErrTooLongToSend = errors.New("rpc: the response is too long to send in one fragment")
)

var (
	NDR32 = []byte{
		0x04, 0x5d, 0x88, 0x8a, 0xeb, 0x1c, 0xc9, 0x11,
		0x9f, 0xe8, 0x08, 0x00, 0x2b, 0x10, 0x48, 0x60,
	}

	NDR64 = []byte{
		0x33, 0x05, 0x71, 0x71, 0xba, 0xbe, 0x37, 0x49,
		0x83, 0x19, 0xb5, 0xdb, 0xef, 0x9c, 0xcc, 0x36,
	}

	BIND_TIME_FEATURES = []byte{
		0x2c, 0x1c, 0xb7, 0x6c, 0x12, 0x98, 0x40, 0x45,
		0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
)

const (
	// MS-RPC packet types.
	PACKET_TYPE_REQUEST                = 0x00
	PACKET_TYPE_RESPONSE               = 0x02
	PACKET_TYPE_FAULT                  = 0x03
	PACKET_TYPE_BIND                   = 0x0b
	PACKET_TYPE_BIND_ACK               = 0x0c
	PACKET_TYPE_BIND_NAK               = 0x0d
	PACKET_TYPE_ALTER_CONTEXT          = 0x0e
	PACKET_TYPE_ALTER_CONTEXT_RESPONSE = 0x0f
	PACKET_TYPE_AUTH3                  = 0x10
	PACKET_TYPE_SHUTDOWN               = 0x11
	PACKET_TYPE_CANCEL                 = 0x12
	PACKET_TYPE_ORPHANED               = 0x13
)

const (
	// MS-RPC packet flags.
	PFC_FIRST_FRAG          = 0x01
	PFC_LAST_FRAG           = 0x02
	PFC_PENDING_CANCEL      = 0x04
	PFC_SUPPORT_HEADER_SIGN = 0x04
	PFC_CONC_MPX            = 0x10
	PFC_DID_NOT_EXECUTE     = 0x20
	PFC_MAYBE               = 0x40
	PFC_OBJECT_UUID         = 0x80
)

// Header represents the header of an MS-RPC packet.
type Header struct {
	RPCVersionMajor    uint8
	RPCVersionMinor    uint8
	PacketType         uint8
	PacketFlags        uint8
	DataRepresentation uint32
	FragLength         uint16
	AuthLength         uint16
	CallID             uint32
}

// Encode implements Encoder interface.
func (h *Header) Encode(w io.Writer) {
	buf := make([]byte, 16)
	buf[0] = h.RPCVersionMajor
	buf[1] = h.RPCVersionMinor
	buf[2] = h.PacketType
	buf[3] = h.PacketFlags
	binary.LittleEndian.PutUint32(buf[4:8], h.DataRepresentation)
	binary.LittleEndian.PutUint16(buf[8:10], h.FragLength)
	binary.LittleEndian.PutUint16(buf[10:12], h.AuthLength)
	binary.LittleEndian.PutUint32(buf[12:], h.CallID)
	w.Write(buf)
}

// Decode implements Decoder interface.
func (h *Header) Decode(r io.Reader) error {
	buf := make([]byte, 16)
	// Read is free to hand back fewer bytes than were asked for and no error with them, so the
	// whole of the header is waited for rather than whatever the first read happened to bring.
	// Taking the short read would leave the fields behind it holding zeroes that nothing sent.
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}

	h.RPCVersionMajor = buf[0]
	h.RPCVersionMinor = buf[1]
	h.PacketType = buf[2]
	h.PacketFlags = buf[3]
	h.DataRepresentation = binary.LittleEndian.Uint32(buf[4:8])
	h.FragLength = binary.LittleEndian.Uint16(buf[8:10])
	h.AuthLength = binary.LittleEndian.Uint16(buf[10:12])
	h.CallID = binary.LittleEndian.Uint32(buf[12:])

	return nil
}

// NewHeader returns a standard MS-RPC packet header.
func NewHeader(pt uint8, pf uint8, callID uint32) *Header {
	return &Header{
		RPCVersionMajor:    5,
		RPCVersionMinor:    0,
		PacketType:         pt,
		PacketFlags:        pf,
		DataRepresentation: 0x00000010, // LE byte order, ASCII character format, IEEE float format
		CallID:             callID,
	}
}

// InboundPacket represents an MS-RPC request.
type InboundPacket struct {
	Header  *Header
	Body    Decoder
	Payload []byte
}

// Read reads and decodes an MS-RPC request. A packet that cannot be read is reported rather than
// left half-decoded: the caller reaches into the body and the payload on the strength of the
// packet type, and a header that never arrived reads as a request of type zero carrying nothing,
// which is a live opnum on two of the three pipes.
func (ip *InboundPacket) Read(r io.Reader) error {
	ip.Header = &Header{}
	if err := ip.Header.Decode(r); err != nil {
		return err
	}

	switch ip.Header.PacketType {
	case PACKET_TYPE_BIND:
		ip.Body = &Bind{}
	case PACKET_TYPE_BIND_ACK:
		ip.Body = &BindAck{}
	case PACKET_TYPE_REQUEST:
		ip.Body = &Request{}
	case PACKET_TYPE_RESPONSE:
		ip.Body = &Response{}
	default:
		return ErrUnsupportedPacketType
	}

	if err := ip.Body.Decode(r); err != nil {
		return err
	}

	var buf bytes.Buffer
	n, err := buf.ReadFrom(r)
	if err == nil && n > 0 {
		ip.Payload = buf.Bytes()
	}

	return nil
}

// OutboundPacket represents an MS-RPC response.
type OutboundPacket struct {
	Header *Header
	Body   Encoder
}

// Write encodes an MS-RPC response and writes it to the provided stream.
func (op *OutboundPacket) Write(w io.Writer) error {
	if op == nil || op.Header == nil || op.Body == nil {
		return nil
	}

	var body bytes.Buffer
	op.Body.Encode(&body)

	// The length of a fragment is told in sixteen bits, so a body that will not fit in one
	// cannot be described by the header in front of it. Counted out regardless the length wraps
	// round and the packet goes out saying it is a few bytes long while carrying tens of
	// thousands, which a client reads as one packet and then reads the rest as another. This
	// implementation does not break a response into fragments, so the honest answer is to say
	// it cannot be sent. A share list long enough to reach this is the way in.
	if body.Len()+HeaderSize > math.MaxUint16 {
		return ErrTooLongToSend
	}

	op.Header.FragLength = uint16(body.Len()) + HeaderSize
	op.Header.Encode(w)
	_, err := w.Write(body.Bytes())

	return err
}
