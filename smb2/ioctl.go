package smb2

import "encoding/binary"

const (
	SMB2IoctlRequestMinSize       = 56
	SMB2IoctlRequestStructureSize = 57

	SMB2IoctlResponseMinSize       = 48
	SMB2IoctlResponseStructureSize = 49
)

const (
	// FSCTL control codes.
	FSCTL_DFS_GET_REFERRALS                 = 0x00060194
	FSCTL_PIPE_PEEK                         = 0x0011400c
	FSCTL_PIPE_WAIT                         = 0x00110018
	FSCTL_PIPE_TRANSCEIVE                   = 0x0011c017
	FSCTL_SRV_COPYCHUNK                     = 0x001440f2
	FSCTL_SRV_ENUMERATE_SNAPSHOTS           = 0x00144064
	FSCTL_SRV_REQUEST_RESUME_KEY            = 0x00140078
	FSCTL_SRV_READ_HASH                     = 0x001441bb
	FSCTL_SRV_COPYCHUNK_WRITE               = 0x001480f2
	FSCTL_LMR_REQUEST_RESILIENCY            = 0x001401d4
	FSCTL_QUERY_NETWORK_INTERFACE_INFO      = 0x001401fc
	FSCTL_SET_REPARSE_POINT                 = 0x000900a4
	FSCTL_DFS_GET_REFERRALS_EX              = 0x000601b0
	FSCTL_FILE_LEVEL_TRIM                   = 0x00098208
	FSCTL_VALIDATE_NEGOTIATE_INFO           = 0x00140204
	FSCTL_CREATE_OR_GET_OBJECT_ID           = 0x000900c0
	FSCTL_SVHDX_SYNC_TUNNEL_REQUEST         = 0x00090304
	FSCTL_QUERY_SHARED_VIRTUAL_DISK_SUPPORT = 0x00090300
)

const (
	// IOCTL flags.
	IOCTL_IS_FSCTL = 0x00000001
)

var (
	DummyFileID = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
)

// IoctlRequest represents an SMB2_IOCTL request.
type IoctlRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (ir IoctlRequest) Validate(supportsMultiCredit bool) error {
	if err := Header(ir.data).Validate(); err != nil {
		return err
	}

	if len(ir.data) < SMB2HeaderSize+SMB2IoctlRequestMinSize {
		return ErrWrongLength
	}

	if ir.structureSize() != SMB2IoctlRequestStructureSize {
		return ErrWrongFormat
	}

	off := binary.LittleEndian.Uint32(ir.data[SMB2HeaderSize+24 : SMB2HeaderSize+28])
	length := binary.LittleEndian.Uint32(ir.data[SMB2HeaderSize+28 : SMB2HeaderSize+32])
	if length > 0 && ((off > 0 && off < SMB2HeaderSize+SMB2IoctlRequestMinSize) || off%8 > 0 || !fits(uint64(off), uint64(length), uint64(len(ir.data)))) {
		return ErrInvalidParameter
	}

	// Validate CreditCharge.
	if supportsMultiCredit {
		sps := uint32(len(ir.data) - SMB2HeaderSize - SMB2IoctlRequestMinSize)
		ers := ir.MaxInputResponse() + ir.MaxOutputResponse()
		if ir.Header().CreditCharge() == 0 {
			if sps > 65536 || ers > 65536 {
				return ErrInvalidParameter
			}
		} else if ir.Header().CreditCharge() < uint16((max(sps, ers)-1)/65536)+1 {
			return ErrInvalidParameter
		}
	}

	return nil
}

// CtlCode returns the CtlCode field of the SMB2_IOCTL request.
func (ir IoctlRequest) CtlCode() uint32 {
	return binary.LittleEndian.Uint32(ir.data[SMB2HeaderSize+4 : SMB2HeaderSize+8])
}

// FileID returns the FileID field of the SMB2_IOCTL request.
func (ir IoctlRequest) FileID() []byte {
	fid := make([]byte, 16)
	copy(fid, ir.data[SMB2HeaderSize+8:SMB2HeaderSize+24])
	return fid
}

// InputBuffer returns the Buffer field of the SMB2_IOCTL request.
func (ir IoctlRequest) InputBuffer() []byte {
	off := binary.LittleEndian.Uint32(ir.data[SMB2HeaderSize+24 : SMB2HeaderSize+28])
	length := binary.LittleEndian.Uint32(ir.data[SMB2HeaderSize+28 : SMB2HeaderSize+32])
	if !fits(uint64(off), uint64(length), uint64(len(ir.data))) {
		return nil
	}

	return ir.data[off : off+length]
}

// MaxInputResponse returns the MaxInputResponse field of the SMB2_IOCTL request.
func (ir IoctlRequest) MaxInputResponse() uint32 {
	return binary.LittleEndian.Uint32(ir.data[SMB2HeaderSize+32 : SMB2HeaderSize+36])
}

// MaxOutputResponse returns the MaxOutputResponse field of the SMB2_IOCTL request.
func (ir IoctlRequest) MaxOutputResponse() uint32 {
	return binary.LittleEndian.Uint32(ir.data[SMB2HeaderSize+44 : SMB2HeaderSize+48])
}

// Flags returns the Flags field of the SMB2_IOCTL request.
func (ir IoctlRequest) Flags() uint32 {
	return binary.LittleEndian.Uint32(ir.data[SMB2HeaderSize+48 : SMB2HeaderSize+52])
}

func (ir IoctlRequest) ValidateNegotiateInfo() (caps uint32, guid []byte, sm uint16, dialects []uint16, err error) {
	buf := ir.InputBuffer()
	if len(buf) < 24 {
		err = ErrWrongLength
		return
	}
	caps = binary.LittleEndian.Uint32(buf[:4])
	guid = make([]byte, 16)
	copy(guid, buf[4:20])
	sm = binary.LittleEndian.Uint16(buf[20:22])
	count := int(binary.LittleEndian.Uint16(buf[22:24]))
	if len(buf) < 24+count*2 {
		err = ErrInvalidParameter
		return
	}
	dialects = make([]uint16, count)
	for i := 0; i < count; i++ {
		dialects[i] = binary.LittleEndian.Uint16(buf[24+i*2 : 26+i*2])
	}
	return
}

// IoctlResponse represents an SMB2_IOCTL response.
type IoctlResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_IOCTL response.
func (ir *IoctlResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(ir.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2IoctlResponseStructureSize)
}

// SetCtlCode sets the CtlCode field of the SMB2_IOCTL response.
func (ir *IoctlResponse) SetCtlCode(code uint32) {
	binary.LittleEndian.PutUint32(ir.data[SMB2HeaderSize+4:SMB2HeaderSize+8], code)
}

// SetFileID sets the FileID field of the SMB2_IOCTL response.
func (ir *IoctlResponse) SetFileID(fid []byte) {
	copy(ir.data[SMB2HeaderSize+8:SMB2HeaderSize+24], fid)
}

// SetFlags sets the Flags field of the SMB2_IOCTL response.
func (ir *IoctlResponse) SetFlags(flags uint32) {
	binary.LittleEndian.PutUint32(ir.data[SMB2HeaderSize+40:SMB2HeaderSize+44], flags)
}

// SetOutputBuffer sets the Buffer field of the SMB2_IOCTL response.
func (ir *IoctlResponse) SetOutputBuffer(buf []byte) {
	binary.LittleEndian.PutUint32(ir.data[SMB2HeaderSize+24:SMB2HeaderSize+28], SMB2HeaderSize+SMB2IoctlResponseMinSize)
	binary.LittleEndian.PutUint32(ir.data[SMB2HeaderSize+28:SMB2HeaderSize+32], 0)
	var off uint32
	if len(buf) > 0 {
		off = uint32(SMB2HeaderSize + SMB2IoctlResponseMinSize)
	}
	binary.LittleEndian.PutUint32(ir.data[SMB2HeaderSize+32:SMB2HeaderSize+36], off)
	binary.LittleEndian.PutUint32(ir.data[SMB2HeaderSize+36:SMB2HeaderSize+40], uint32(len(buf)))
	ir.data = ir.data[:SMB2HeaderSize+SMB2IoctlResponseMinSize]
	ir.data = append(ir.data, buf...)
}

// FromRequest implements GenericResponse interface.
func (ir *IoctlResponse) FromRequest(req GenericRequest) {
	ir.Response.FromRequest(req)

	body := make([]byte, SMB2IoctlResponseMinSize)
	ir.data = append(ir.data, body...)

	ir.setStructureSize()
	Header(ir.data).SetNextCommand(0)
	Header(ir.data).SetStatus(STATUS_OK)
	if Header(ir.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(ir.data).SetCreditResponse(0)
	} else {
		Header(ir.data).SetCreditResponse(max(req.Header().CreditCharge(), req.Header().CreditRequest()))
	}
}

// Generate populates the fields of the SMB2_IOCTL response.
func (ir *IoctlResponse) Generate(code uint32, fid []byte, flags uint32, output []byte) {
	ir.SetCtlCode(code)
	ir.SetFileID(fid)
	ir.SetFlags(flags)
	ir.SetOutputBuffer(output)
}

func ValidateNegotiateInfo(caps uint32, guid []byte, sm uint16, dialect uint16) []byte {
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint32(buf[:4], caps)
	copy(buf[4:20], guid)
	binary.LittleEndian.PutUint16(buf[20:22], sm)
	binary.LittleEndian.PutUint16(buf[22:24], dialect)
	return buf
}
