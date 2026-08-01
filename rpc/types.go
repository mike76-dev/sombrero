package rpc

import (
	"context"
	"encoding/binary"
	"io"

	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/utils"
	"github.com/oiweiwei/go-msrpc/msrpc/lsat/lsarpc/v0"
	"github.com/oiweiwei/go-msrpc/ndr"
)

// Encoder is an interface for encoding outbound MS-RPC packets.
type Encoder interface {
	Encode(w io.Writer)
}

// Decoder is an interface for decoding inbound MS-RPC packets. Decoding reports what it could
// not read rather than leaving the fields behind a short read holding zeroes nobody sent.
type Decoder interface {
	Decode(r io.Reader) error
}

// SyntaxID represents an LSARPC syntax.
type SyntaxID struct {
	IfUUID         [16]byte
	IfVersionMajor uint16
	IfVersionMinor uint16
}

// Encode implements Encoder interface.
func (sid *SyntaxID) Encode(w io.Writer) {
	buf := make([]byte, 16)
	copy(buf, sid.IfUUID[:])
	buf = binary.LittleEndian.AppendUint16(buf, sid.IfVersionMajor)
	buf = binary.LittleEndian.AppendUint16(buf, sid.IfVersionMinor)
	w.Write(buf)
}

// Decode implements Decoder interface.
func (sid *SyntaxID) Decode(r io.Reader) error {
	buf := make([]byte, 20)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}

	copy(sid.IfUUID[:], buf[:16])
	sid.IfVersionMajor = binary.LittleEndian.Uint16(buf[16:18])
	sid.IfVersionMinor = binary.LittleEndian.Uint16(buf[18:20])

	return nil
}

// Context represents an LSARPC context.
type Context struct {
	ContextID        uint16
	AbstractSyntax   *SyntaxID
	TransferSyntaxes []*SyntaxID
}

// Encode implements Encoder interface.
func (c *Context) Encode(w io.Writer) {
	var buf []byte
	buf = binary.LittleEndian.AppendUint16(buf, c.ContextID)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(c.TransferSyntaxes)))
	w.Write(buf)
	c.AbstractSyntax.Encode(w)
	for _, ts := range c.TransferSyntaxes {
		ts.Encode(w)
	}
}

// Decode implements Decoder interface.
func (c *Context) Decode(r io.Reader) error {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}

	c.ContextID = binary.LittleEndian.Uint16(buf[:2])
	c.AbstractSyntax = &SyntaxID{}
	if err := c.AbstractSyntax.Decode(r); err != nil {
		return err
	}

	// The list is grown as each syntax is read rather than laid out to the length the count
	// names. The count arrives from the far end and is believed by nobody until the bytes it
	// promises actually turn up.
	count := binary.LittleEndian.Uint16(buf[2:])
	for i := 0; i < int(count); i++ {
		sid := &SyntaxID{}
		if err := sid.Decode(r); err != nil {
			return err
		}
		c.TransferSyntaxes = append(c.TransferSyntaxes, sid)
	}

	return nil
}

// Bind represents an MS-RPC Bind call.
type Bind struct {
	MaxXmitFrag  uint16
	MaxRecvFrag  uint16
	AssocGroupID uint32
	ContextList  []*Context
}

// Encode implements Encoder interface.
func (b *Bind) Encode(w io.Writer) {
	var buf []byte
	buf = binary.LittleEndian.AppendUint16(buf, b.MaxXmitFrag)
	buf = binary.LittleEndian.AppendUint16(buf, b.MaxRecvFrag)
	buf = binary.LittleEndian.AppendUint32(buf, b.AssocGroupID)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(b.ContextList)))
	w.Write(buf)
	for _, c := range b.ContextList {
		c.Encode(w)
	}
}

// Decode implements Decoder interface.
func (b *Bind) Decode(r io.Reader) error {
	buf := make([]byte, 12)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}

	b.MaxXmitFrag = binary.LittleEndian.Uint16(buf[:2])
	b.MaxRecvFrag = binary.LittleEndian.Uint16(buf[2:4])
	b.AssocGroupID = binary.LittleEndian.Uint32(buf[4:8])

	// The number of contexts is a thirty-two bit field, and laying out a list to the length it
	// names hands whoever sent it the run of this machine's memory: twelve bytes saying four
	// billion asks for thirty-two gigabytes of pointers before a single context is read, and
	// the process is killed for it. The list is grown as contexts actually arrive instead, so
	// what can be claimed is bounded by what was sent.
	count := binary.LittleEndian.Uint32(buf[8:])
	for i := uint32(0); i < count; i++ {
		c := &Context{}
		if err := c.Decode(r); err != nil {
			return err
		}
		b.ContextList = append(b.ContextList, c)
	}

	return nil
}

// Result represents an MS-RPC bind result.
type Result struct {
	DefResult      uint16
	ProviderReason uint16
	TransferSyntax *SyntaxID
}

// Encode implements Encoder interface.
func (res *Result) Encode(w io.Writer) {
	var buf []byte
	buf = binary.LittleEndian.AppendUint16(buf, res.DefResult)
	buf = binary.LittleEndian.AppendUint16(buf, res.ProviderReason)
	w.Write(buf)
	res.TransferSyntax.Encode(w)
}

// Decode implements Decoder interface.
func (res *Result) Decode(r io.Reader) error {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}

	res.DefResult = binary.LittleEndian.Uint16(buf[:2])
	res.ProviderReason = binary.LittleEndian.Uint16(buf[2:4])
	res.TransferSyntax = &SyntaxID{}

	return res.TransferSyntax.Decode(r)
}

// BindAck represents an MS-RPC Bind_ack call.
type BindAck struct {
	MaxXmitFrag  uint16
	MaxRecvFrag  uint16
	AssocGroupID uint32
	PortSpec     string
	ResultList   []*Result
}

// Encode implements Encoder interface.
func (ba *BindAck) Encode(w io.Writer) {
	var buf []byte
	buf = binary.LittleEndian.AppendUint16(buf, ba.MaxXmitFrag)
	buf = binary.LittleEndian.AppendUint16(buf, ba.MaxRecvFrag)
	buf = binary.LittleEndian.AppendUint32(buf, ba.AssocGroupID)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(ba.PortSpec)+1))
	buf = append(buf, []byte(ba.PortSpec)...)
	buf = append(buf, 0)
	padLen := utils.Roundup(len(buf), 4)
	padding := make([]byte, padLen-len(buf))
	buf = append(buf, padding...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(ba.ResultList)))
	w.Write(buf)
	for _, res := range ba.ResultList {
		res.Encode(w)
	}
}

// Decode implements Decoder interface.
func (ba *BindAck) Decode(r io.Reader) error {
	buf := make([]byte, 10)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}

	ba.MaxXmitFrag = binary.LittleEndian.Uint16(buf[:2])
	ba.MaxRecvFrag = binary.LittleEndian.Uint16(buf[2:4])
	ba.AssocGroupID = binary.LittleEndian.Uint32(buf[4:8])

	// The address counts its own terminator, so a length of zero describes something shorter
	// than the terminator it must end with. Taking one off it regardless wraps the count round
	// to its largest value and asks for the whole of a slice that has nothing in it.
	addrLen := binary.LittleEndian.Uint16(buf[8:])
	if addrLen == 0 {
		return ErrTruncated
	}

	addr := make([]byte, addrLen)
	if _, err := io.ReadFull(r, addr); err != nil {
		return err
	}

	ba.PortSpec = string(addr[:addrLen-1])
	padLen := utils.Roundup(len(buf)+int(addrLen), 4)
	padding := make([]byte, padLen-len(buf)-int(addrLen))
	if _, err := io.ReadFull(r, padding); err != nil {
		return err
	}

	resNum := make([]byte, 4)
	if _, err := io.ReadFull(r, resNum); err != nil {
		return err
	}

	// As with the context list of a bind: grown as results arrive, never laid out to a length
	// the far end named.
	count := binary.LittleEndian.Uint32(resNum)
	for i := uint32(0); i < count; i++ {
		res := &Result{}
		if err := res.Decode(r); err != nil {
			return err
		}
		ba.ResultList = append(ba.ResultList, res)
	}

	return nil
}

// Request represents an MS-RPC Request call.
type Request struct {
	AllocHint  uint32
	ContextID  uint16
	OpNum      uint16
	ObjectUUID []byte
}

// Encode implements Encoder interface.
func (req *Request) Encode(w io.Writer) {
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, req.AllocHint)
	buf = binary.LittleEndian.AppendUint16(buf, req.ContextID)
	buf = binary.LittleEndian.AppendUint16(buf, req.OpNum)
	if req.ObjectUUID != nil {
		buf = append(buf, req.ObjectUUID...)
	}
	w.Write(buf)
}

// Decode implements Decoder interface.
func (req *Request) Decode(r io.Reader) error {
	buf := make([]byte, 8)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}

	req.AllocHint = binary.LittleEndian.Uint32(buf[:4])
	req.ContextID = binary.LittleEndian.Uint16(buf[4:6])
	req.OpNum = binary.LittleEndian.Uint16(buf[6:8])
	if req.ObjectUUID != nil {
		uuid := make([]byte, 16)
		if _, err := io.ReadFull(r, uuid); err != nil {
			return err
		}

		copy(req.ObjectUUID, uuid)
	}

	return nil
}

// Response represents an MS-RPC Response call.
type Response struct {
	AllocHint   uint32
	ContextID   uint16
	CancelCount uint16
}

// Encode implements Encoder interface.
func (resp *Response) Encode(w io.Writer) {
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, resp.AllocHint)
	buf = binary.LittleEndian.AppendUint16(buf, resp.ContextID)
	buf = binary.LittleEndian.AppendUint16(buf, resp.CancelCount)
	w.Write(buf)
}

// Decode implements Decoder interface.
func (resp *Response) Decode(r io.Reader) error {
	buf := make([]byte, 8)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}

	resp.AllocHint = binary.LittleEndian.Uint32(buf[:4])
	resp.ContextID = binary.LittleEndian.Uint16(buf[4:6])
	resp.CancelCount = binary.LittleEndian.Uint16(buf[6:8])

	return nil
}

// Frame combines an LSARPC frame handle and an NTLM security context.
type Frame struct {
	Handle          lsarpc.Handle
	SecurityContext ntlm.SecurityContext
}

// NetShareGetInfoRequest represents an MS-RPC NetShareGetInfo request.
type NetShareGetInfoRequest struct {
	Server string
	Share  string
	Level  uint32
}

// ndrString reads a conformant and varying string: a maximum count, an offset into it, the count
// actually present and then that many sixteen-bit characters, the last of which terminates it.
// It hands back the string and how far past off the whole of it reached.
//
// Every length here arrives from the far end, so the arithmetic is done in a width wide enough
// that none of it can wrap round and name a range that falls back inside the buffer.
func ndrString(buf []byte, off uint64) (string, uint64, error) {
	const headerLen = 12

	if !fits(off, headerLen, uint64(len(buf))) {
		return "", 0, ErrTruncated
	}

	count := uint64(binary.LittleEndian.Uint32(buf[off+8 : off+12]))
	length := count * 2
	if !fits(off+headerLen, length, uint64(len(buf))) {
		return "", 0, ErrTruncated
	}

	// A string of no characters has no terminator on the end to take off.
	if count == 0 {
		return "", headerLen, nil
	}

	return utils.DecodeToString(buf[off+headerLen : off+headerLen+length]), headerLen + length, nil
}

// fits reports whether a field of the given length starting at off is wholly inside a buffer of
// the given size. The sum is worked out in sixty-four bits so that an offset and a length near
// the top of their own fields cannot wrap round and appear to fit.
func fits(off, length, size uint64) bool {
	return off+length <= size
}

// uint32At reads a four-byte field, saying so if the buffer does not reach that far.
func uint32At(buf []byte, off uint64) (uint32, error) {
	if !fits(off, 4, uint64(len(buf))) {
		return 0, ErrTruncated
	}

	return binary.LittleEndian.Uint32(buf[off : off+4]), nil
}

// Unmarshal decodes the NetShareGetInfo request.
func (req *NetShareGetInfoRequest) Unmarshal(buf []byte) error {
	var off uint64

	ptr, err := uint32At(buf, off)
	if err != nil {
		return err
	}
	if ptr > 256 {
		off += 4
	}

	server, n, err := ndrString(buf, off)
	if err != nil {
		return err
	}
	req.Server = server
	off = uint64(utils.Roundup(int(off+n), 4))

	ptr, err = uint32At(buf, off)
	if err != nil {
		return err
	}
	if ptr > 256 {
		off += 4
	}

	share, n, err := ndrString(buf, off)
	if err != nil {
		return err
	}
	req.Share = share
	off = uint64(utils.Roundup(int(off+n), 4))

	req.Level, err = uint32At(buf, off)

	return err
}

// NetShareInfo1 represents an MS-RPC NetShareInfo Type 1 structure.
type NetShareInfo1 struct {
	Share   string
	Type    uint32
	Comment string
}

// NetShareInfo1Response represents an MS-RPC NetShareInfo Type 1 response.
type NetShareInfo1Response struct {
	NetShareInfo1
	Result uint32
}

// MarshalNDR implements ndr.Marshaller interface.
func (resp *NetShareInfo1Response) MarshalNDR(ctx context.Context, w ndr.Writer) error {
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, 1)
	buf = binary.LittleEndian.AppendUint32(buf, 0x00020004)
	buf = binary.LittleEndian.AppendUint32(buf, 0x00020008)
	buf = binary.LittleEndian.AppendUint32(buf, resp.Type)
	buf = binary.LittleEndian.AppendUint32(buf, 0x0002000c)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(resp.Share)+1))
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(resp.Share)+1))
	buf = append(buf, utils.EncodeStringToBytes(resp.Share)...)
	buf = append(buf, 0, 0)
	padLen := utils.Roundup(len(buf), 4) - len(buf)
	padding := make([]byte, padLen)
	buf = append(buf, padding...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(resp.Comment)+1))
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(resp.Comment)+1))
	buf = append(buf, utils.EncodeStringToBytes(resp.Comment)...)
	buf = append(buf, 0, 0)
	padLen = utils.Roundup(len(buf), 4) - len(buf)
	padding = make([]byte, padLen)
	buf = append(buf, padding...)
	buf = binary.LittleEndian.AppendUint32(buf, resp.Result)
	_, err := w.Write(buf)
	return err
}

// MdsOpenRequest represents an MS-RPC MdsOpen request.
type MdsOpenRequest struct {
	DeviceID       uint32
	Unkn2          uint32
	Unkn3          uint32
	ShareMountPath string
	ShareName      string
	MaxCount       uint32
}

// Unmarshal decodes the MdsOpen request.
func (req *MdsOpenRequest) Unmarshal(buf []byte) error {
	if !fits(0, 24, uint64(len(buf))) {
		return ErrTruncated
	}

	req.DeviceID = binary.LittleEndian.Uint32(buf[:4])
	req.Unkn2 = binary.LittleEndian.Uint32(buf[4:8])
	req.Unkn3 = binary.LittleEndian.Uint32(buf[8:12])
	req.MaxCount = binary.LittleEndian.Uint32(buf[12:16])

	// The two names here are plain bytes rather than sixteen-bit characters, and each counts
	// the terminator that ends it. A count of zero describes something shorter than the
	// terminator alone, and taking one off it wraps it round to name the whole address space.
	smpLen := uint64(binary.LittleEndian.Uint32(buf[20:24]))
	if smpLen == 0 || !fits(24, smpLen, uint64(len(buf))) {
		return ErrTruncated
	}

	req.ShareMountPath = string(buf[24 : 24+smpLen-1])

	off := uint64(utils.Roundup(int(24+smpLen), 4))
	if !fits(off, 12, uint64(len(buf))) {
		return ErrTruncated
	}

	snLen := uint64(binary.LittleEndian.Uint32(buf[off+8 : off+12]))
	if snLen == 0 || !fits(off+12, snLen, uint64(len(buf))) {
		return ErrTruncated
	}

	req.ShareName = string(buf[off+12 : off+12+snLen-1])

	return nil
}

// MdsOpenResponse represents an MS-RPC MdsOpen response.
type MdsOpenResponse struct {
	DeviceID     uint32
	Unkn2        uint32
	Unkn3        uint32
	SharePath    string
	PolicyHandle [20]byte
	MaxCount     uint32
}

// MarshalNDR implements ndr.Marshaller interface.
func (resp *MdsOpenResponse) MarshalNDR(ctx context.Context, w ndr.Writer) error {
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, resp.DeviceID)
	buf = binary.LittleEndian.AppendUint32(buf, resp.Unkn2)
	buf = binary.LittleEndian.AppendUint32(buf, resp.Unkn3)
	buf = binary.LittleEndian.AppendUint32(buf, resp.MaxCount)
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(resp.SharePath)+1))
	buf = append(buf, []byte(resp.SharePath)...)
	buf = append(buf, 0)
	padLen := utils.Roundup(len(buf), 4) - len(buf)
	padding := make([]byte, padLen)
	buf = append(buf, padding...)
	buf = append(buf, resp.PolicyHandle[:]...)
	_, err := w.Write(buf)
	return err
}

// NetShareEnumAllRequest represents an MS-RPC NetShareEnumAll request.
type NetShareEnumAllRequest struct {
	Server    string
	Level     uint32
	MaxBuffer uint32
}

// Unmarshal decodes the NetShareEnumAll request.
func (req *NetShareEnumAllRequest) Unmarshal(buf []byte) error {
	// The name begins one field further in than the others, the pointer in front of it having
	// already been stepped over by the caller.
	server, n, err := ndrString(buf, 4)
	if err != nil {
		return err
	}
	req.Server = server

	off := uint64(utils.Roundup(int(4+n), 4))
	req.Level, err = uint32At(buf, off)
	if err != nil {
		return err
	}

	req.MaxBuffer, err = uint32At(buf, off+20)

	return err
}

// NetShareEnumAllResponse represents an MS-RPC NetShareEnumAll response.
type NetShareEnumAllResponse struct {
	Shares []NetShareInfo1
	Result uint32
}

// MarshalNDR implements ndr.Marshaller interface.
func (resp *NetShareEnumAllResponse) MarshalNDR(ctx context.Context, w ndr.Writer) error {
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, 1)
	buf = binary.LittleEndian.AppendUint32(buf, 1)
	buf = binary.LittleEndian.AppendUint32(buf, 0x0002000c)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(resp.Shares)))
	buf = binary.LittleEndian.AppendUint32(buf, 0x00020010)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(resp.Shares)))
	for i, share := range resp.Shares {
		buf = binary.LittleEndian.AppendUint32(buf, 0x00020014+uint32(i)*8)
		buf = binary.LittleEndian.AppendUint32(buf, share.Type)
		buf = binary.LittleEndian.AppendUint32(buf, 0x00020018+uint32(i)*8)
	}

	for _, share := range resp.Shares {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(share.Share)+1))
		buf = binary.LittleEndian.AppendUint32(buf, 0)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(share.Share)+1))
		buf = append(buf, utils.EncodeStringToBytes(share.Share)...)
		buf = append(buf, 0, 0)
		padLen := utils.Roundup(len(buf), 4) - len(buf)
		padding := make([]byte, padLen)
		buf = append(buf, padding...)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(share.Comment)+1))
		buf = binary.LittleEndian.AppendUint32(buf, 0)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(share.Comment)+1))
		buf = append(buf, utils.EncodeStringToBytes(share.Comment)...)
		buf = append(buf, 0, 0)
		padLen = utils.Roundup(len(buf), 4) - len(buf)
		padding = make([]byte, padLen)
		buf = append(buf, padding...)
	}

	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(resp.Shares)))
	buf = binary.LittleEndian.AppendUint32(buf, 0x00020014+uint32(len(resp.Shares)*2))
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, resp.Result)
	_, err := w.Write(buf)
	return err
}
