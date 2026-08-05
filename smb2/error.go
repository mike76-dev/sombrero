package smb2

import (
	"encoding/binary"
)

const (
	SMB2ErrorResponseMinSize       = 8
	SMB2ErrorResponseStructureSize = 9
)

const (
	// NT status codes.
	STATUS_OK                                    = 0x00000000
	STATUS_PENDING                               = 0x00000103
	STATUS_NOTIFY_CLEANUP                        = 0x0000010b
	STATUS_NOTIFY_ENUM_DIR                       = 0x0000010c
	STATUS_BUFFER_OVERFLOW                       = 0x80000005
	STATUS_NO_MORE_FILES                         = 0x80000006
	STATUS_UNSUCCESSFUL                          = 0xc0000001
	STATUS_INFO_LENGTH_MISMATCH                  = 0xc0000004
	STATUS_INVALID_HANDLE                        = 0xc0000008
	STATUS_INVALID_PARAMETER                     = 0xc000000d
	STATUS_NO_SUCH_FILE                          = 0xc000000f
	STATUS_INVALID_DEVICE_REQUEST                = 0xC0000010
	STATUS_END_OF_FILE                           = 0xc0000011
	STATUS_MORE_PROCESSING_REQUIRED              = 0xc0000016
	STATUS_ACCESS_DENIED                         = 0xc0000022
	STATUS_BUFFER_TOO_SMALL                      = 0xc0000023
	STATUS_OBJECT_NAME_NOT_FOUND                 = 0xc0000034
	STATUS_OBJECT_NAME_COLLISION                 = 0xc0000035
	STATUS_DATA_ERROR                            = 0xc000003e
	STATUS_EAS_NOT_SUPPORTED                     = 0xc000004f
	STATUS_NO_SUCH_USER                          = 0xc0000064
	STATUS_NONE_MAPPED                           = 0xc0000073
	STATUS_INSUFFICIENT_RESOURCES                = 0xc000009a
	STATUS_IO_TIMEOUT                            = 0xc00000b5
	STATUS_NOT_SUPPORTED                         = 0xc00000bb
	STATUS_UNEXPECTED_NETWORK_ERROR              = 0xc00000c4
	STATUS_NETWORK_NAME_DELETED                  = 0xc00000c9
	STATUS_NETWORK_ACCESS_DENIED                 = 0xc00000ca
	STATUS_BAD_NETWORK_NAME                      = 0xc00000cc
	STATUS_REQUEST_NOT_ACCEPTED                  = 0xc00000d0
	STATUS_INVALID_OPLOCK_PROTOCOL               = 0xc00000e3
	STATUS_DIRECTORY_NOT_EMPTY                   = 0xc0000101
	STATUS_CANCELLED                             = 0xc0000120
	STATUS_FILE_CLOSED                           = 0xc0000128
	STATUS_INVALID_DEVICE_STATE                  = 0xc0000184
	STATUS_USER_SESSION_DELETED                  = 0xc0000203
	STATUS_NOT_FOUND                             = 0xc0000225
	STATUS_DUPLICATE_OBJECTID                    = 0xc000022a
	STATUS_NETWORK_SESSION_EXPIRED               = 0xc000035c
	STATUS_FILE_NOT_AVAILABLE                    = 0xc0000467
	STATUS_SHARE_UNAVAILABLE                     = 0xc0000480
	STATUS_SMB_NO_PREAUTH_INTEGRITY_HASH_OVERLAP = 0xc05d0000
)

// ErrorResponse represents an SMB2_ERROR response.
type ErrorResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_ERROR response.
func (er *ErrorResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(er.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2ErrorResponseStructureSize)
}

// SetErrorData sets the ErrorData field of the SMB2_ERROR response.
func (er *ErrorResponse) SetErrorData(data []byte) {
	binary.LittleEndian.PutUint32(er.data[SMB2HeaderSize+4:SMB2HeaderSize+8], uint32(len(data)))
	er.data = er.data[:SMB2HeaderSize+SMB2ErrorResponseMinSize]
	if data == nil {
		er.data = append(er.data, byte(0))
	} else {
		er.data = append(er.data, data...)
	}
}

// SetErrorContextCount sets the ErrorContextCount of the SMB2_ERROR response.
func (er *ErrorResponse) SetErrorContextCount(count uint8) {
	er.data[SMB2HeaderSize+2] = count
}

// NegotiateErrorResponse generates an SMB2_ERROR response to an SMB_COM_NEGOTIATE request.
func NegotiateErrorResponse(status uint32) *ErrorResponse {
	er := &ErrorResponse{}
	er.data = make([]byte, SMB2HeaderSize+SMB2ErrorResponseMinSize)
	Header(er.data).SetStatus(status)
	Header(er.data).SetFlags(FLAGS_SERVER_TO_REDIR)
	return er
}

// FromRequest implements GenericResponse interface.
func (er *ErrorResponse) FromRequest(req GenericRequest) {
	er.Response.FromRequest(req)

	body := make([]byte, SMB2ErrorResponseMinSize)
	er.data = append(er.data, body...)
	Header(er.data).SetNextCommand(0)

	if Header(er.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(er.data).SetCreditResponse(0)
	}

	er.setStructureSize()
}

// NewErrorResponse generates an SMB2_ERROR response with the specified parameters.
func NewErrorResponse(req GenericRequest, status uint32, count uint8, data []byte) *ErrorResponse {
	er := &ErrorResponse{}
	er.FromRequest(req)
	Header(er.data).SetStatus(status)
	if status == STATUS_PENDING {
		Header(er.data).SetCreditResponse(0)
	}
	er.SetErrorContextCount(count)
	er.SetErrorData(data)
	return er
}

// ErrorContextData generates an ErrorContextData blob.
func ErrorContextData(eid uint32, ecd []byte) []byte {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint32(data[:4], uint32(len(ecd)))
	binary.LittleEndian.PutUint32(data[4:], eid)
	return append(data, ecd...)
}
