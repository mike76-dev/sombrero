package smb2

import (
	"encoding/binary"
)

const (
	// Lease states. A lease is a combination of these; the ones the server is willing to grant
	// are decided elsewhere.
	SMB2_LEASE_NONE           = 0x00000000
	SMB2_LEASE_READ_CACHING   = 0x00000001
	SMB2_LEASE_HANDLE_CACHING = 0x00000002
	SMB2_LEASE_WRITE_CACHING  = 0x00000004
)

const (
	// Flags of the SMB2_CREATE_REQUEST_LEASE_V2 create context.
	SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET = 0x00000004

	// Flags of the lease break notification.
	SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED = 0x00000001
)

const (
	leaseRequestV1Size = 32
	leaseRequestV2Size = 52
)

const (
	SMB2LeaseBreakNotificationSize          = 44
	SMB2LeaseBreakNotificationStructureSize = 44

	SMB2LeaseBreakRequestMinSize       = 36
	SMB2LeaseBreakRequestStructureSize = 36

	SMB2LeaseBreakResponseMinSize       = 36
	SMB2LeaseBreakResponseStructureSize = 36
)

// LeaseRequest is the content of an SMB2_CREATE_REQUEST_LEASE create context, with which the
// client asks for a lease on the file it is opening. Unlike an oplock, a lease belongs to the
// client rather than to the open: every open the same client has on the file under the same
// lease key shares it, and none of them breaks the others.
type LeaseRequest struct {
	LeaseKey       [16]byte
	LeaseState     uint32
	Flags          uint32
	ParentLeaseKey [16]byte
	Epoch          uint16

	// Version is 2 for the contexts that carry a parent lease key and an epoch, and 1 for the
	// ones that do not. Both go under the same create context name and are told apart by their
	// length alone.
	Version int
}

// ParseLeaseRequest decodes the content of an SMB2_CREATE_REQUEST_LEASE or
// SMB2_CREATE_REQUEST_LEASE_V2 create context. It returns false if the context is too short to
// be either.
func ParseLeaseRequest(data []byte) (LeaseRequest, bool) {
	//        SMB2_CREATE_REQUEST_LEASE                 _V2 adds
	//  0-16: LeaseKey
	// 16-20: LeaseState
	// 20-24: LeaseFlags                                Flags
	// 24-32: LeaseDuration
	// 32-48:                                           ParentLeaseKey
	// 48-50:                                           Epoch
	// 50-52:                                           Reserved
	if len(data) < leaseRequestV1Size {
		return LeaseRequest{}, false
	}

	req := LeaseRequest{
		LeaseState: binary.LittleEndian.Uint32(data[16:20]),
		Flags:      binary.LittleEndian.Uint32(data[20:24]),
		Version:    1,
	}
	copy(req.LeaseKey[:], data[:16])

	if len(data) >= leaseRequestV2Size {
		req.Version = 2
		copy(req.ParentLeaseKey[:], data[32:48])
		req.Epoch = binary.LittleEndian.Uint16(data[48:50])
	}

	return req, true
}

// HandleCreateRequestLease generates an SMB2_CREATE_RESPONSE_LEASE context, which tells the
// client which lease it has been granted. The answer is given in the version the client asked
// in, and names the same lease key, so that the client can match it to what it asked for.
//
// The flags of the answer stay clear: they say only whether a parent lease key is being
// returned, and the server grants no leases on directories to be a parent of.
func HandleCreateRequestLease(req LeaseRequest, granted uint32, epoch uint16) []byte {
	size := leaseRequestV1Size
	if req.Version == 2 {
		size = leaseRequestV2Size
	}

	resp := make([]byte, size)
	copy(resp[:16], req.LeaseKey[:])
	binary.LittleEndian.PutUint32(resp[16:20], granted)

	if req.Version == 2 {
		binary.LittleEndian.PutUint16(resp[48:50], epoch)
	}

	return resp
}

// LeaseBreakNotification represents a lease break notification, with which the server tells a
// client that the lease it holds is being cut back, and to what. It is not a response to a
// request and can only be built by NewLeaseBreakNotification.
//
// The notification is not signed. It is still encrypted whenever the session encrypts, which
// is the only protection it carries.
type LeaseBreakNotification struct {
	Response
}

// NewLeaseBreakNotification generates a lease break notification for the given lease. If the
// new state still leaves the client something to cache, it has to acknowledge before the state
// takes effect; a break all the way down to nothing needs no answer, and ackRequired says which
// of the two this is.
func NewLeaseBreakNotification(leaseKey [16]byte, current, granted uint32, epoch uint16, ackRequired bool) *LeaseBreakNotification {
	//        Lease Break Notification
	//   0-2: StructureSize
	//   2-4: NewEpoch
	//   4-8: Flags
	//  8-24: LeaseKey
	// 24-28: CurrentLeaseState
	// 28-32: NewLeaseState
	// 32-36: BreakReason
	// 36-40: AccessMaskHint
	// 40-44: ShareMaskHint
	lbn := &LeaseBreakNotification{}
	lbn.data = make([]byte, SMB2HeaderSize+SMB2LeaseBreakNotificationSize)

	h := NewHeader(lbn.data)
	h.SetCommand(SMB2_OPLOCK_BREAK)
	h.SetStatus(STATUS_OK)
	h.SetFlags(FLAGS_SERVER_TO_REDIR)
	h.SetMessageID(OplockBreakUnsolicitedMessageID)
	h.SetCreditCharge(0)
	h.SetCreditResponse(0)

	// A lease belongs to a client rather than to a session, and the notification may travel
	// over any connection that client has. Neither the session nor the tree connect is named;
	// the lease key alone says what is meant.
	h.SetSessionID(0)
	h.SetTreeID(0)

	body := lbn.data[SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], SMB2LeaseBreakNotificationStructureSize)
	binary.LittleEndian.PutUint16(body[2:4], epoch)
	if ackRequired {
		binary.LittleEndian.PutUint32(body[4:8], SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED)
	}
	copy(body[8:24], leaseKey[:])
	binary.LittleEndian.PutUint32(body[24:28], current)
	binary.LittleEndian.PutUint32(body[28:32], granted)

	return lbn
}

// LeaseBreakRequest represents a lease break acknowledgment, with which the client confirms
// that it has cut its lease back to the state it names.
type LeaseBreakRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (lbr LeaseBreakRequest) Validate(_ bool) error {
	if err := Header(lbr.data).Validate(); err != nil {
		return err
	}

	if len(lbr.data) < SMB2HeaderSize+SMB2LeaseBreakRequestMinSize {
		return ErrWrongLength
	}

	// An oplock break acknowledgment arrives under the same command and is told apart from a
	// lease one by its structure size.
	if lbr.structureSize() != SMB2LeaseBreakRequestStructureSize {
		return ErrWrongFormat
	}

	return nil
}

// LeaseKey returns the LeaseKey field of the lease break acknowledgment.
func (lbr LeaseBreakRequest) LeaseKey() [16]byte {
	var key [16]byte
	copy(key[:], lbr.data[SMB2HeaderSize+8:SMB2HeaderSize+24])
	return key
}

// LeaseState returns the LeaseState field of the lease break acknowledgment.
func (lbr LeaseBreakRequest) LeaseState() uint32 {
	return binary.LittleEndian.Uint32(lbr.data[SMB2HeaderSize+24 : SMB2HeaderSize+28])
}

// LeaseBreakResponse represents a lease break response, with which the server closes off the
// break the client has just acknowledged.
type LeaseBreakResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the lease break response.
func (lbr *LeaseBreakResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(lbr.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2LeaseBreakResponseStructureSize)
}

// FromRequest implements GenericResponse interface.
func (lbr *LeaseBreakResponse) FromRequest(req GenericRequest) {
	lbr.Response.FromRequest(req)

	body := make([]byte, SMB2LeaseBreakResponseMinSize)
	lbr.data = append(lbr.data, body...)

	lbr.setStructureSize()
	Header(lbr.data).SetNextCommand(0)
	Header(lbr.data).SetStatus(STATUS_OK)
	if Header(lbr.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(lbr.data).SetCreditResponse(0)
	}
}

// Generate populates the fields of the lease break response with the lease the client is left
// holding.
func (lbr *LeaseBreakResponse) Generate(leaseKey [16]byte, state uint32) {
	//        Lease Break Response
	//   0-2: StructureSize
	//   2-4: Reserved
	//   4-8: Flags
	//  8-24: LeaseKey
	// 24-28: LeaseState
	// 28-36: LeaseDuration
	body := lbr.data[SMB2HeaderSize:]
	copy(body[8:24], leaseKey[:])
	binary.LittleEndian.PutUint32(body[24:28], state)
}
