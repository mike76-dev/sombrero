package smb2

import (
	"encoding/binary"

	"github.com/mike76-dev/sombrero/utils"
)

const (
	SMB2TreeConnectRequestMinSize       = 8
	SMB2TreeConnectRequestStructureSize = 9

	SMB2TreeConnectResponseMinSize       = 16
	SMB2TreeConnectResponseStructureSize = 16

	SMB2TreeDisconnectRequestMinSize       = 4
	SMB2TreeDisconnectRequestStructureSize = 4

	SMB2TreeDisconnectResponseMinSize       = 4
	SMB2TreeDisconnectResponseStructureSize = 4
)

const (
	// Share types.
	SHARE_TYPE_DISK  = 0x01
	SHARE_TYPE_PIPE  = 0x02
	SHARE_TYPE_PRINT = 0x03
)

const (
	// Share flags.
	SHAREFLAG_MANUAL_CACHING              = 0x00000000
	SHAREFLAG_AUTO_CACHING                = 0x00000010
	SHAREFLAG_VDO_CACHING                 = 0x00000020
	SHAREFLAG_NO_CACHING                  = 0x00000030
	SHAREFLAG_DFS                         = 0x00000001
	SHAREFLAG_DFS_ROOT                    = 0x00000002
	SHAREFLAG_RESTRICT_EXCLUSIVE_OPENS    = 0x00000100
	SHAREFLAG_FORCE_SHARED_DELETE         = 0x00000200
	SHAREFLAG_ALLOW_NAMESPACE_CACHING     = 0x00000400
	SHAREFLAG_ACCESS_BASED_DIRECTORY_ENUM = 0x00000800
	SHAREFLAG_FORCE_LEVELII_OPLOCK        = 0x00001000
	SHAREFLAG_ENABLE_HASH_V1              = 0x00002000
	SHAREFLAG_ENABLE_HASH_V2              = 0x00004000
	SHAREFLAG_ENCRYPT_DATA                = 0x00008000
	SHAREFLAG_IDENTITY_REMOTING           = 0x00040000
	SHAREFLAG_COMPRESS_DATA               = 0x00100000
	SHAREFLAG_ISOLATED_TRANSPORT          = 0x00200000
)

const (
	// Share capabilities.
	SHARE_CAP_DFS                     = 0x00000008
	SHARE_CAP_CONTINUOUS_AVAILABILITY = 0x00000010
	SMB2_SHARE_CAP_SCALEOUT           = 0x00000020
	SHARE_CAP_CLUSTER                 = 0x00000040
	SHARE_CAP_ASYMMETRIC              = 0x00000080
	SHARE_CAP_REDIRECT_TO_OWNER       = 0x00000100
)

const (
	// File access flags.
	FILE_READ_DATA         = 0x00000001
	FILE_WRITE_DATA        = 0x00000002
	FILE_APPEND_DATA       = 0x00000004
	FILE_READ_EA           = 0x00000008
	FILE_WRITE_EA          = 0x00000010
	FILE_EXECUTE           = 0x00000020
	FILE_DELETE_CHILD      = 0x00000040
	FILE_READ_ATTRIBUTES   = 0x00000080
	FILE_WRITE_ATTRIBUTES  = 0x00000100
	DELETE                 = 0x00010000
	READ_CONTROL           = 0x00020000
	WRITE_DAC              = 0x00040000
	WRITE_OWNER            = 0x00080000
	SYNCHRONIZE            = 0x00100000
	ACCESS_SYSTEM_SECURITY = 0x01000000
	MAXIMUM_ALLOWED        = 0x02000000
	GENERIC_ALL            = 0x10000000
	GENERIC_EXECUTE        = 0x20000000
	GENERIC_WRITE          = 0x40000000
	GENERIC_READ           = 0x80000000
)

const (
	// Directory access flags.
	FILE_LIST_DIRECTORY   = 0x00000001
	FILE_ADD_FILE         = 0x00000002
	FILE_ADD_SUBDIRECTORY = 0x00000004
	FILE_TRAVERSE         = 0x00000020
)

// TreeConnectRequest represents an SMB2_TREE_CONNECT request.
type TreeConnectRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (tcr TreeConnectRequest) Validate(supportsMultiCredit bool) error {
	if err := Header(tcr.data).Validate(); err != nil {
		return err
	}

	if len(tcr.data) < SMB2HeaderSize+SMB2TreeConnectRequestMinSize {
		return ErrWrongLength
	}

	if tcr.structureSize() != SMB2TreeConnectRequestStructureSize {
		return ErrWrongFormat
	}

	// Validate CreditCharge.
	if supportsMultiCredit {
		sps := uint32(len(tcr.data) - SMB2HeaderSize - SMB2TreeConnectRequestMinSize)
		if tcr.Header().CreditCharge() == 0 {
			if sps > 65536 {
				return ErrInvalidParameter
			}
		} else if tcr.Header().CreditCharge() < uint16((sps-1)/65536)+1 {
			return ErrInvalidParameter
		}
	}

	return nil
}

// PathName returns the share path referenced by the Buffer field of the SMB2_CREATE request.
func (tcr TreeConnectRequest) PathName() string {
	off := binary.LittleEndian.Uint16(tcr.data[SMB2HeaderSize+4 : SMB2HeaderSize+6])
	length := binary.LittleEndian.Uint16(tcr.data[SMB2HeaderSize+6 : SMB2HeaderSize+8])
	if off+length > uint16(len(tcr.data)) {
		return ""
	}

	return utils.DecodeToString(tcr.data[off : off+length])
}

// TreeConnectResponse represents an SMB2_TREE_CONNECT response.
type TreeConnectResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_TREE_CONNECT response.
func (tcr *TreeConnectResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(tcr.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2TreeConnectResponseStructureSize)
}

// SetShareType sets the ShareType field of the SMB2_TREE_CONNECT response.
func (tcr *TreeConnectResponse) SetShareType(st uint8) {
	tcr.data[SMB2HeaderSize+2] = st
}

// SetShareFlags sets the ShareFlags field of the SMB2_TREE_CONNECT response.
func (tcr *TreeConnectResponse) SetShareFlags(flags uint32) {
	binary.LittleEndian.PutUint32(tcr.data[SMB2HeaderSize+4:SMB2HeaderSize+8], flags)
}

// SetCapabilities sets the Capabilities field of the SMB2_TREE_CONNECT response.
func (tcr *TreeConnectResponse) SetCapabilities(cap uint32) {
	binary.LittleEndian.PutUint32(tcr.data[SMB2HeaderSize+8:SMB2HeaderSize+12], cap)
}

// SetMaximalAccess sets the MaximalAccess field of the SMB2_TREE_CONNECT response.
func (tcr *TreeConnectResponse) SetMaximalAccess(ma uint32) {
	binary.LittleEndian.PutUint32(tcr.data[SMB2HeaderSize+12:SMB2HeaderSize+16], ma)
}

// FromRequest implements GenericResponse interface.
func (tcr *TreeConnectResponse) FromRequest(req GenericRequest) {
	tcr.Response.FromRequest(req)

	body := make([]byte, SMB2TreeConnectResponseMinSize)
	tcr.data = append(tcr.data, body...)

	tcr.setStructureSize()
	Header(tcr.data).SetNextCommand(0)
	if Header(tcr.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(tcr.data).SetCreditResponse(0)
	}
}

// Generate populates the fields of the SMB2_TREE_CONNECT response.
func (tcr *TreeConnectResponse) Generate(tid uint32, st uint8, access uint32, encrypt, compress bool) {
	Header(tcr.data).SetStatus(STATUS_OK)
	Header(tcr.data).SetTreeID(tid)
	tcr.SetShareType(st)
	tcr.SetMaximalAccess(access)
	var flags uint32
	if encrypt {
		flags |= SHAREFLAG_ENCRYPT_DATA
	}
	if compress {
		flags |= SHAREFLAG_COMPRESS_DATA
	}
	tcr.SetShareFlags(flags)
}

// TreeDisconnectRequest represents an SMB2_TREE_DISCONNECT request.
type TreeDisconnectRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (tdr TreeDisconnectRequest) Validate(_ bool) error {
	if err := Header(tdr.data).Validate(); err != nil {
		return err
	}

	if len(tdr.data) < SMB2HeaderSize+SMB2TreeDisconnectRequestMinSize {
		return ErrWrongLength
	}

	if tdr.structureSize() != SMB2TreeDisconnectRequestStructureSize {
		return ErrWrongFormat
	}

	return nil
}

// TreeDisconnectResponse represents an SMB2_TREE_DISCONNECT response.
type TreeDisconnectResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_TREE_DISCONNECT response.
func (tdr *TreeDisconnectResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(tdr.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2TreeDisconnectResponseStructureSize)
}

// FromRequest implements GenericResponse interface.
func (tdr *TreeDisconnectResponse) FromRequest(req GenericRequest) {
	tdr.Response.FromRequest(req)

	body := make([]byte, SMB2TreeDisconnectResponseMinSize)
	tdr.data = append(tdr.data, body...)

	tdr.setStructureSize()
	Header(tdr.data).SetNextCommand(0)
	Header(tdr.data).SetStatus(STATUS_OK)
	if Header(tdr.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(tdr.data).SetCreditResponse(0)
	}
}
