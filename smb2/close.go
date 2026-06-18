package smb2

import (
	"encoding/binary"
	"time"

	"github.com/mike76-dev/sombrero/utils"
)

const (
	SMB2CloseRequestMinSize       = 24
	SMB2CloseRequestStructureSize = 24

	SMB2CloseResponseMinSize       = 60
	SMB2CloseResponseStructureSize = 60
)

const (
	// SMB2_CLOSE flags.
	CLOSE_FLAG_POSTQUERY_ATTRIB = 0x0001
)

// CloseRequest represents an SMB2_CLOSE request.
type CloseRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (cr CloseRequest) Validate(_ bool) error {
	if err := Header(cr.data).Validate(); err != nil {
		return err
	}

	if len(cr.data) < SMB2HeaderSize+SMB2CloseRequestMinSize {
		return ErrWrongLength
	}

	if cr.structureSize() != SMB2CloseRequestStructureSize {
		return ErrWrongFormat
	}

	return nil
}

// Flags returns the Flags field of the SMB2_CLOSE request.
func (cr CloseRequest) Flags() uint16 {
	return binary.LittleEndian.Uint16(cr.data[SMB2HeaderSize+2 : SMB2HeaderSize+4])
}

// FileID returns the FileID field of the SMB2_CLOSE request.
func (cr CloseRequest) FileID() []byte {
	fid := make([]byte, 16)
	copy(fid, cr.data[SMB2HeaderSize+8:SMB2HeaderSize+24])
	return fid
}

// CloseResponse represents an SMB2_CLOSE response.
type CloseResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_CLOSE response.
func (cr *CloseResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(cr.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2CloseResponseStructureSize)
}

// SetFlags sets the Flags field of the SMB2_CLOSE response.
func (cr *CloseResponse) SetFlags(flags uint16) {
	binary.LittleEndian.PutUint16(cr.data[SMB2HeaderSize+2:SMB2HeaderSize+4], flags)
}

// SetFileTime sets the CreationTime, LastAccessTime, LastWriteTime, and ChangeTime fields of the SMB2_CLOSE response.
func (cr *CloseResponse) SetFileTime(creation, lastAccess, lastWrite, change time.Time) {
	binary.LittleEndian.PutUint64(cr.data[SMB2HeaderSize+8:SMB2HeaderSize+16], utils.UnixToFiletime(creation))
	binary.LittleEndian.PutUint64(cr.data[SMB2HeaderSize+16:SMB2HeaderSize+24], utils.UnixToFiletime(lastAccess))
	binary.LittleEndian.PutUint64(cr.data[SMB2HeaderSize+24:SMB2HeaderSize+32], utils.UnixToFiletime(lastWrite))
	binary.LittleEndian.PutUint64(cr.data[SMB2HeaderSize+32:SMB2HeaderSize+40], utils.UnixToFiletime(change))
}

// SetFilesize sets the AllocationSize and EndOfFile fields of the SMB2_CLOSE response.
func (cr *CloseResponse) SetFilesize(size, allocated uint64) {
	binary.LittleEndian.PutUint64(cr.data[SMB2HeaderSize+40:SMB2HeaderSize+48], size)
	binary.LittleEndian.PutUint64(cr.data[SMB2HeaderSize+48:SMB2HeaderSize+56], allocated)
}

// SetFileAttributes sets the FileAttributes field of the SMB2_CLOSE response.
func (cr *CloseResponse) SetFileAttributes(fa uint32) {
	binary.LittleEndian.PutUint32(cr.data[SMB2HeaderSize+56:SMB2HeaderSize+60], fa)
}

// FromRequest implements GenericResponse interface.
func (cr *CloseResponse) FromRequest(req GenericRequest) {
	cr.Response.FromRequest(req)

	body := make([]byte, SMB2CloseResponseMinSize)
	cr.data = append(cr.data, body...)

	cr.setStructureSize()
	Header(cr.data).SetNextCommand(0)
	Header(cr.data).SetStatus(STATUS_OK)
	if Header(cr.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(cr.data).SetCreditResponse(0)
	}

	if req.(CloseRequest).Flags() == CLOSE_FLAG_POSTQUERY_ATTRIB {
		cr.SetFlags(CLOSE_FLAG_POSTQUERY_ATTRIB)
	}
}

// Generate populates the fields of the SMB2_CLOSE response.
func (cr *CloseResponse) Generate(modTime time.Time, size, allocated uint64, fa uint32) {
	if binary.LittleEndian.Uint16(cr.data[SMB2HeaderSize+2:SMB2HeaderSize+4]) == CLOSE_FLAG_POSTQUERY_ATTRIB {
		cr.SetFileTime(modTime, modTime, modTime, modTime)
		cr.SetFileAttributes(fa)
		if fa&FILE_ATTRIBUTE_DIRECTORY == 0 {
			cr.SetFilesize(size, allocated)
		}
	}
}
