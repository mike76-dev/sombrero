package smb2

import (
	"encoding/binary"
)

const (
	SMB2OplockBreakRequestMinSize       = 24
	SMB2OplockBreakRequestStructureSize = 24

	SMB2OplockBreakResponseMinSize       = 24
	SMB2OplockBreakResponseStructureSize = 24

	SMB2OplockBreakNotificationSize          = 24
	SMB2OplockBreakNotificationStructureSize = 24
)

// OplockBreakUnsolicitedMessageID is the message ID an oplock break notification carries.
// The notification is sent on the server's own initiative rather than in reply to anything,
// so there is no request whose ID it could take.
const OplockBreakUnsolicitedMessageID = 0xffffffffffffffff

// OplockBreakNotification represents an SMB2_OPLOCK_BREAK notification, with which the server
// tells a client that the oplock it holds is being revoked, and to which level. It is not a
// response to a request and can only be built by NewOplockBreakNotification.
//
// The notification is not signed. It is still encrypted whenever the session encrypts, which
// is the only protection it carries.
type OplockBreakNotification struct {
	Response
}

// NewOplockBreakNotification generates an SMB2_OPLOCK_BREAK notification for the open
// identified by fid, telling the client to drop to the given oplock level. The level is the
// highest the server will accept back in the acknowledgment, and is one of
// OPLOCK_LEVEL_NONE, OPLOCK_LEVEL_II or OPLOCK_LEVEL_EXCLUSIVE: a break never names
// OPLOCK_LEVEL_BATCH, however the oplock being broken was granted.
func NewOplockBreakNotification(oplockLevel uint8, fid []byte, sessionID uint64) *OplockBreakNotification {
	//        SMB2 OPLOCK_BREAK Notification
	//   0-2: StructureSize
	//     2: OplockLevel
	//     3: Reserved
	//   4-8: Reserved2
	//  8-24: FileId
	obn := &OplockBreakNotification{}
	obn.data = make([]byte, SMB2HeaderSize+SMB2OplockBreakNotificationSize)

	h := NewHeader(obn.data)
	h.SetCommand(SMB2_OPLOCK_BREAK)
	h.SetStatus(STATUS_OK)
	h.SetFlags(FLAGS_SERVER_TO_REDIR)
	h.SetMessageID(OplockBreakUnsolicitedMessageID)

	// The notification is not a reply, so it neither answers a credit request nor consumes
	// any of the credits the client was granted.
	h.SetCreditCharge(0)
	h.SetCreditResponse(0)

	// The break names the session that holds the open, but not its tree connect: the file ID
	// alone says which open is meant, and the tree ID is required to be zero. Older Windows
	// servers left the session at zero as well, so a client cannot be relying on either.
	h.SetSessionID(sessionID)
	h.SetTreeID(0)

	binary.LittleEndian.PutUint16(obn.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2OplockBreakNotificationStructureSize)
	obn.data[SMB2HeaderSize+2] = oplockLevel
	copy(obn.data[SMB2HeaderSize+8:SMB2HeaderSize+24], fid)

	return obn
}

// OplockBreakRequest represents an SMB2_OPLOCK_BREAK acknowledgment, with which the client
// confirms that it has dropped the oplock it was told to break.
type OplockBreakRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (obr OplockBreakRequest) Validate(_ bool) error {
	if err := Header(obr.data).Validate(); err != nil {
		return err
	}

	if len(obr.data) < SMB2HeaderSize+SMB2OplockBreakRequestMinSize {
		return ErrWrongLength
	}

	// A lease break acknowledgment arrives under the same command and is told apart from an
	// oplock one by its structure size. The server grants no leases, so there is nothing it
	// could be acknowledging.
	if obr.structureSize() != SMB2OplockBreakRequestStructureSize {
		return ErrWrongFormat
	}

	return nil
}

// OplockLevel returns the OplockLevel field of the SMB2_OPLOCK_BREAK acknowledgment.
func (obr OplockBreakRequest) OplockLevel() uint8 {
	return obr.data[SMB2HeaderSize+2]
}

// FileID returns the FileID field of the SMB2_OPLOCK_BREAK acknowledgment.
func (obr OplockBreakRequest) FileID() []byte {
	fid := make([]byte, 16)
	copy(fid, obr.data[SMB2HeaderSize+8:SMB2HeaderSize+24])
	return fid
}

// OplockBreakResponse represents an SMB2_OPLOCK_BREAK response, with which the server closes
// off the break the client has just acknowledged.
type OplockBreakResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_OPLOCK_BREAK response.
func (obr *OplockBreakResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(obr.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2OplockBreakResponseStructureSize)
}

// SetOplockLevel sets the OplockLevel field of the SMB2_OPLOCK_BREAK response.
func (obr *OplockBreakResponse) SetOplockLevel(ol uint8) {
	obr.data[SMB2HeaderSize+2] = ol
}

// SetFileID sets the FileID field of the SMB2_OPLOCK_BREAK response.
func (obr *OplockBreakResponse) SetFileID(fid []byte) {
	copy(obr.data[SMB2HeaderSize+8:SMB2HeaderSize+24], fid)
}

// FromRequest implements GenericResponse interface.
func (obr *OplockBreakResponse) FromRequest(req GenericRequest) {
	obr.Response.FromRequest(req)

	body := make([]byte, SMB2OplockBreakResponseMinSize)
	obr.data = append(obr.data, body...)

	obr.setStructureSize()
	Header(obr.data).SetNextCommand(0)
	Header(obr.data).SetStatus(STATUS_OK)
	if Header(obr.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(obr.data).SetCreditResponse(0)
	}
}

// Generate populates the fields of the SMB2_OPLOCK_BREAK response. The level and the file are
// echoed back from the acknowledgment, so that the client can tell which break was closed off.
func (obr *OplockBreakResponse) Generate(oplockLevel uint8, fid []byte) {
	obr.SetOplockLevel(oplockLevel)
	obr.SetFileID(fid)
}
