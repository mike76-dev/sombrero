package smb2

import "encoding/binary"

const (
	SMB2WriteRequestMinSize       = 48
	SMB2WriteRequestStructureSize = 49

	SMB2WriteResponseMinSize       = 16
	SMB2WriteResponseStructureSize = 17
)

const (
	// SMB2_WRITE flags.
	WRITEFLAG_WRITE_THROUGH    = 0x00000001
	WRITEFLAG_WRITE_UNBUFFERED = 0x00000002
)

// MaxWriteDataOffset is how far into an SMB2_WRITE request the data may begin. The fixed part of
// the request ends well before it, so the rest is padding the client is free to insert; past
// this the request is malformed rather than padded.
const MaxWriteDataOffset = 0x100

// WriteRequest represents an SMB2_WRITE request.
type WriteRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (wr WriteRequest) Validate(supportsMultiCredit bool) error {
	if err := Header(wr.data).Validate(); err != nil {
		return err
	}

	if len(wr.data) < SMB2HeaderSize+SMB2WriteRequestMinSize {
		return ErrWrongLength
	}

	if wr.structureSize() != SMB2WriteRequestStructureSize {
		return ErrWrongFormat
	}

	length := binary.LittleEndian.Uint32(wr.data[SMB2HeaderSize+4 : SMB2HeaderSize+8])
	if uint32(wr.DataOffset())+length > uint32(len(wr.data)) {
		return ErrInvalidParameter
	}

	// Validate CreditCharge.
	if supportsMultiCredit {
		sps := uint32(len(wr.data) - SMB2HeaderSize - SMB2WriteRequestMinSize)
		if wr.Header().CreditCharge() == 0 {
			if sps > 65536 {
				return ErrInvalidParameter
			}
		} else if wr.Header().CreditCharge() < uint16((sps-1)/65536)+1 {
			return ErrInvalidParameter
		}
	}

	return nil
}

// DataOffset returns the DataOffset field of the SMB2_WRITE request: where the data begins,
// counted from the start of the SMB2 header.
func (wr WriteRequest) DataOffset() uint16 {
	return binary.LittleEndian.Uint16(wr.data[SMB2HeaderSize+2 : SMB2HeaderSize+4])
}

// Offset returns the Offset field of the SMB2_WRITE request.
func (wr WriteRequest) Offset() uint64 {
	return binary.LittleEndian.Uint64(wr.data[SMB2HeaderSize+8 : SMB2HeaderSize+16])
}

// FileID returns the FileID field of the SMB2_WRITE request.
func (wr WriteRequest) FileID() []byte {
	fid := make([]byte, 16)
	copy(fid, wr.data[SMB2HeaderSize+16:SMB2HeaderSize+32])
	return fid
}

// Channel returns the Channel field of the SMB2_WRITE request. The values it may carry are the
// SMB2_CHANNEL_* constants, which SMB2_READ shares.
func (wr WriteRequest) Channel() uint32 {
	return binary.LittleEndian.Uint32(wr.data[SMB2HeaderSize+32 : SMB2HeaderSize+36])
}

// Flags returns the Flags field of the SMB2_WRITE request.
func (wr WriteRequest) Flags() uint32 {
	return binary.LittleEndian.Uint32(wr.data[SMB2HeaderSize+44 : SMB2HeaderSize+48])
}

// Buffer returns the Buffer field of the SMB2_WRITE request.
func (wr WriteRequest) Buffer() []byte {
	off := uint32(wr.DataOffset())
	length := binary.LittleEndian.Uint32(wr.data[SMB2HeaderSize+4 : SMB2HeaderSize+8])
	return wr.data[off : off+length]
}

// WriteResponse represents an SMB2_WRITE response.
type WriteResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_WRITE response.
func (wr *WriteResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(wr.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2WriteResponseStructureSize)
}

// SetCount sets the Count field of the SMB2_WRITE response.
func (wr *WriteResponse) SetCount(count uint32) {
	binary.LittleEndian.PutUint32(wr.data[SMB2HeaderSize+4:SMB2HeaderSize+8], count)
}

// FromRequest implements GenericResponse interface.
func (wr *WriteResponse) FromRequest(req GenericRequest) {
	wr.Response.FromRequest(req)

	body := make([]byte, SMB2WriteResponseMinSize)
	wr.data = append(wr.data, body...)

	wr.setStructureSize()
	Header(wr.data).SetNextCommand(0)
	Header(wr.data).SetStatus(STATUS_OK)
	if Header(wr.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(wr.data).SetCreditResponse(0)
	} else {
		Header(wr.data).SetCreditResponse(max(req.Header().CreditCharge(), req.Header().CreditRequest()))
	}
}

// Generate populates the fields of the SMB2_WRITE response.
func (wr *WriteResponse) Generate(count uint32) {
	wr.SetCount(count)
}
