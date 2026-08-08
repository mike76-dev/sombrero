package smb2

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// oplockBreakFileID is a file ID in the order the rest of the server writes one: the volatile
// half first, the durable half second.
var oplockBreakFileID = func() []byte {
	fid := make([]byte, 16)
	binary.LittleEndian.PutUint64(fid[:8], 0x1111222233334444)
	binary.LittleEndian.PutUint64(fid[8:], 0x5555666677778888)
	return fid
}()

func TestNewOplockBreakNotification(t *testing.T) {
	obn := NewOplockBreakNotification(OPLOCK_LEVEL_II, oplockBreakFileID, 0xabcd)

	buf := obn.Encode()
	if len(buf) != SMB2HeaderSize+SMB2OplockBreakNotificationSize {
		t.Fatalf("len = %d, want %d", len(buf), SMB2HeaderSize+SMB2OplockBreakNotificationSize)
	}

	h := obn.Header()
	if err := h.Validate(); err != nil {
		t.Errorf("the header of the notification is malformed: %v", err)
	}
	if h.Command() != SMB2_OPLOCK_BREAK {
		t.Errorf("Command = %#x, want %#x", h.Command(), SMB2_OPLOCK_BREAK)
	}

	// The notification is not a reply, so it carries the reserved message ID rather than one
	// belonging to a request, and grants no credits.
	if h.MessageID() != OplockBreakUnsolicitedMessageID {
		t.Errorf("MessageID = %#x, want %#x", h.MessageID(), uint64(OplockBreakUnsolicitedMessageID))
	}
	if h.CreditCharge() != 0 || h.CreditRequest() != 0 {
		t.Errorf("CreditCharge = %d, CreditResponse = %d, want 0 and 0", h.CreditCharge(), h.CreditRequest())
	}

	if !h.IsFlagSet(FLAGS_SERVER_TO_REDIR) {
		t.Error("the notification is not marked as coming from the server")
	}
	if h.IsFlagSet(FLAGS_ASYNC_COMMAND) || h.IsFlagSet(FLAGS_RELATED_OPERATIONS) {
		t.Error("the notification carries flags of a request it is not answering")
	}

	if h.SessionID() != 0xabcd {
		t.Errorf("SessionID = %#x, want %#x", h.SessionID(), 0xabcd)
	}

	// The tree ID is required to be zero, whichever tree connect the open belongs to.
	if h.TreeID() != 0 {
		t.Errorf("TreeID = %#x, want 0", h.TreeID())
	}

	// The notification is not signed. Encryption, where the session uses it, is applied later
	// to the whole message and is not this constructor's business.
	if h.IsFlagSet(FLAGS_SIGNED) {
		t.Error("the notification is marked as signed")
	}
	if !bytes.Equal(h.Signature(), make([]byte, 16)) {
		t.Error("the notification carries a signature")
	}

	if ss := binary.LittleEndian.Uint16(buf[SMB2HeaderSize : SMB2HeaderSize+2]); ss != SMB2OplockBreakNotificationStructureSize {
		t.Errorf("StructureSize = %d, want %d", ss, SMB2OplockBreakNotificationStructureSize)
	}
	if buf[SMB2HeaderSize+2] != OPLOCK_LEVEL_II {
		t.Errorf("OplockLevel = %#x, want %#x", buf[SMB2HeaderSize+2], OPLOCK_LEVEL_II)
	}
	if !bytes.Equal(buf[SMB2HeaderSize+8:SMB2HeaderSize+24], oplockBreakFileID) {
		t.Errorf("FileId = % x, want % x", buf[SMB2HeaderSize+8:SMB2HeaderSize+24], oplockBreakFileID)
	}
}

// newOplockBreakRequest builds an SMB2_OPLOCK_BREAK acknowledgment with the given structure
// size, so that a lease break acknowledgment can be simulated as well.
func newOplockBreakRequest(structureSize uint16, oplockLevel uint8, fid []byte) OplockBreakRequest {
	data := make([]byte, SMB2HeaderSize+SMB2OplockBreakRequestMinSize)
	h := NewHeader(data)
	h.SetCommand(SMB2_OPLOCK_BREAK)

	binary.LittleEndian.PutUint16(data[SMB2HeaderSize:SMB2HeaderSize+2], structureSize)
	data[SMB2HeaderSize+2] = oplockLevel
	copy(data[SMB2HeaderSize+8:SMB2HeaderSize+24], fid)

	return OplockBreakRequest{Request: Request{data: data}}
}

func TestOplockBreakRequest(t *testing.T) {
	obr := newOplockBreakRequest(SMB2OplockBreakRequestStructureSize, OPLOCK_LEVEL_NONE, oplockBreakFileID)
	if err := obr.Validate(false); err != nil {
		t.Fatalf("a well-formed acknowledgment was rejected: %v", err)
	}
	if obr.OplockLevel() != OPLOCK_LEVEL_NONE {
		t.Errorf("OplockLevel = %#x, want %#x", obr.OplockLevel(), OPLOCK_LEVEL_NONE)
	}
	if !bytes.Equal(obr.FileID(), oplockBreakFileID) {
		t.Errorf("FileID = % x, want % x", obr.FileID(), oplockBreakFileID)
	}

	// FileID hands out a copy: a caller that keeps the ID around must not be able to reach
	// back into the message it came from.
	fid := obr.FileID()
	fid[0] ^= 0xff
	if bytes.Equal(obr.FileID(), fid) {
		t.Error("FileID returned a view into the request rather than a copy")
	}

	// A lease break acknowledgment arrives under the same command with a structure size of
	// its own. Nothing grants leases, so it must not be taken for an oplock acknowledgment.
	lease := newOplockBreakRequest(36, OPLOCK_LEVEL_NONE, oplockBreakFileID)
	if err := lease.Validate(false); err != ErrWrongFormat {
		t.Errorf("a lease break acknowledgment gave %v, want %v", err, ErrWrongFormat)
	}

	truncated := OplockBreakRequest{Request: Request{data: obr.data[:SMB2HeaderSize+SMB2OplockBreakRequestMinSize-1]}}
	if err := truncated.Validate(false); err != ErrWrongLength {
		t.Errorf("a truncated acknowledgment gave %v, want %v", err, ErrWrongLength)
	}
}

func TestOplockBreakResponse(t *testing.T) {
	req := newOplockBreakRequest(SMB2OplockBreakRequestStructureSize, OPLOCK_LEVEL_NONE, oplockBreakFileID)

	resp := &OplockBreakResponse{}
	resp.FromRequest(req)
	resp.Generate(req.OplockLevel(), req.FileID())

	buf := resp.Encode()
	if len(buf) != SMB2HeaderSize+SMB2OplockBreakResponseMinSize {
		t.Fatalf("len = %d, want %d", len(buf), SMB2HeaderSize+SMB2OplockBreakResponseMinSize)
	}

	h := resp.Header()
	if h.Command() != SMB2_OPLOCK_BREAK {
		t.Errorf("Command = %#x, want %#x", h.Command(), SMB2_OPLOCK_BREAK)
	}
	if h.Status() != STATUS_OK {
		t.Errorf("Status = %#x, want %#x", h.Status(), STATUS_OK)
	}
	if !h.IsFlagSet(FLAGS_SERVER_TO_REDIR) {
		t.Error("the response is not marked as coming from the server")
	}

	if ss := binary.LittleEndian.Uint16(buf[SMB2HeaderSize : SMB2HeaderSize+2]); ss != SMB2OplockBreakResponseStructureSize {
		t.Errorf("StructureSize = %d, want %d", ss, SMB2OplockBreakResponseStructureSize)
	}

	// The level and the file are echoed back, so that the client can tell which of the breaks
	// it may have outstanding has just been closed off.
	if buf[SMB2HeaderSize+2] != OPLOCK_LEVEL_NONE {
		t.Errorf("OplockLevel = %#x, want %#x", buf[SMB2HeaderSize+2], OPLOCK_LEVEL_NONE)
	}
	if !bytes.Equal(buf[SMB2HeaderSize+8:SMB2HeaderSize+24], oplockBreakFileID) {
		t.Errorf("FileId = % x, want % x", buf[SMB2HeaderSize+8:SMB2HeaderSize+24], oplockBreakFileID)
	}
}
