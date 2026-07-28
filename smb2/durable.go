package smb2

import (
	"encoding/binary"
)

const (
	// Flags of the durable handle create contexts.
	DHANDLE_FLAG_PERSISTENT = 0x00000002
)

const (
	durableHandleRequestV2Size   = 32
	durableHandleReconnectV2Size = 36
)

// DurableHandleRequestV2 is the content of an SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2 create
// context, with which the client asks for a handle that outlives the loss of the connection
// it was created on.
type DurableHandleRequestV2 struct {
	// Timeout is how long the client asks for the handle to be kept, in milliseconds. Zero
	// leaves the choice to the server.
	Timeout    uint32
	Flags      uint32
	CreateGuid [16]byte
}

// ParseDurableHandleRequestV2 decodes the content of an SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2
// create context. It returns false if the context is too short to be one.
func ParseDurableHandleRequestV2(data []byte) (DurableHandleRequestV2, bool) {
	//        SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2
	//   0-4: Timeout
	//   4-8: Flags
	//  8-16: Reserved
	// 16-32: CreateGuid
	if len(data) < durableHandleRequestV2Size {
		return DurableHandleRequestV2{}, false
	}

	req := DurableHandleRequestV2{
		Timeout: binary.LittleEndian.Uint32(data[:4]),
		Flags:   binary.LittleEndian.Uint32(data[4:8]),
	}
	copy(req.CreateGuid[:], data[16:32])

	return req, true
}

// DurableHandleReconnectV2 is the content of an SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2
// create context, with which the client claims a handle it was granted earlier. The file ID
// says which handle is meant and the GUID proves that this is the client that created it.
type DurableHandleReconnectV2 struct {
	FileID     uint64
	DurableID  uint64
	CreateGuid [16]byte
	Flags      uint32
}

// ParseDurableHandleReconnectV2 decodes the content of an
// SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2 create context. It returns false if the context is
// too short to be one.
func ParseDurableHandleReconnectV2(data []byte) (DurableHandleReconnectV2, bool) {
	//        SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2
	//  0-16: FileId
	// 16-32: CreateGuid
	// 32-36: Flags
	if len(data) < durableHandleReconnectV2Size {
		return DurableHandleReconnectV2{}, false
	}

	rec := DurableHandleReconnectV2{
		FileID:    binary.LittleEndian.Uint64(data[:8]),
		DurableID: binary.LittleEndian.Uint64(data[8:16]),
		Flags:     binary.LittleEndian.Uint32(data[32:36]),
	}
	copy(rec.CreateGuid[:], data[16:32])

	return rec, true
}

// HandleCreateDurableHandleRequestV2 generates an SMB2_CREATE_DURABLE_HANDLE_RESPONSE_V2
// context, which tells the client how long the handle it asked for will be kept.
func HandleCreateDurableHandleRequestV2(timeout uint32) []byte {
	//        SMB2_CREATE_DURABLE_HANDLE_RESPONSE_V2
	//   0-4: Timeout
	//   4-8: Flags
	resp := make([]byte, 8)
	binary.LittleEndian.PutUint32(resp[:4], timeout)

	return resp
}
