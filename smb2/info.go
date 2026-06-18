package smb2

import (
	"encoding/binary"
	"strings"

	"github.com/mike76-dev/sombrero/utils"
)

const (
	SMB2SetInfoRequestMinSize       = 32
	SMB2SetInfoRequestStructureSize = 33

	SMB2SetInfoResponseMinSize       = 2
	SMB2SetInfoResponseStructureSize = 2
)

// SetInfoRequest represents an SMB2_SET_INFO request.
type SetInfoRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (sir SetInfoRequest) Validate(supportsMultiCredit bool) error {
	if err := Header(sir.data).Validate(); err != nil {
		return err
	}

	if len(sir.data) < SMB2HeaderSize+SMB2SetInfoRequestMinSize {
		return ErrWrongLength
	}

	if sir.structureSize() != SMB2SetInfoRequestStructureSize {
		return ErrWrongFormat
	}

	off := binary.LittleEndian.Uint16(sir.data[SMB2HeaderSize+8 : SMB2HeaderSize+10])
	length := binary.LittleEndian.Uint32(sir.data[SMB2HeaderSize+4 : SMB2HeaderSize+8])
	if uint32(off)+length > uint32(len(sir.data)) {
		return ErrInvalidParameter
	}

	// Validate CreditCharge.
	if supportsMultiCredit {
		sps := uint32(len(sir.data) - SMB2HeaderSize - SMB2SetInfoRequestMinSize)
		if sir.Header().CreditCharge() == 0 {
			if sps > 65536 {
				return ErrInvalidParameter
			}
		} else if sir.Header().CreditCharge() < uint16((sps-1)/65536)+1 {
			return ErrInvalidParameter
		}
	}

	return nil
}

// InfoType returns the InfoType field of the SMB2_SET_INFO request.
func (sir SetInfoRequest) InfoType() uint8 {
	return sir.data[SMB2HeaderSize+2]
}

// FileInfoClass returns the FileInfoClass field of the SMB2_SET_INFO request.
func (sir SetInfoRequest) FileInfoClass() uint8 {
	return sir.data[SMB2HeaderSize+3]
}

// AdditionalInformation returns the AdditionalInformation field of the SMB2_SET_INFO request.
func (sir SetInfoRequest) AdditionalInformation() uint32 {
	return binary.LittleEndian.Uint32(sir.data[SMB2HeaderSize+12 : SMB2HeaderSize+16])
}

// FileID returns the FileID field of the SMB2_SET_INFO request.
func (sir SetInfoRequest) FileID() []byte {
	fid := make([]byte, 16)
	copy(fid, sir.data[SMB2HeaderSize+16:SMB2HeaderSize+32])
	return fid
}

// Buffer returns the Buffer field of the SMB2_SET_INFO request.
func (sir SetInfoRequest) Buffer() []byte {
	off := binary.LittleEndian.Uint16(sir.data[SMB2HeaderSize+8 : SMB2HeaderSize+10])
	length := binary.LittleEndian.Uint32(sir.data[SMB2HeaderSize+4 : SMB2HeaderSize+8])
	return sir.data[off : uint32(off)+length]
}

// SetInfoResponse represents an SMB2_SET_INFO response.
type SetInfoResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_SET_INFO response.
func (sir *SetInfoResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(sir.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2SetInfoResponseStructureSize)
}

// FromRequest implements GenericResponse interface.
func (sir *SetInfoResponse) FromRequest(req GenericRequest) {
	sir.Response.FromRequest(req)

	body := make([]byte, SMB2SetInfoResponseMinSize)
	sir.data = append(sir.data, body...)

	sir.setStructureSize()
	Header(sir.data).SetNextCommand(0)
	Header(sir.data).SetStatus(STATUS_OK)
	if Header(sir.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(sir.data).SetCreditResponse(0)
	} else {
		Header(sir.data).SetCreditResponse(max(req.Header().CreditCharge(), req.Header().CreditRequest()))
	}
}

// FileRenameInfo contains the information about renaming a file, according to FILE_RENAME_INFORMATION_TYPE_2 (MS-FSCC).
type FileRenameInfo struct {
	ReplaceIfExists bool
	RootDirectory   uint64
	FileName        string
}

// Decode unmarshals a byte sequence into a FileRenameInfo structure.
func (fri *FileRenameInfo) Decode(buf []byte) error {
	if len(buf) < 20 {
		return ErrInvalidParameter
	}

	if buf[0] > 0 {
		fri.ReplaceIfExists = true
	}

	fri.RootDirectory = binary.LittleEndian.Uint64(buf[8:16])
	length := binary.LittleEndian.Uint32(buf[16:20])
	if len(buf) < 20+int(length) {
		return ErrInvalidParameter
	}

	name := utils.DecodeToString(buf[20 : 20+length])
	fri.FileName = strings.ReplaceAll(name, "\\", "/")
	return nil
}
