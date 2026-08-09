package smb2

import (
	"encoding/binary"
	"errors"
	"time"

	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/utils"
)

const (
	SMBNegotiateRequestMinSize = 5 // Enough to validate an SMB_COM_NEGOTIATE request

	SMB2NegotiateRequestMinSize       = 36
	SMB2NegotiateRequestStructureSize = 36

	SMB2NegotiateResponseMinSize       = 64
	SMB2NegotiateResponseStructureSize = 65
)

const (
	MaxTransactSize = 1048576 * 8 // 8MiB
	MaxReadSize     = 1048576 * 8 // 8MiB
	MaxWriteSize    = 1048576 * 8 // 8MiB
)

const (
	// SMB dialects.
	SMB_DIALECT_1     = "NT LM 0.12"
	SMB_DIALECT_2     = "SMB 2.002"
	SMB_DIALECT_MULTI = "SMB 2.???"

	// SMB2 dialects.
	SMB_DIALECT_202         = 0x0202
	SMB_DIALECT_21          = 0x0210
	SMB_DIALECT_30          = 0x0300
	SMB_DIALECT_302         = 0x0302
	SMB_DIALECT_311         = 0x0311
	SMB_DIALECT_MULTICREDIT = 0x02ff
	SMB_DIALECT_UNKNOWN     = 0xffff
)

const (
	// MinSupportedDialect is the minimum dialect that is supported by the server.
	MinSupportedDialect = SMB_DIALECT_202

	// MaxSupportedDialect is the maximum dialect that is supported by the server.
	MaxSupportedDialect = SMB_DIALECT_311
)

const (
	// Security modes.
	NEGOTIATE_SIGNING_ENABLED  = 0x0001
	NEGOTIATE_SIGNING_REQUIRED = 0x0002
)

const (
	// Capabilities.
	GLOBAL_CAP_DFS                = 0x00000001
	GLOBAL_CAP_LEASING            = 0x00000002
	GLOBAL_CAP_LARGE_MTU          = 0x00000004
	GLOBAL_CAP_MULTI_CHANNEL      = 0x00000008
	GLOBAL_CAP_PERSISTENT_HANDLES = 0x00000010
	GLOBAL_CAP_DIRECTORY_LEASING  = 0x00000020
	GLOBAL_CAP_ENCRYPTION         = 0x00000040
	GLOBAL_CAP_NOTIFICATIONS      = 0x00000080
)

const (
	// Negotiate context types.
	PREAUTH_INTEGRITY_CAPABILITIES = 0x0001
	ENCRYPTION_CAPABILITIES        = 0x0002
	COMPRESSION_CAPABILITIES       = 0x0003
	NETNAME_NEGOTIATE_CONTEXT_ID   = 0x0005
	TRANSPORT_CAPABILITIES         = 0x0006
	RDMA_TRANSFORM_CAPABILITIES    = 0x0007
	SIGNING_CAPABILITIES           = 0x0008
	CONTEXTTYPE_RESERVED           = 0x0100
)

const (
	// Hash algorithms.
	SHA_512 = 0x0001
)

const (
	// Encryption ciphers.
	AES_128_CCM = 0x0001
	AES_128_GCM = 0x0002
	AES_256_CCM = 0x0003
	AES_256_GCM = 0x0004
)

const (
	// Compression capabilities.
	COMPRESSION_CAPABILITIES_FLAG_NONE    = 0x00000000
	COMPRESSION_CAPABILITIES_FLAG_CHAINED = 0x00000001
)

const (
	// Compression algorithms.
	COMPRESSION_NONE         = 0x0000
	COMPRESSION_LZNT1        = 0x0001
	COMPRESSION_LZ77         = 0x0002
	COMPRESSION_LZ77_HUFFMAN = 0x0003
	COMPRESSION_PATTERN_V1   = 0x0004
	COMPRESSION_LZ4          = 0x0005
)

const (
	// Transport capabilities.
	ACCEPT_TRANSPORT_LEVEL_SECURITY = 0x00000001
)

const (
	// RDMA transform capabilities.
	RDMA_TRANSFORM_NONE       = 0x0000
	RDMA_TRANSFORM_ENCRYPTION = 0x0001
	RDMA_TRANSFORM_SIGNING    = 0x0002
)

const (
	// Signing capabilities.
	HMAC_SHA256 = 0x0000
	AES_CMAC    = 0x0001
	AES_GMAC    = 0x0002
)

var (
	ErrDialectNotSupported = errors.New("dialect not supported")
	ErrInvalidParameter    = errors.New("wrong parameter supplied")
)

// Is3X returns true if the dialect belongs to the 3.x family.
func Is3X(dialect uint16) bool {
	return dialect != SMB_DIALECT_UNKNOWN && dialect >= SMB_DIALECT_30
}

// DialectCapabilities is the set of capabilities a dialect may carry: every flag [MS-SMB2] 3.3.5.4
// allows the server to put in the Capabilities field of a NEGOTIATE response on that dialect, and
// no other. It says what the dialect permits rather than what any particular server has - what the
// server itself supports is a separate set, and a capability is advertised only if it is in both.
func DialectCapabilities(dialect uint16) uint32 {
	// DFS is the one capability that predates the rest and belongs to every dialect.
	caps := uint32(GLOBAL_CAP_DFS)

	// Leasing and large MTU arrived with 2.1: both are held against the dialect being 2.0.2 and
	// nothing else.
	if dialect != SMB_DIALECT_202 {
		caps |= GLOBAL_CAP_LEASING | GLOBAL_CAP_LARGE_MTU
	}

	if Is3X(dialect) {
		caps |= GLOBAL_CAP_MULTI_CHANNEL | GLOBAL_CAP_PERSISTENT_HANDLES | GLOBAL_CAP_DIRECTORY_LEASING
	}

	// Encryption is the exception to 3.x taking everything: 3.1.1 settles a cipher in a negotiate
	// context instead, so the capability flag belongs to 3.0 and 3.0.2 alone.
	if dialect == SMB_DIALECT_30 || dialect == SMB_DIALECT_302 {
		caps |= GLOBAL_CAP_ENCRYPTION
	}

	return caps
}

// NegotiateRequest represents an SMB2_NEGOTIATE request.
type NegotiateRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (nr NegotiateRequest) Validate(supportsMultiCredit bool) error {
	if err := Header(nr.data).Validate(); err != nil {
		return err
	}

	if Header(nr.data).IsSmb() { // SMB_COM_NEGOTIATE
		if len(nr.data) < SMBHeaderSize+SMBNegotiateRequestMinSize {
			return ErrWrongLength
		}

		if nr.data[SMBHeaderSize] != 0 {
			return ErrWrongFormat
		}

		if binary.LittleEndian.Uint16(nr.data[SMBHeaderSize+1:SMBHeaderSize+3]) < 2 {
			return ErrWrongFormat
		}

		dialects := utils.NullTerminatedToStrings(nr.data[SMBHeaderSize+4:])
		if len(dialects) == 0 {
			return ErrInvalidParameter
		}

		var supported bool
		for _, d := range dialects {
			switch d {
			case SMB_DIALECT_2:
				supported = true
			case SMB_DIALECT_MULTI:
				if MaxSupportedDialect != SMB_DIALECT_202 {
					supported = true
				}
			}
			if supported {
				break
			}
		}

		if !supported {
			return ErrDialectNotSupported
		}

		return nil
	}

	if len(nr.data) < SMB2HeaderSize+SMB2NegotiateRequestMinSize {
		return ErrWrongLength
	}

	if nr.structureSize() != SMB2NegotiateRequestStructureSize {
		return ErrWrongFormat
	}

	if Header(nr.data).IsFlagSet(FLAGS_SIGNED) {
		return ErrInvalidParameter
	}

	dialectCount := binary.LittleEndian.Uint16(nr.data[SMB2HeaderSize+2 : SMB2HeaderSize+4])
	if dialectCount == 0 {
		return ErrInvalidParameter
	}

	if len(nr.data) < SMB2HeaderSize+SMB2NegotiateRequestMinSize+2*int(dialectCount) {
		return ErrWrongLength
	}

	var supported bool
	for i := 0; i < int(dialectCount); i++ {
		dialect := binary.LittleEndian.Uint16(nr.data[SMB2HeaderSize+SMB2NegotiateRequestMinSize+i*2 : SMB2HeaderSize+SMB2NegotiateRequestMinSize+i*2+2])
		if dialect >= MinSupportedDialect && dialect <= MaxSupportedDialect {
			supported = true
			break
		}
	}

	if !supported {
		return ErrDialectNotSupported
	}

	// Validate CreditCharge.
	if supportsMultiCredit {
		sps := uint32(len(nr.data) - SMB2HeaderSize - SMB2NegotiateRequestMinSize)
		ers := uint32(74) //TODO revisit in 3.1.1
		if nr.Header().CreditCharge() == 0 {
			if sps > 65536 || ers > 65536 {
				return ErrInvalidParameter
			}
		} else if nr.Header().CreditCharge() < uint16((max(sps, ers)-1)/65536)+1 {
			return ErrInvalidParameter
		}
	}

	return nil
}

// SecurityMode returns the SecurityMode field of the SMB2_NEGOTIATE request.
func (nr NegotiateRequest) SecurityMode() uint16 {
	return binary.LittleEndian.Uint16(nr.data[SMB2HeaderSize+4 : SMB2HeaderSize+6])
}

// Capabilities returns the Capabilities field of the SMB2_NEGOTIATE request.
func (nr NegotiateRequest) Capabilities() uint32 {
	return binary.LittleEndian.Uint32(nr.data[SMB2HeaderSize+8 : SMB2HeaderSize+12])
}

// ClientGuid returns the ClientGuid field of the SMB2_NEGOTIATE request.
func (nr NegotiateRequest) ClientGuid() []byte {
	guid := make([]byte, 16)
	copy(guid, nr.data[SMB2HeaderSize+12:SMB2HeaderSize+28])
	return guid
}

// MaxCommonDialect returns the greatest dialect supported both by the client and the server.
func (nr NegotiateRequest) MaxCommonDialect() uint16 {
	var max uint16
	if Header(nr.data).IsSmb() { // SMB_COM_NEGOTIATE
		dialects := utils.NullTerminatedToStrings(nr.data[SMBHeaderSize+4:])
		for _, d := range dialects {
			switch d {
			case SMB_DIALECT_2:
				if max < SMB_DIALECT_202 && MinSupportedDialect <= SMB_DIALECT_202 {
					max = SMB_DIALECT_202
				}
			case SMB_DIALECT_MULTI:
				if max < SMB_DIALECT_MULTICREDIT && MaxSupportedDialect >= SMB_DIALECT_21 {
					max = SMB_DIALECT_MULTICREDIT
				}
			}
		}
	} else {
		dialectCount := binary.LittleEndian.Uint16(nr.data[SMB2HeaderSize+2 : SMB2HeaderSize+4])
		for i := 0; i < int(dialectCount); i++ {
			dialect := binary.LittleEndian.Uint16(nr.data[SMB2HeaderSize+SMB2NegotiateRequestMinSize+i*2 : SMB2HeaderSize+SMB2NegotiateRequestMinSize+i*2+2])
			if dialect > max && dialect >= MinSupportedDialect && dialect <= MaxSupportedDialect {
				max = dialect
			}
		}
	}

	return max
}

// Dialects returns the Dialects field of the SMB2_NEGOTIATE request.
func (nr NegotiateRequest) Dialects() []uint16 {
	dialectCount := binary.LittleEndian.Uint16(nr.data[SMB2HeaderSize+2 : SMB2HeaderSize+4])
	var dialects []uint16
	for i := range dialectCount {
		dialects = append(dialects, binary.LittleEndian.Uint16(nr.data[SMB2HeaderSize+SMB2NegotiateRequestMinSize+i*2:SMB2HeaderSize+SMB2NegotiateRequestMinSize+i*2+2]))
	}
	return dialects
}

// NegotiateContexts returns a list of NEGOTIATE_CONTEXT values.
func (nr NegotiateRequest) NegotiateContexts() []NegotiateContext {
	offset := binary.LittleEndian.Uint32(nr.data[SMB2HeaderSize+28 : SMB2HeaderSize+32])
	count := binary.LittleEndian.Uint16(nr.data[SMB2HeaderSize+32 : SMB2HeaderSize+34])
	var ncs []NegotiateContext
	for range count {
		if len(nr.data) < int(offset)+4 {
			return ncs
		}
		t := binary.LittleEndian.Uint16(nr.data[offset : offset+2])
		l := binary.LittleEndian.Uint16(nr.data[offset+2 : offset+4])
		if len(nr.data) < int(offset)+int(l)+8 {
			return ncs
		}
		data := make([]byte, l)
		copy(data, nr.data[offset+8:offset+uint32(l)+8])
		ncs = append(ncs, NegotiateContext{t, data})
		offset += uint32(utils.Roundup(int(l), 8)) + 8
	}
	return ncs
}

// NegotiateContext represents a NEGOTIATE_CONTEXT value.
type NegotiateContext struct {
	ContextType uint16
	Data        []byte
}

// GetPreauthIntegrityCapabilities returns the SMB2_PREAUTH_INTEGRITY_CAPABILITIES context,
// if present.
func GetPreauthIntegrityCapabilities(ncs []NegotiateContext) (hashAlgos []uint16, salt []byte, err error) {
	for _, nc := range ncs {
		if nc.ContextType == PREAUTH_INTEGRITY_CAPABILITIES {
			if len(hashAlgos) != 0 {
				return nil, nil, ErrInvalidParameter // Exactly one context is allowed
			}
			if len(nc.Data) < 6 { // At least one HashAlgorithmID must be present
				return nil, nil, ErrInvalidParameter
			}
			count := int(binary.LittleEndian.Uint16(nc.Data[:2]))
			length := int(binary.LittleEndian.Uint16(nc.Data[2:4]))
			if count == 0 || len(nc.Data) < 2*count+length+4 {
				return nil, nil, ErrInvalidParameter
			}
			salt = make([]byte, length)
			copy(salt, nc.Data[4+2*count:4+2*count+length])
			for i := range count {
				hashAlgos = append(hashAlgos, binary.LittleEndian.Uint16(nc.Data[4+i*2:6+i*2]))
			}
		}
	}
	if len(hashAlgos) == 0 {
		return nil, nil, ErrInvalidParameter // Exactly one context is allowed
	}
	return
}

// GetEncryptionCapabilities returns the SMB2_ENCRYPTION_CAPABILITIES context, if present.
func GetEncryptionCapabilities(ncs []NegotiateContext) (ciphers []uint16, err error) {
	for _, nc := range ncs {
		if nc.ContextType == ENCRYPTION_CAPABILITIES {
			if len(ciphers) != 0 {
				return nil, ErrInvalidParameter // Maximum one context is allowed
			}
			if len(nc.Data) < 4 {
				return nil, ErrInvalidParameter
			}
			count := int(binary.LittleEndian.Uint16(nc.Data[:2]))
			if count == 0 || len(nc.Data) < 2*count+2 {
				return nil, ErrInvalidParameter
			}
			for i := range count {
				ciphers = append(ciphers, binary.LittleEndian.Uint16(nc.Data[2+i*2:4+i*2]))
			}
		}
	}
	return
}

// GetCompressionCapabilities returns the SMB2_COMPRESSION_CAPABILITIES context, if present.
func GetCompressionCapabilities(ncs []NegotiateContext) (flags uint32, algos []uint16, err error) {
	for _, nc := range ncs {
		if nc.ContextType == COMPRESSION_CAPABILITIES {
			if len(algos) != 0 {
				return 0, nil, ErrInvalidParameter // Maximum one context is allowed
			}
			if len(nc.Data) < 8 {
				return 0, nil, ErrInvalidParameter
			}
			count := int(binary.LittleEndian.Uint16(nc.Data[:2]))
			if count == 0 || len(nc.Data) < 2*count+8 {
				return 0, nil, ErrInvalidParameter
			}
			flags = binary.LittleEndian.Uint32(nc.Data[4:8])
			for i := range count {
				algos = append(algos, binary.LittleEndian.Uint16(nc.Data[8+i*2:10+i*2]))
			}
		}
	}
	return
}

// NetName returns the SMB2_NETNAME_NEGOTIATE_CONTEXT_ID context, if present.
func NetName(ncs []NegotiateContext) string {
	for _, nc := range ncs {
		if nc.ContextType == NETNAME_NEGOTIATE_CONTEXT_ID {
			return utils.DecodeToString(nc.Data)
		}
	}
	return ""
}

// TransportCapabilities returns the SMB2_TRANSPORT_CAPABILITIES context, if present.
func TransportCapabilities(ncs []NegotiateContext) (flags uint32, err error) {
	for _, nc := range ncs {
		if nc.ContextType == TRANSPORT_CAPABILITIES {
			if len(nc.Data) < 4 {
				return 0, ErrInvalidParameter
			}
			return binary.LittleEndian.Uint32(nc.Data[:4]), nil
		}
	}
	return 0, nil
}

// RDMATransformCapabilities returns the SMB2_RDMA_TRANSFORM_CAPABILITIES context,
// if present.
func RDMATransformCapabilities(ncs []NegotiateContext) (ids []uint16, err error) {
	for _, nc := range ncs {
		if nc.ContextType == RDMA_TRANSFORM_CAPABILITIES {
			if len(ids) != 0 {
				return nil, ErrInvalidParameter // Maximum one context is allowed
			}
			if len(nc.Data) < 10 { // At least one ID must be present
				return nil, ErrInvalidParameter
			}
			count := int(binary.LittleEndian.Uint16(nc.Data[:2]))
			if count == 0 || len(nc.Data) < 2*count+8 {
				return nil, ErrInvalidParameter
			}
			for i := range count {
				ids = append(ids, binary.LittleEndian.Uint16(nc.Data[8+i*2:10+i*2]))
			}
		}
	}
	return
}

// GetSigningCapabilities returns the SMB2_SIGNING_CAPABILITIES context, if present.
func GetSigningCapabilities(ncs []NegotiateContext) (algos []uint16, err error) {
	for _, nc := range ncs {
		if nc.ContextType == SIGNING_CAPABILITIES {
			if len(algos) != 0 {
				return nil, ErrInvalidParameter // Maximum one context is allowed
			}
			if len(nc.Data) < 4 { // At least one algo must be present
				return nil, ErrInvalidParameter
			}
			count := int(binary.LittleEndian.Uint16(nc.Data[:2]))
			if count == 0 || len(nc.Data) < 2*count+2 {
				return nil, ErrInvalidParameter
			}
			for i := range count {
				algos = append(algos, binary.LittleEndian.Uint16(nc.Data[2+i*2:4+i*2]))
			}
		}
	}
	return
}

// NegotiateResponse represents an SMB2_NEGOTIATE response.
type NegotiateResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_NEGOTIATE response.
func (nr *NegotiateResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(nr.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2NegotiateResponseStructureSize)
}

// SetSecurityMode sets the SecurityMode field of the SMB2_NEGOTIATE response.
func (nr *NegotiateResponse) SetSecurityMode(sm uint16) {
	binary.LittleEndian.PutUint16(nr.data[SMB2HeaderSize+2:SMB2HeaderSize+4], sm)
}

// SetDialectRevision sets the DialectRevision field of the SMB2_NEGOTIATE response.
func (nr *NegotiateResponse) SetDialectRevision(dialect uint16) {
	binary.LittleEndian.PutUint16(nr.data[SMB2HeaderSize+4:SMB2HeaderSize+6], dialect)
}

// SetServerGuid sets the ServerGuid field of the SMB2_NEGOTIATE response.
func (nr *NegotiateResponse) SetServerGuid(guid []byte) {
	copy(nr.data[SMB2HeaderSize+8:SMB2HeaderSize+24], guid)
}

// SetCapabilities sets the Capabilities field of the SMB2_NEGOTIATE response.
func (nr *NegotiateResponse) SetCapabilities(cap uint32) {
	binary.LittleEndian.PutUint32(nr.data[SMB2HeaderSize+24:SMB2HeaderSize+28], cap)
}

// SetMaxTransactSize sets the MaxTransactSize field of the SMB2_NEGOTIATE response.
func (nr *NegotiateResponse) SetMaxTransactSize(size uint32) {
	binary.LittleEndian.PutUint32(nr.data[SMB2HeaderSize+28:SMB2HeaderSize+32], size)
}

// SetMaxReadSize sets the MaxReadSize field of the SMB2_NEGOTIATE response.
func (nr *NegotiateResponse) SetMaxReadSize(size uint32) {
	binary.LittleEndian.PutUint32(nr.data[SMB2HeaderSize+32:SMB2HeaderSize+36], size)
}

// SetMaxWriteSize sets the M<axWriteSize field of the SMB2_NEGOTIATE response.
func (nr *NegotiateResponse) SetMaxWriteSize(size uint32) {
	binary.LittleEndian.PutUint32(nr.data[SMB2HeaderSize+36:SMB2HeaderSize+40], size)
}

// SetSystemTime sets the SystemTime field of the SMB2_NEGOTIATE response.
func (nr *NegotiateResponse) SetSystemTime(t time.Time) {
	binary.LittleEndian.PutUint64(nr.data[SMB2HeaderSize+40:SMB2HeaderSize+48], utils.UnixToFiletime(t))
}

// SetSecurityBuffer sets the Buffer field of the SMB2_NEGOTIATE response.
func (nr *NegotiateResponse) SetSecurityBuffer(buf []byte) {
	binary.LittleEndian.PutUint16(nr.data[SMB2HeaderSize+56:SMB2HeaderSize+58], SMB2HeaderSize+SMB2NegotiateResponseMinSize)
	binary.LittleEndian.PutUint16(nr.data[SMB2HeaderSize+58:SMB2HeaderSize+60], uint16(len(buf)))
	nr.data = nr.data[:SMB2HeaderSize+SMB2NegotiateResponseMinSize]
	nr.data = append(nr.data, buf...)
}

// NewNegotiateResponse generates an SMB2_NEGOTIATE response to an SMB_COM_NEGOTIATE request.
func NewNegotiateResponse(serverGuid []byte, ns *ntlm.Server, dialect uint16, capabilities uint32, maxTransactSize, maxReadSize, maxWriteSize uint32) *NegotiateResponse {
	nr := &NegotiateResponse{}
	nr.data = make([]byte, SMB2HeaderSize+SMB2NegotiateResponseMinSize)
	h := NewHeader(nr.data)
	h.SetCommand(SMB2_NEGOTIATE)
	h.SetStatus(STATUS_OK)
	h.SetFlags(FLAGS_SERVER_TO_REDIR)
	nr.Generate(serverGuid, ns, dialect, capabilities, maxTransactSize, maxReadSize, maxWriteSize)
	return nr
}

// FromRequest implements GenericResponse interface.
func (nr *NegotiateResponse) FromRequest(req GenericRequest) {
	nr.Response.FromRequest(req)

	body := make([]byte, SMB2NegotiateResponseMinSize)
	nr.data = append(nr.data, body...)
	Header(nr.data).SetNextCommand(0)

	r, ok := req.(NegotiateRequest)
	if !ok {
		panic("not a negotiate request")
	}

	if NegotiateRequest(r).SecurityMode()&NEGOTIATE_SIGNING_REQUIRED > 0 {
		nr.SetSecurityMode(r.SecurityMode() | NEGOTIATE_SIGNING_REQUIRED)
	}
}

// Generate populates the fields of the SMB2_NEGOTIATE response.
func (nr *NegotiateResponse) Generate(serverGuid []byte, ns *ntlm.Server, dialect uint16, capabilities uint32, maxTransactSize, maxReadSize, maxWriteSize uint32) {
	token, err := ns.Negotiate()
	if err != nil {
		panic(err)
	}

	if Header(nr.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(nr.data).SetCreditResponse(0)
	} else {
		Header(nr.data).SetCreditResponse(1)
	}

	nr.setStructureSize()
	nr.SetDialectRevision(dialect)
	nr.SetSecurityMode(NEGOTIATE_SIGNING_ENABLED)
	nr.SetCapabilities(capabilities)
	nr.SetServerGuid(serverGuid)
	nr.SetMaxTransactSize(maxTransactSize)
	nr.SetMaxReadSize(maxReadSize)
	nr.SetMaxWriteSize(maxWriteSize)
	nr.SetSystemTime(time.Now())

	nr.SetSecurityBuffer(token)
}

// AddNegotiateContexts appends a list of marshalled negotiate contexts to the response.
// A security buffer needs to be added already.
func (nr *NegotiateResponse) AddNegotiateContexts(blobs [][]byte) {
	var ncs []byte
	for i := range blobs {
		blob := blobs[i]
		size := len(blob)
		if i < len(blobs)-1 {
			size = utils.Roundup(size, 8)
		}
		paddedBlob := make([]byte, size)
		copy(paddedBlob, blob)
		ncs = append(ncs, paddedBlob...)
	}
	padding := make([]byte, utils.Roundup(len(nr.data), 8)-len(nr.data))
	nr.data = append(nr.data, padding...)
	binary.LittleEndian.PutUint16(nr.data[SMB2HeaderSize+6:SMB2HeaderSize+8], uint16(len(blobs)))
	binary.LittleEndian.PutUint32(nr.data[SMB2HeaderSize+60:SMB2HeaderSize+64], uint32(len(nr.data)))
	nr.data = append(nr.data, ncs...)
}

// PreauthIntegrityCapabilities forms an SMB2_PREAUTH_INTEGRITY_CAPABILITIES
// response context.
func PreauthIntegrityCapabilities(salt []byte) []byte {
	data := make([]byte, 6+len(salt))
	binary.LittleEndian.PutUint16(data[:2], 1)
	binary.LittleEndian.PutUint16(data[2:4], uint16(len(salt)))
	binary.LittleEndian.PutUint16(data[4:6], SHA_512)
	if len(salt) > 0 {
		copy(data[6:], salt)
	}
	ctx := make([]byte, 8)
	binary.LittleEndian.PutUint16(ctx[:2], PREAUTH_INTEGRITY_CAPABILITIES)
	binary.LittleEndian.PutUint16(ctx[2:4], uint16(len(data)))
	return append(ctx, data...)
}

// EncryptionCapabilities forms an SMB2_ENCRYPTION_CAPABILITIES response context.
func EncryptionCapabilities(cipher uint16) []byte {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint16(data[:2], 1)
	binary.LittleEndian.PutUint16(data[2:4], cipher)
	ctx := make([]byte, 8)
	binary.LittleEndian.PutUint16(ctx[:2], ENCRYPTION_CAPABILITIES)
	binary.LittleEndian.PutUint16(ctx[2:4], uint16(len(data)))
	return append(ctx, data...)
}

// CompressionCapabilities forms an SMB2_COMPRESSION_CAPABILITIES response context.
func CompressionCapabilities(flags uint32, algos []uint16) []byte {
	data := make([]byte, 8+2*len(algos))
	binary.LittleEndian.PutUint16(data[:2], uint16(len(algos)))
	binary.LittleEndian.PutUint32(data[4:8], flags)
	for i, algo := range algos {
		binary.LittleEndian.PutUint16(data[8+i*2:10+i*2], algo)
	}
	ctx := make([]byte, 8)
	binary.LittleEndian.PutUint16(ctx[:2], COMPRESSION_CAPABILITIES)
	binary.LittleEndian.PutUint16(ctx[2:4], uint16(len(data)))
	return append(ctx, data...)
}

// SigningCapabilities forms an SMB2_SIGNING_CAPABILITIES response context.
func SigningCapabilities(algo uint16) []byte {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint16(data[:2], 1)
	binary.LittleEndian.PutUint16(data[2:4], algo)
	ctx := make([]byte, 8)
	binary.LittleEndian.PutUint16(ctx[:2], SIGNING_CAPABILITIES)
	binary.LittleEndian.PutUint16(ctx[2:4], uint16(len(data)))
	return append(ctx, data...)
}
