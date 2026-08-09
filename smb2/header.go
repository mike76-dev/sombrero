package smb2

import (
	"encoding/binary"
	"errors"
)

const (
	PROTOCOL_SMB             = 0x424d53ff
	PROTOCOL_SMB2            = 0x424d53fe
	PROTOCOL_SMB2_ENCRYPTED  = 0x424d53fd
	PROTOCOL_SMB2_COMPRESSED = 0x424d53fc
)

const (
	// SMB command codes.
	SMB_COM_NEGOTIATE = 0x72

	// SMB2 command codes.
	SMB2_NEGOTIATE                     = 0x0000
	SMB2_SESSION_SETUP                 = 0x0001
	SMB2_LOGOFF                        = 0x0002
	SMB2_TREE_CONNECT                  = 0x0003
	SMB2_TREE_DISCONNECT               = 0x0004
	SMB2_CREATE                        = 0x0005
	SMB2_CLOSE                         = 0x0006
	SMB2_FLUSH                         = 0x0007
	SMB2_READ                          = 0x0008
	SMB2_WRITE                         = 0x0009
	SMB2_LOCK                          = 0x000a
	SMB2_IOCTL                         = 0x000b
	SMB2_CANCEL                        = 0x000c
	SMB2_ECHO                          = 0x000d
	SMB2_QUERY_DIRECTORY               = 0x000e
	SMB2_CHANGE_NOTIFY                 = 0x000f
	SMB2_QUERY_INFO                    = 0x0010
	SMB2_SET_INFO                      = 0x0011
	SMB2_OPLOCK_BREAK                  = 0x0012
	SMB2_SERVER_TO_CLIENT_NOTIFICATION = 0x0013
)

const (
	// SMB2 header flags.
	FLAGS_SERVER_TO_REDIR    = 0x00000001
	FLAGS_ASYNC_COMMAND      = 0x00000002
	FLAGS_RELATED_OPERATIONS = 0x00000004
	FLAGS_SIGNED             = 0x00000008
	FLAGS_PRIORITY_MASK      = 0x00000070
	FLAGS_DFS_OPERATIONS     = 0x10000000
	FLAGS_REPLAY_OPERATION   = 0x20000000
)

var (
	ErrEncryptedMessage  = errors.New("message encryption not supported")
	ErrCompressedMessage = errors.New("message compression not supported")
	ErrWrongLength       = errors.New("wrong data length")
	ErrWrongFormat       = errors.New("wrong data format")
	ErrWrongProtocol     = errors.New("unsupported protocol")
)

const (
	SMBHeaderSize  = 32
	SMB2HeaderSize = 64

	SMB2TransformHeaderSize            = 52
	SMB2CompressionTransformHeaderSize = 16
	SMB2CompressionPayloadHeaderOffset = 8
	SMB2CompressionPayloadHeaderSize   = 8

	SMB2HeaderStructureSize = 64
)

// Header extends the raw byte sequence with SMB functionality.
type Header []byte

// NewHeader converts a byte sequence into an SMB2 header.
func NewHeader(data []byte) Header {
	binary.LittleEndian.PutUint32(data[:4], PROTOCOL_SMB2)
	binary.LittleEndian.PutUint16(data[4:6], SMB2HeaderStructureSize)
	return Header(data)
}

// CopyFrom copies the data from another header. This is typically done when generating a response to a request.
func (h Header) CopyFrom(src Header) {
	copy(h[:SMB2HeaderSize], src[:SMB2HeaderSize])
}

// IsSmb returns true if the SMB signature is detected in the header.
func (h Header) IsSmb() bool {
	return h.ProtocolID() == PROTOCOL_SMB
}

// IsSmb2 returns true if the SMB2 signature is detected in the header.
func (h Header) IsSmb2() bool {
	id := h.ProtocolID()
	return id == PROTOCOL_SMB2 || id == PROTOCOL_SMB2_ENCRYPTED || id == PROTOCOL_SMB2_COMPRESSED
}

// Validate returns an error if the header is malformed, nil otherwise.
func (h Header) Validate() error {
	if len(h) < 4 {
		return ErrWrongLength
	}

	if h.IsSmb() {
		if len(h) < SMBHeaderSize {
			return ErrWrongLength
		}

		if h.LegacyCommand() != SMB_COM_NEGOTIATE {
			return ErrWrongProtocol
		}

		return nil
	}

	if h.IsSmb2() {
		if len(h) < SMB2HeaderSize {
			return ErrWrongLength
		}

		id := h.ProtocolID()
		if id == PROTOCOL_SMB2_ENCRYPTED {
			return ErrEncryptedMessage
		} else if id == PROTOCOL_SMB2_COMPRESSED {
			return ErrCompressedMessage
		} else if binary.LittleEndian.Uint16(h[4:6]) != SMB2HeaderStructureSize {
			return ErrWrongFormat
		}

		return nil
	}

	return ErrWrongProtocol
}

// LegacyCommand returns the Command field of the header assuming this is a legacy SMB header.
func (h Header) LegacyCommand() uint8 {
	return h[4]
}

// CreditCharge returns the CreditCharge field of the SMB2 header.
func (h Header) CreditCharge() uint16 {
	return binary.LittleEndian.Uint16(h[6:8])
}

// SetCreditCharge sets the CreditCharge field of the SMB2 header.
func (h Header) SetCreditCharge(cc uint16) {
	binary.LittleEndian.PutUint16(h[6:8], cc)
}

// Status returns the Status field of the SMB2 header.
func (h Header) Status() uint32 {
	return binary.LittleEndian.Uint32(h[8:12])
}

// SetStatus sets the Status field of the SMB2 header.
func (h Header) SetStatus(status uint32) {
	binary.LittleEndian.PutUint32(h[8:12], status)
}

// ChannelSequence returns the ChannelSequence field of the SMB2 header.
func (h Header) ChannelSequence() uint16 {
	return binary.LittleEndian.Uint16(h[8:10])
}

// Command returns the Command field of the SMB2 header.
func (h Header) Command() uint16 {
	return binary.LittleEndian.Uint16(h[12:14])
}

// SetCommand sets the Command field of the SMB2 header.
func (h Header) SetCommand(command uint16) {
	binary.LittleEndian.PutUint16(h[12:14], command)
}

// CreditRequest returns the CreditRequest field of the SMB2 header.
func (h Header) CreditRequest() uint16 {
	return binary.LittleEndian.Uint16(h[14:16])
}

// SetCreditResponse sets the CreditResponse field of the SMB2 header.
func (h Header) SetCreditResponse(cr uint16) {
	binary.LittleEndian.PutUint16(h[14:16], cr)
}

// Flags returns the Flags field of the SMB2 header.
func (h Header) Flags() uint32 {
	return binary.LittleEndian.Uint32(h[16:20])
}

// SetFlags sets the Flags field of the SMB2 header.
func (h Header) SetFlags(flags uint32) {
	binary.LittleEndian.PutUint32(h[16:20], flags)
}

// IsFlagSet returns true if the specified bit(s) is (are) set in the Flags field of the SMB2 header.
func (h Header) IsFlagSet(flag uint32) bool {
	return h.Flags()&flag > 0
}

// SetFlag sets the specified bit(s) in the Flags field of the SMB2 header.
func (h Header) SetFlag(flag uint32) {
	h.SetFlags(h.Flags() | flag)
}

// ClearFlag clears the specified bit(s) in the Flags field of the SMB2 header.
func (h Header) ClearFlag(flag uint32) {
	h.SetFlags(h.Flags() &^ flag)
}

// NextCommand returns the NextCommand field of the SMB2 header.
func (h Header) NextCommand() uint32 {
	return binary.LittleEndian.Uint32(h[20:24])
}

// SetNextCommand sets the NextCommand field of the SMB2 header.
func (h Header) SetNextCommand(nc uint32) {
	binary.LittleEndian.PutUint32(h[20:24], nc)
}

// MessageID returns the MessageID field of the SMB2 header.
func (h Header) MessageID() uint64 {
	return binary.LittleEndian.Uint64(h[24:32])
}

// SetMessageID sets the MessageID field of the SMB2 header.
func (h Header) SetMessageID(mid uint64) {
	binary.LittleEndian.PutUint64(h[24:32], mid)
}

// AsyncID returns the AsyncID field of the SMB2 header.
func (h Header) AsyncID() uint64 {
	return binary.LittleEndian.Uint64(h[32:40])
}

// SetAsyncID sets the AsyncID field of the SMB2 header.
func (h Header) SetAsyncID(aid uint64) {
	binary.LittleEndian.PutUint64(h[32:40], aid)
}

// TreeID returns the TreeID field of the SMB2 header.
func (h Header) TreeID() uint32 {
	return binary.LittleEndian.Uint32(h[36:40])
}

// SetTreeID sets the TreeID field of the SMB2 header.
func (h Header) SetTreeID(tid uint32) {
	binary.LittleEndian.PutUint32(h[36:40], tid)
}

// SessionID returns the SessionID field of the SMB2 header.
func (h Header) SessionID() uint64 {
	return binary.LittleEndian.Uint64(h[40:48])
}

// SetSessionID sets the SessionID field of the SMB2 header.
func (h Header) SetSessionID(sid uint64) {
	binary.LittleEndian.PutUint64(h[40:48], sid)
}

// Signature returns the Signature field of the SMB2 header.
func (h Header) Signature() []byte {
	signature := make([]byte, 16)
	copy(signature, h[48:64])
	return signature
}

// SetSignature sets the Signature field of the SMB2 header.
func (h Header) SetSignature(signature []byte) {
	copy(h[48:64], signature)
}

// WipeSignature clears the Signature field of the SMB2 header.
func (h Header) WipeSignature() {
	var zero [16]byte
	h.SetSignature(zero[:])
}

// EncryptionSignature returns the Signature field of the SMB2 TRANSFORM_HEADER.
func (h Header) EncryptionSignature() []byte {
	signature := make([]byte, 16)
	copy(signature, h[4:20])
	return signature
}

// SetEncryptionSignature sets the Signature field of the SMB2 TRANSFORM_HEADER.
func (h Header) SetEncryptionSignature(signature []byte) {
	copy(h[4:20], signature)
}

// WipeEncryptionSignature clears the Signature field of the SMB2 TRANSFORM_HEADER.
func (h Header) WipeEncryptionSignature() {
	var zero [16]byte
	h.SetEncryptionSignature(zero[:])
}

// Nonce returns the Nonce field of the SMB2 TRANSFORM_HEADER.
func (h Header) Nonce() []byte {
	nonce := make([]byte, 16)
	copy(nonce, h[20:36])
	return nonce
}

// SetNonce sets the Nonce field of the SMB2 TRANSFORM_HEADER.
func (h Header) SetNonce(nonce []byte) {
	n := make([]byte, 16)
	copy(n, nonce)
	copy(h[20:36], n)
}

// OriginalMessageSize returns the OriginalMessageSize field of the SMB2 TRANSFORM_HEADER.
func (h Header) OriginalMessageSize() uint32 {
	return binary.LittleEndian.Uint32(h[36:40])
}

// SetOriginalMessageSize sets the OriginalMessageSize field of the SMB2 TRANSFORM_HEADER.
func (h Header) SetOriginalMessageSize(size uint32) {
	binary.LittleEndian.PutUint32(h[36:40], size)
}

// EncryptionFlags returns the Flags field of the SMB2 TRANSFORM_HEADER.
func (h Header) EncryptionFlags() uint16 {
	return binary.LittleEndian.Uint16(h[42:44])
}

// SetEncryptionFlags sets the EncryptionFlags field of the SMB2 TRANSFORM_HEADER.
func (h Header) SetEncryptionFlags(flags uint16) {
	binary.LittleEndian.PutUint16(h[42:44], flags)
}

// TransformSessionID returns the SessionID field of the SMB2 TRANSFORM_HEADER.
func (h Header) TransformSessionID() uint64 {
	return binary.LittleEndian.Uint64(h[44:52])
}

// SetTransformSessionID sets the SessionID field of the SMB2 TRANSFORM_HEADER.
func (h Header) SetTransformSessionID(sid uint64) {
	binary.LittleEndian.PutUint64(h[44:52], sid)
}

// AssociatedData returns the encrypting-relevant data of the SMB2 TRANSFORM_HEADER.
func (h Header) AssociatedData() []byte {
	return h[20:52]
}

// ProtocolID returns the ProtocolID of the header.
func (h Header) ProtocolID() uint32 {
	return binary.LittleEndian.Uint32(h[:4])
}

// SetProtocolID sets the ProtocolID of the header.
func (h Header) SetProtocolID(id uint32) {
	binary.LittleEndian.PutUint32(h[:4], id)
}

// OriginalCompressedSegmentSize returns the OriginalCompressedSegmentSize field of
// the SMB2_COMPRESSION_TRANSFORM_HEADER.
func (h Header) OriginalCompressedSegmentSize() uint32 {
	return binary.LittleEndian.Uint32(h[4:8])
}

// SetOriginalCompressedSegmentSize sets the OriginalCompressedSegmentSize field of
// the SMB2_COMPRESSION_TRANSFORM_HEADER.
func (h Header) SetOriginalCompressedSegmentSize(size uint32) {
	binary.LittleEndian.PutUint32(h[4:8], size)
}

// CompressionAlgorithm returns the CompressionAlgorithm field of the
// SMB2_COMPRESSION_TRANSFORM_HEADER_UNCHAINED.
func (h Header) CompressionAlgorithm() uint16 {
	return binary.LittleEndian.Uint16(h[8:10])
}

// SetCompressionAlgorithm sets the CompressionAlgorithm field of the
// SMB2_COMPRESSION_TRANSFORM_HEADER_UNCHAINED.
func (h Header) SetCompressionAlgorithm(algo uint16) {
	binary.LittleEndian.PutUint16(h[8:10], algo)
}

// CompressionFlags returns the Flags field of the
// SMB2_COMPRESSION_TRANSFORM_HEADER_UNCHAINED.
func (h Header) CompressionFlags() uint16 {
	return binary.LittleEndian.Uint16(h[10:12])
}

// SetCompressionFlags sets the Flags field of the
// SMB2_COMPRESSION_TRANSFORM_HEADER_UNCHAINED.
func (h Header) SetCompressionFlags(flags uint16) {
	binary.LittleEndian.PutUint16(h[10:12], flags)
}

// Offset returns the Offset field of the
// SMB2_COMPRESSION_TRANSFORM_HEADER_UNCHAINED.
func (h Header) Offset() uint32 {
	return binary.LittleEndian.Uint32(h[12:16])
}

// SetOffset sets the Offset field of the
// SMB2_COMPRESSION_TRANSFORM_HEADER_UNCHAINED.
func (h Header) SetOffset(off uint32) {
	binary.LittleEndian.PutUint32(h[12:16], off)
}

// PayloadHeader is a typecast from SMB2_COMPRESSION_TRANSFORM_HEADER to
// SMB2_COMPRESSION_CHAINED_PAYLOAD_HEADER.
type PayloadHeader []byte

// CompressionAlgorithm returns the CompressionAlgorithm field of the
// SMB2_COMPRESSION_CHAINED_PAYLOAD_HEADER.
func (ph PayloadHeader) CompressionAlgorithm() uint16 {
	return binary.LittleEndian.Uint16(ph[:2])
}

// SetCompressionAlgorithm sets the CompressionAlgorithm field of the
// SMB2_COMPRESSION_CHAINED_PAYLOAD_HEADER.
func (ph PayloadHeader) SetCompressionAlgorithm(algo uint16) {
	binary.LittleEndian.PutUint16(ph[:2], algo)
}

// Flags returns the Flags field of the SMB2_COMPRESSION_CHAINED_PAYLOAD_HEADER.
func (ph PayloadHeader) Flags() uint16 {
	return binary.LittleEndian.Uint16(ph[2:4])
}

// SetFlags sets the Flags field of the SMB2_COMPRESSION_CHAINED_PAYLOAD_HEADER.
func (ph PayloadHeader) SetFlags(flags uint16) {
	binary.LittleEndian.PutUint16(ph[2:4], flags)
}

// Length returns the Length field of the SMB2_COMPRESSION_CHAINED_PAYLOAD_HEADER.
func (ph PayloadHeader) Length() uint32 {
	return binary.LittleEndian.Uint32(ph[4:8])
}

// SetLength sets the Length field of the SMB2_COMPRESSION_CHAINED_PAYLOAD_HEADER.
func (ph PayloadHeader) SetLength(length uint32) {
	binary.LittleEndian.PutUint32(ph[4:8], length)
}

// PatternV1 represents a SMB2_COMPRESSION_PATTERN_PAYLOAD_V1 structure.
type PatternV1 struct {
	Pattern     uint8
	Repetitions uint32
}

// Marshal converts a PatternV1 structure into a byte sequence.
func (p PatternV1) Marshal() []byte {
	b := make([]byte, 8)
	b[0] = p.Pattern
	binary.LittleEndian.PutUint32(b[4:8], p.Repetitions)
	return b
}

// Unmarshal converts a byte sequence into a PatternV1 structure.
func (p *PatternV1) Unmarshal(b []byte) error {
	if len(b) < 8 {
		return ErrWrongLength
	}
	p.Pattern = b[0]
	p.Repetitions = binary.LittleEndian.Uint32(b[4:8])
	return nil
}
