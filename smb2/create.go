package smb2

import (
	"encoding/binary"
	"errors"
	"time"

	"github.com/mike76-dev/sombrero/utils"
)

var (
	ErrNotSupported = errors.New("not supported")
)

const (
	SMB2CreateRequestMinSize       = 57
	SMB2CreateRequestStructureSize = 57

	SMB2CreateResponseMinSize       = 88
	SMB2CreateResponseStructureSize = 89
)

const (
	// Oplock level.
	OPLOCK_LEVEL_NONE      = 0x00
	OPLOCK_LEVEL_II        = 0x01
	OPLOCK_LEVEL_EXCLUSIVE = 0x08
	OPLOCK_LEVEL_BATCH     = 0x09
	OPLOCK_LEVEL_LEASE     = 0xff
)

const (
	// Impersonation level.
	IMPERSONATION_ANONYMOUS      = 0x00000000
	IMPERSONATION_IDENTIFICATION = 0x00000001
	IMPERSONATION_IMPERSONATION  = 0x00000002
	IMPERSONATION_DELEGATE       = 0x00000003
)

const (
	// Share access.
	FILE_SHARE_READ   = 0x00000001
	FILE_SHARE_WRITE  = 0x00000002
	FILE_SHARE_DELETE = 0x00000004
)

const (
	// Create disposition.
	FILE_SUPERSEDE    = 0x00000000
	FILE_OPEN         = 0x00000001
	FILE_CREATE       = 0x00000002
	FILE_OPEN_IF      = 0x00000003
	FILE_OVERWRITE    = 0x00000004
	FILE_OVERWRITE_IF = 0x00000005
)

const (
	// Create options.
	FILE_DIRECTORY_FILE            = 0x00000001
	FILE_WRITE_THROUGH             = 0x00000002
	FILE_SEQUENTIAL_ONLY           = 0x00000004
	FILE_NO_INTERMEDIATE_BUFFERING = 0x00000008
	FILE_SYNCHRONOUS_IO_ALERT      = 0x00000010
	FILE_SYNCHRONOUS_IO_NONALERT   = 0x00000020
	FILE_NON_DIRECTORY_FILE        = 0x00000040
	FILE_COMPLETE_IF_OPLOCKED      = 0x00000100
	FILE_NO_EA_KNOWLEDGE           = 0x00000200
	FILE_OPEN_REMOTE_INSTANCE      = 0x00000400
	FILE_RANDOM_ACCESS             = 0x00000800
	FILE_DELETE_ON_CLOSE           = 0x00001000
	FILE_OPEN_BY_FILE_ID           = 0x00002000
	FILE_OPEN_FOR_BACKUP_INTENT    = 0x00004000
	FILE_NO_COMPRESSION            = 0x00008000
	FILE_OPEN_REQUIRING_OPLOCK     = 0x00010000
	FILE_DISALLOW_EXCLUSIVE        = 0x00020000
	FILE_RESERVE_OPFILTER          = 0x00100000
	FILE_OPEN_REPARSE_POINT        = 0x00200000
	FILE_OPEN_NO_RECALL            = 0x00400000
	FILE_OPEN_FOR_FREE_SPACE_QUERY = 0x00800000
)

const (
	// File attributes.
	FILE_ATTRIBUTE_READONLY              = 0x00000001
	FILE_ATTRIBUTE_HIDDEN                = 0x00000002
	FILE_ATTRIBUTE_SYSTEM                = 0x00000004
	FILE_ATTRIBUTE_DIRECTORY             = 0x00000010
	FILE_ATTRIBUTE_ARCHIVE               = 0x00000020
	FILE_ATTRIBUTE_NORMAL                = 0x00000080
	FILE_ATTRIBUTE_TEMPORARY             = 0x00000100
	FILE_ATTRIBUTE_SPARSE_FILE           = 0x00000200
	FILE_ATTRIBUTE_REPARSE_POINT         = 0x00000400
	FILE_ATTRIBUTE_COMPRESSED            = 0x00000800
	FILE_ATTRIBUTE_OFFLINE               = 0x00001000
	FILE_ATTRIBUTE_NOT_CONTENT_INDEXED   = 0x00002000
	FILE_ATTRIBUTE_ENCRYPTED             = 0x00004000
	FILE_ATTRIBUTE_INTEGRITY_STREAM      = 0x00008000
	FILE_ATTRIBUTE_NO_SCRUB_DATA         = 0x00020000
	FILE_ATTRIBUTE_RECALL_ON_OPEN        = 0x00040000
	FILE_ATTRIBUTE_PINNED                = 0x00080000
	FILE_ATTRIBUTE_UNPINNED              = 0x00100000
	FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS = 0x00400000
)

const (
	// Create context.
	CREATE_EA_BUFFER                        = 0x45787441
	CREATE_SD_BUFFER                        = 0x53656344
	CREATE_DURABLE_HANDLE_REQUEST           = 0x44486e51
	CREATE_DURABLE_HANDLE_RECONNECT         = 0x44486e43
	CREATE_ALLOCATION_SIZE                  = 0x416c5369
	CREATE_QUERY_MAXIMAL_ACCESS_REQUEST     = 0x4d784163
	CREATE_TIMEWAROP_TOKEN                  = 0x54577270
	CREATE_QUERY_ON_DISK_ID                 = 0x51466964
	CREATE_REQUEST_LEASE                    = 0x52714c73
	SMB2_CREATE_REQUEST_LEASE_V2            = 0x52714c73
	SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2   = 0x44483251
	SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2 = 0x44483243
	SMB2_CREATE_APP_INSTANCE_ID             = 0x45BCA66AEFA7F74A9008FA462E144D74
	SMB2_CREATE_APP_INSTANCE_VERSION        = 0xB982D0B73B56074FA07B524A8116A010
	SVHDX_OPEN_DEVICE_CONTEXT               = 0x9CCBCF9E04C1E643980E158DA1F6EC83
	SMB2_CREATECONTEXT_RESERVED             = 0x93AD25509CB411E7B42383DE968BCD7C
)

const (
	// Oplock status.
	OplockNone int = iota
	OplockHeld
	OplockBreaking
)

const (
	// Create action.
	FILE_SUPERSEDED  = 0x00000000
	FILE_OPENED      = 0x00000001
	FILE_CREATED     = 0x00000002
	FILE_OVERWRITTEN = 0x00000003
)

const (
	BytesPerSector = 4 * 1024 * 1024 // 4MiB
)

// CreateRequest represents an SMB2_CREATE request.
type CreateRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (cr CreateRequest) Validate(supportsMultiCredit bool) error {
	if err := Header(cr.data).Validate(); err != nil {
		return err
	}

	if len(cr.data) < SMB2HeaderSize+SMB2CreateRequestMinSize {
		return ErrWrongLength
	}

	if cr.structureSize() != SMB2CreateRequestStructureSize {
		return ErrWrongFormat
	}

	cd := cr.CreateDisposition()
	co := cr.CreateOptions()
	if co&uint32(FILE_DIRECTORY_FILE) > 0 {
		if cd != FILE_CREATE && cd != FILE_OPEN && cd != FILE_OPEN_IF {
			return ErrInvalidParameter
		}

		if co&uint32(FILE_OPEN_BY_FILE_ID) > 0 || co&uint32(FILE_RESERVE_OPFILTER) > 0 {
			return ErrNotSupported
		}
	}

	off := binary.LittleEndian.Uint16(cr.data[SMB2HeaderSize+44 : SMB2HeaderSize+46])
	length := binary.LittleEndian.Uint16(cr.data[SMB2HeaderSize+46 : SMB2HeaderSize+48])
	if off%8 > 0 || length%2 > 0 || !fits(uint64(off), uint64(length), uint64(len(cr.data))) {
		return ErrInvalidParameter
	}

	cOff := binary.LittleEndian.Uint32(cr.data[SMB2HeaderSize+48 : SMB2HeaderSize+52])
	cLength := binary.LittleEndian.Uint32(cr.data[SMB2HeaderSize+52 : SMB2HeaderSize+56])
	if !fits(uint64(cOff), uint64(cLength), uint64(len(cr.data))) {
		return ErrInvalidParameter
	}

	// Validate CreditCharge.
	if supportsMultiCredit {
		sps := uint32(len(cr.data) - SMB2HeaderSize - SMB2CreateRequestMinSize)
		var ers uint32
		contexts, err := cr.CreateContexts()
		if err != nil {
			return ErrInvalidParameter
		}
		for ctx := range contexts {
			switch ctx {
			case CREATE_EA_BUFFER, CREATE_SD_BUFFER:
			case CREATE_DURABLE_HANDLE_REQUEST, CREATE_DURABLE_HANDLE_RECONNECT:
				ers += 8
			case SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2:
				ers += 8
			case SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2:
			case CREATE_QUERY_MAXIMAL_ACCESS_REQUEST:
				ers += 8
			case CREATE_ALLOCATION_SIZE, CREATE_TIMEWAROP_TOKEN:
			case CREATE_QUERY_ON_DISK_ID:
				ers += 32
			case CREATE_REQUEST_LEASE:
				ers += 32
			}
		}
		if cr.Header().CreditCharge() == 0 {
			if sps > 65536 || ers > 65536 {
				return ErrInvalidParameter
			}
		} else if cr.Header().CreditCharge() < uint16((max(sps, ers)-1)/65536)+1 {
			return ErrInvalidParameter
		}
	}

	return nil
}

// RequestedOplockLevel returns the RequestedOplockLevel field of the SMB2_CREATE request.
func (cr CreateRequest) RequestedOplockLevel() uint8 {
	return cr.data[SMB2HeaderSize+3]
}

// ImpersonationLevel returns the ImpersonationLevel field of the SMB2_CREATE request.
func (cr CreateRequest) ImpersonationLevel() uint32 {
	return binary.LittleEndian.Uint32(cr.data[SMB2HeaderSize+4 : SMB2HeaderSize+8])
}

// DesiredAccess returns the DesiredAccess field of the SMB2_CREATE request.
func (cr CreateRequest) DesiredAccess() uint32 {
	return binary.LittleEndian.Uint32(cr.data[SMB2HeaderSize+24 : SMB2HeaderSize+28])
}

// SetDesiredAccess sets the DesiredAccess field of the SMB2_CREATE request.
func (cr *CreateRequest) SetDesiredAccess(da uint32) {
	binary.LittleEndian.PutUint32(cr.data[SMB2HeaderSize+24:SMB2HeaderSize+28], da)
}

// FileAttributes returns the FileAttributes field of the SMB2_CREATE request.
func (cr CreateRequest) FileAttributes() uint32 {
	return binary.LittleEndian.Uint32(cr.data[SMB2HeaderSize+28 : SMB2HeaderSize+32])
}

// ShareAccess returns the ShareAccess field of the SMB2_CREATE request.
func (cr CreateRequest) ShareAccess() uint32 {
	return binary.LittleEndian.Uint32(cr.data[SMB2HeaderSize+32 : SMB2HeaderSize+36])
}

// CreateDisposition returns the CreateDisposition field of the SMB2_CREATE request.
func (cr CreateRequest) CreateDisposition() uint32 {
	return binary.LittleEndian.Uint32(cr.data[SMB2HeaderSize+36 : SMB2HeaderSize+40])
}

// CreateOptions returns the CreateOptions field of the SMB2_CREATE request.
func (cr CreateRequest) CreateOptions() uint32 {
	return binary.LittleEndian.Uint32(cr.data[SMB2HeaderSize+40 : SMB2HeaderSize+44])
}

// SetCreateOptions sets the CreateOptions field of the SMB2_CREATE request.
func (cr *CreateRequest) SetCreateOptions(options uint32) {
	binary.LittleEndian.PutUint32(cr.data[SMB2HeaderSize+40:SMB2HeaderSize+44], options)
}

// CreateOptionSelected returns true if the given bit(s) is (are) set in the CreateOptions field.
func (cr CreateRequest) CreateOptionSelected(option uint32) bool {
	return cr.CreateOptions()&option > 0
}

// Filename returns the filename referenced by the Buffer field of the SMB2_CREATE request.
func (cr CreateRequest) Filename() string {
	off := binary.LittleEndian.Uint16(cr.data[SMB2HeaderSize+44 : SMB2HeaderSize+46])
	length := binary.LittleEndian.Uint16(cr.data[SMB2HeaderSize+46 : SMB2HeaderSize+48])
	if !fits(uint64(off), uint64(length), uint64(len(cr.data))) {
		return ""
	}

	return utils.DecodeToString(cr.data[off : uint32(off)+uint32(length)])
}

// CreateContexts returns the create contexts referenced by the Buffer field of the SMB2_CREATE request.
func (cr CreateRequest) CreateContexts() (map[uint32][]byte, error) {
	off := binary.LittleEndian.Uint32(cr.data[SMB2HeaderSize+48 : SMB2HeaderSize+52])
	length := binary.LittleEndian.Uint32(cr.data[SMB2HeaderSize+52 : SMB2HeaderSize+56])
	if length < 4 {
		return nil, nil
	}

	// The chain is walked in a width the offsets on the wire cannot carry past, so that a
	// context which points beyond the message is answered for here rather than reached for, and
	// a step that runs off the end of the count ends the walk instead of coming back round to a
	// place already visited.
	size := uint64(len(cr.data))
	if !fits(uint64(off), uint64(length), size) {
		return nil, ErrInvalidParameter
	}

	contexts := make(map[uint32][]byte)
	for pos := uint64(off); pos < size; {
		// The fixed part of the context has to have arrived before any of it is read.
		if !fits(pos, 16, size) {
			return nil, ErrInvalidParameter
		}

		next := uint64(binary.LittleEndian.Uint32(cr.data[pos : pos+4]))
		nameOff := uint64(binary.LittleEndian.Uint16(cr.data[pos+4 : pos+6]))
		nameLen := binary.LittleEndian.Uint16(cr.data[pos+6 : pos+8])
		if nameLen > 4 {
			if next == 0 {
				break
			}
			pos += next
			continue
		} else if nameLen < 4 {
			return nil, ErrInvalidParameter
		}

		if !fits(pos+nameOff, 4, size) {
			return nil, ErrInvalidParameter
		}
		name := binary.BigEndian.Uint32(cr.data[pos+nameOff : pos+nameOff+4])

		dataOff := uint64(binary.LittleEndian.Uint16(cr.data[pos+10 : pos+12]))
		dataLen := uint64(binary.LittleEndian.Uint32(cr.data[pos+12 : pos+16]))

		// The room set aside for the context is the room the context says it takes, so it is
		// measured against the message before any of it is set aside.
		if !fits(pos+dataOff, dataLen, size) {
			return nil, ErrInvalidParameter
		}

		data := make([]byte, dataLen)
		copy(data, cr.data[pos+dataOff:pos+dataOff+dataLen])
		contexts[name] = data

		if next == 0 {
			break
		}
		pos += next
	}

	return contexts, nil
}

// CreateResponse represents an SMB2_CREATE response.
type CreateResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_CREATE response.
func (cr *CreateResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(cr.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2CreateResponseStructureSize)
}

// SetOplockLevel sets the OplockLevel field of the SMB2_CREATE response.
func (cr *CreateResponse) SetOplockLevel(ol uint8) {
	cr.data[SMB2HeaderSize+2] = ol
}

// SetCreateAction sets the CreateAction field of the SMB2_CREATE response.
func (cr *CreateResponse) SetCreateAction(ca uint32) {
	binary.LittleEndian.PutUint32(cr.data[SMB2HeaderSize+4:SMB2HeaderSize+8], ca)
}

// SetFileTime sets the CreationTime, LastAccessTime, LastWriteTime, and ChangeTime fields of the SMB2_CREATE response.
func (cr *CreateResponse) SetFileTime(creation, lastAccess, lastWrite, change time.Time) {
	binary.LittleEndian.PutUint64(cr.data[SMB2HeaderSize+8:SMB2HeaderSize+16], utils.UnixToFiletime(creation))
	binary.LittleEndian.PutUint64(cr.data[SMB2HeaderSize+16:SMB2HeaderSize+24], utils.UnixToFiletime(lastAccess))
	binary.LittleEndian.PutUint64(cr.data[SMB2HeaderSize+24:SMB2HeaderSize+32], utils.UnixToFiletime(lastWrite))
	binary.LittleEndian.PutUint64(cr.data[SMB2HeaderSize+32:SMB2HeaderSize+40], utils.UnixToFiletime(change))
}

// SetFilesize sets the AllocationSize and EndOfFile fields of the SMB2_CREATE response.
func (cr *CreateResponse) SetFilesize(size, allocated uint64) {
	binary.LittleEndian.PutUint64(cr.data[SMB2HeaderSize+40:SMB2HeaderSize+48], size)
	binary.LittleEndian.PutUint64(cr.data[SMB2HeaderSize+48:SMB2HeaderSize+56], allocated)
}

// SetFileAttributes sets the FileAttributes field of the SMB2_CREATE response.
func (cr *CreateResponse) SetFileAttributes(fa uint32) {
	binary.LittleEndian.PutUint32(cr.data[SMB2HeaderSize+56:SMB2HeaderSize+60], fa)
}

// SetFileID sets the FileID field of the SMB2_CREATE response.
func (cr *CreateResponse) SetFileID(fid []byte) {
	copy(cr.data[SMB2HeaderSize+64:SMB2HeaderSize+80], fid)
}

// SetCreateContexts places the create contexts in the Buffer field of the SMB2_CREATE response.
func (cr *CreateResponse) SetCreateContexts(contexts map[uint32][]byte) {
	length := len(contexts)
	if length == 0 {
		return
	}

	var buf []byte
	var count int
	for id, ctx := range contexts {
		ctxLen := 24 + len(ctx)
		if count < length-1 {
			ctxLen = utils.Roundup(ctxLen, 8)
		}

		context := make([]byte, ctxLen)
		if count < length-1 {
			binary.LittleEndian.PutUint32(context[:4], uint32(ctxLen))
		}

		binary.LittleEndian.PutUint16(context[4:6], 16)
		binary.LittleEndian.PutUint16(context[6:8], 4)
		binary.BigEndian.PutUint32(context[16:20], id)

		binary.LittleEndian.PutUint16(context[10:12], 24)
		binary.LittleEndian.PutUint32(context[12:16], uint32(len(ctx)))
		copy(context[24:24+len(ctx)], ctx)

		buf = append(buf, context...)
		count++
	}

	binary.LittleEndian.PutUint32(cr.data[SMB2HeaderSize+80:SMB2HeaderSize+84], SMB2HeaderSize+88)
	binary.LittleEndian.PutUint32(cr.data[SMB2HeaderSize+84:SMB2HeaderSize+88], uint32(len(buf)))
	cr.data = append(cr.data, buf...)
}

// FromRequest implements GenericResponse interface.
func (cr *CreateResponse) FromRequest(req GenericRequest) {
	cr.Response.FromRequest(req)

	body := make([]byte, SMB2CreateResponseMinSize)
	cr.data = append(cr.data, body...)

	cr.setStructureSize()
	Header(cr.data).SetNextCommand(0)
	Header(cr.data).SetStatus(STATUS_OK)
	if Header(cr.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(cr.data).SetCreditResponse(0)
	}
}

// Generate populates the fields of the SMB2_CREATE response.
func (cr *CreateResponse) Generate(
	oplockLevel uint8,
	createAction uint32,
	size uint64,
	allocated uint64,
	modTime time.Time,
	isDir bool,
	fileID uint64,
	durableFileID uint64,
	createContexts map[uint32][]byte,
) {
	cr.SetOplockLevel(oplockLevel)
	cr.SetCreateAction(createAction)

	var creationTime, lastAccessTime, lastWriteTime, changeTime time.Time
	now := time.Now()
	switch createAction {
	case FILE_SUPERSEDED, FILE_CREATED:
		creationTime = now
		lastAccessTime = now
		lastWriteTime = now
		changeTime = now
	case FILE_OPENED:
		creationTime = modTime
		lastAccessTime = now
		lastWriteTime = modTime
		changeTime = modTime
	case FILE_OVERWRITTEN:
		creationTime = modTime
		lastAccessTime = now
		lastWriteTime = now
		changeTime = now
	}

	cr.SetFileTime(creationTime, lastAccessTime, lastWriteTime, changeTime)

	if isDir {
		cr.SetFileAttributes(FILE_ATTRIBUTE_DIRECTORY)
	} else {
		cr.SetFileAttributes(FILE_ATTRIBUTE_NORMAL)
		cr.SetFilesize(size, allocated)
	}

	fid := make([]byte, 16)
	binary.LittleEndian.PutUint64(fid[:8], fileID)
	binary.LittleEndian.PutUint64(fid[8:], durableFileID)
	cr.SetFileID(fid)

	cr.SetCreateContexts(createContexts)
}

// HandleCreateQueryMaximalAccessRequest generates an SMB2_CREATE_QUERY_MAXIMAL_ACCESS_REQUEST context.
func HandleCreateQueryMaximalAccessRequest(ctx []byte, modTime time.Time, maxAccess uint32) []byte {
	resp := make([]byte, 8)
	if len(ctx) != 8 {
		binary.LittleEndian.PutUint32(resp[:4], STATUS_OK)
		binary.LittleEndian.PutUint32(resp[4:], maxAccess)
	} else {
		timestamp := utils.FiletimeToUnix(binary.LittleEndian.Uint64(ctx[:8]))
		if timestamp == modTime {
			binary.LittleEndian.PutUint32(resp[:4], STATUS_NONE_MAPPED)
		} else {
			binary.LittleEndian.PutUint32(resp[:4], STATUS_OK)
			binary.LittleEndian.PutUint32(resp[4:], maxAccess)
		}
	}
	return resp
}

// HandleCreateQueryOnDiskID generates an SMB2_CREATE_QUERY_ON_DISK_ID context.
func HandleCreateQueryOnDiskID(fid, vid uint64) []byte {
	resp := make([]byte, 32)
	binary.LittleEndian.PutUint64(resp[:8], fid)
	binary.LittleEndian.PutUint64(resp[8:16], vid)
	return resp
}

// HandleCreateDurableHandleRequest generates an SMB2_CREATE_DURABLE_HANDLE_REQUEST context.
func HandleCreateDurableHandleRequest() []byte {
	resp := make([]byte, 8)
	return resp
}
