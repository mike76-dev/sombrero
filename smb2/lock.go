package smb2

import "encoding/binary"

const (
	SMB2LockRequestMinSize       = 24
	SMB2LockRequestStructureSize = 48

	SMB2LockResponseMinSize       = 4
	SMB2LockResponseStructureSize = 4
)

const (
	// Lock flags.
	LOCKFLAG_SHARED_LOCK      = 0x00000001
	LOCKFLAG_EXCLUSIVE_LOCK   = 0x00000002
	LOCKFLAG_UNLOCK           = 0x00000004
	LOCKFLAG_FAIL_IMMEDIATELY = 0x00000010
)

// LockRequest represents an SMB2_LOCK request.
type LockRequest struct {
	Request
}

// Lock represents an SMB2_LOCK_ELEMENT structure.
type Lock struct {
	Offset uint64
	Length uint64
	Flags  uint32
}

// Validate implements GenericRequest interface.
func (lr LockRequest) Validate(supportsMultiCredit bool) error {
	if err := Header(lr.data).Validate(); err != nil {
		return err
	}

	if len(lr.data) < SMB2HeaderSize+SMB2LockRequestMinSize {
		return ErrWrongLength
	}

	if lr.structureSize() != SMB2LockRequestStructureSize {
		return ErrWrongFormat
	}

	lockCount := binary.LittleEndian.Uint16(lr.data[SMB2HeaderSize+2 : SMB2HeaderSize+4])
	if lockCount == 0 {
		return ErrInvalidParameter
	}

	if len(lr.data) != SMB2HeaderSize+SMB2LockRequestMinSize+24*int(lockCount) {
		return ErrWrongLength
	}

	// Validate CreditCharge.
	if supportsMultiCredit {
		sps := uint32(len(lr.data) - SMB2HeaderSize - SMB2LockRequestMinSize)
		if lr.Header().CreditCharge() == 0 {
			if sps > 65536 {
				return ErrInvalidParameter
			}
		} else if lr.Header().CreditCharge() < uint16((sps-1)/65536)+1 {
			return ErrInvalidParameter
		}
	}

	return nil
}

// FileID returns the FileID field of the SMB2_LOCK request.
func (lr LockRequest) FileID() []byte {
	fid := make([]byte, 16)
	copy(fid, lr.data[SMB2HeaderSize+8:SMB2HeaderSize+24])
	return fid
}

// Locks returns a slice of SMB2_LOCK_ELEMENT structures of the SMB2_LOCK request.
func (lr LockRequest) Locks() []Lock {
	lockCount := binary.LittleEndian.Uint16(lr.data[SMB2HeaderSize+2 : SMB2HeaderSize+4])
	off := SMB2HeaderSize + SMB2LockRequestMinSize
	var locks []Lock
	for i := 0; i < int(lockCount); i++ {
		lock := Lock{
			Offset: binary.LittleEndian.Uint64(lr.data[off : off+8]),
			Length: binary.LittleEndian.Uint64(lr.data[off+8 : off+16]),
			Flags:  binary.LittleEndian.Uint32(lr.data[off+16 : off+20]),
		}

		locks = append(locks, lock)
		off += 24
	}

	return locks
}

// LockResponse represents an SMB2_LOCK response.
type LockResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_LOCK response.
func (lr *LockResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(lr.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2LockResponseStructureSize)
}

// FromRequest implements GenericResponse interface.
func (lr *LockResponse) FromRequest(req GenericRequest) {
	lr.Response.FromRequest(req)

	body := make([]byte, SMB2LockResponseMinSize)
	lr.data = append(lr.data, body...)

	lr.setStructureSize()
	Header(lr.data).SetNextCommand(0)
	Header(lr.data).SetStatus(STATUS_OK)
	if Header(lr.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(lr.data).SetCreditResponse(0)
	}
}
