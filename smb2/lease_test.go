package smb2

import (
	"bytes"
	"encoding/binary"
	"testing"
)

var (
	testLeaseKey   = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	testParentKey  = [16]byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	testLeaseState = uint32(SMB2_LEASE_READ_CACHING | SMB2_LEASE_HANDLE_CACHING | SMB2_LEASE_WRITE_CACHING)
)

// leaseRequestContext builds the content of a lease create context of the given size, so that
// both versions can be produced from one place.
func leaseRequestContext(size int) []byte {
	data := make([]byte, size)
	copy(data[:16], testLeaseKey[:])
	binary.LittleEndian.PutUint32(data[16:20], testLeaseState)
	binary.LittleEndian.PutUint32(data[20:24], SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET)
	if size >= leaseRequestV2Size {
		copy(data[32:48], testParentKey[:])
		binary.LittleEndian.PutUint16(data[48:50], 7)
	}
	return data
}

func TestParseLeaseRequest(t *testing.T) {
	t.Run("version 1", func(t *testing.T) {
		req, ok := ParseLeaseRequest(leaseRequestContext(leaseRequestV1Size))
		if !ok {
			t.Fatal("a context of the full size was rejected")
		}
		if req.Version != 1 {
			t.Errorf("Version = %d, want 1", req.Version)
		}
		if req.LeaseKey != testLeaseKey {
			t.Errorf("LeaseKey = % x, want % x", req.LeaseKey, testLeaseKey)
		}
		if req.LeaseState != testLeaseState {
			t.Errorf("LeaseState = %#x, want %#x", req.LeaseState, testLeaseState)
		}

		// The fields the shorter context does not carry must not be invented.
		if req.ParentLeaseKey != ([16]byte{}) || req.Epoch != 0 {
			t.Error("a version 1 context yielded a parent lease key or an epoch")
		}
	})

	t.Run("version 2", func(t *testing.T) {
		req, ok := ParseLeaseRequest(leaseRequestContext(leaseRequestV2Size))
		if !ok {
			t.Fatal("a context of the full size was rejected")
		}
		if req.Version != 2 {
			t.Errorf("Version = %d, want 2", req.Version)
		}
		if req.LeaseKey != testLeaseKey {
			t.Errorf("LeaseKey = % x, want % x", req.LeaseKey, testLeaseKey)
		}
		if req.ParentLeaseKey != testParentKey {
			t.Errorf("ParentLeaseKey = % x, want % x", req.ParentLeaseKey, testParentKey)
		}
		if req.Epoch != 7 {
			t.Errorf("Epoch = %d, want 7", req.Epoch)
		}
	})

	// A context between the two sizes carries no parent lease key, so it is read as the
	// shorter one rather than off the end of itself.
	t.Run("between the two versions", func(t *testing.T) {
		req, ok := ParseLeaseRequest(leaseRequestContext(leaseRequestV2Size - 1))
		if !ok {
			t.Fatal("a context longer than version 1 was rejected")
		}
		if req.Version != 1 {
			t.Errorf("Version = %d, want 1", req.Version)
		}
	})

	if _, ok := ParseLeaseRequest(leaseRequestContext(leaseRequestV1Size - 1)); ok {
		t.Error("a truncated context was accepted")
	}
	if _, ok := ParseLeaseRequest(nil); ok {
		t.Error("an empty context was accepted")
	}
}

func TestHandleCreateRequestLease(t *testing.T) {
	granted := uint32(SMB2_LEASE_READ_CACHING | SMB2_LEASE_WRITE_CACHING)

	t.Run("answered in the version it was asked in", func(t *testing.T) {
		for _, size := range []int{leaseRequestV1Size, leaseRequestV2Size} {
			req, _ := ParseLeaseRequest(leaseRequestContext(size))
			resp := HandleCreateRequestLease(req, granted, 3)

			if len(resp) != size {
				t.Errorf("a version %d request was answered with %d bytes, want %d", req.Version, len(resp), size)
			}
			if !bytes.Equal(resp[:16], testLeaseKey[:]) {
				t.Errorf("LeaseKey = % x, want % x", resp[:16], testLeaseKey)
			}
			if state := binary.LittleEndian.Uint32(resp[16:20]); state != granted {
				t.Errorf("LeaseState = %#x, want %#x", state, granted)
			}
		}
	})

	t.Run("the epoch is returned only in version 2", func(t *testing.T) {
		req, _ := ParseLeaseRequest(leaseRequestContext(leaseRequestV2Size))
		resp := HandleCreateRequestLease(req, granted, 3)
		if epoch := binary.LittleEndian.Uint16(resp[48:50]); epoch != 3 {
			t.Errorf("Epoch = %d, want 3", epoch)
		}

		// The flags say only whether a parent lease key is being returned, and none is: a
		// client told otherwise would read one out of sixteen zero bytes.
		if flags := binary.LittleEndian.Uint32(resp[20:24]); flags != 0 {
			t.Errorf("Flags = %#x, want 0", flags)
		}
		if !bytes.Equal(resp[32:48], make([]byte, 16)) {
			t.Error("a parent lease key was returned although none is held")
		}
	})
}

func TestNewLeaseBreakNotification(t *testing.T) {
	current := uint32(SMB2_LEASE_READ_CACHING | SMB2_LEASE_HANDLE_CACHING | SMB2_LEASE_WRITE_CACHING)

	lbn := NewLeaseBreakNotification(testLeaseKey, current, SMB2_LEASE_NONE, 4, true)
	buf := lbn.Encode()

	if len(buf) != SMB2HeaderSize+SMB2LeaseBreakNotificationSize {
		t.Fatalf("len = %d, want %d", len(buf), SMB2HeaderSize+SMB2LeaseBreakNotificationSize)
	}

	h := lbn.Header()
	if err := h.Validate(); err != nil {
		t.Errorf("the header of the notification is malformed: %v", err)
	}
	if h.Command() != SMB2_OPLOCK_BREAK {
		t.Errorf("Command = %#x, want %#x", h.Command(), SMB2_OPLOCK_BREAK)
	}
	if h.MessageID() != OplockBreakUnsolicitedMessageID {
		t.Errorf("MessageID = %#x, want %#x", h.MessageID(), uint64(OplockBreakUnsolicitedMessageID))
	}

	// A lease belongs to a client rather than to a session, so neither is named. The lease key
	// is what says which lease is meant.
	if h.SessionID() != 0 || h.TreeID() != 0 {
		t.Errorf("SessionID = %#x, TreeID = %#x, want 0 and 0", h.SessionID(), h.TreeID())
	}
	if h.IsFlagSet(FLAGS_SIGNED) {
		t.Error("the notification is marked as signed")
	}

	body := buf[SMB2HeaderSize:]
	if ss := binary.LittleEndian.Uint16(body[0:2]); ss != SMB2LeaseBreakNotificationStructureSize {
		t.Errorf("StructureSize = %d, want %d", ss, SMB2LeaseBreakNotificationStructureSize)
	}
	if epoch := binary.LittleEndian.Uint16(body[2:4]); epoch != 4 {
		t.Errorf("NewEpoch = %d, want 4", epoch)
	}
	if flags := binary.LittleEndian.Uint32(body[4:8]); flags != SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED {
		t.Errorf("Flags = %#x, want the acknowledgment-required bit", flags)
	}
	if !bytes.Equal(body[8:24], testLeaseKey[:]) {
		t.Errorf("LeaseKey = % x, want % x", body[8:24], testLeaseKey)
	}
	if state := binary.LittleEndian.Uint32(body[24:28]); state != current {
		t.Errorf("CurrentLeaseState = %#x, want %#x", state, current)
	}
	if state := binary.LittleEndian.Uint32(body[28:32]); state != SMB2_LEASE_NONE {
		t.Errorf("NewLeaseState = %#x, want none", state)
	}

	// A break that leaves the client nothing to cache needs no answer, and saying otherwise
	// would have the client reply to a lease that is already gone.
	quiet := NewLeaseBreakNotification(testLeaseKey, current, SMB2_LEASE_NONE, 4, false)
	if flags := binary.LittleEndian.Uint32(quiet.Encode()[SMB2HeaderSize+4 : SMB2HeaderSize+8]); flags != 0 {
		t.Errorf("Flags = %#x, want no acknowledgment required", flags)
	}
}

// newLeaseBreakRequest builds a lease break acknowledgment with the given structure size, so
// that an oplock break acknowledgment can be simulated as well.
func newLeaseBreakRequest(structureSize uint16, key [16]byte, state uint32) LeaseBreakRequest {
	data := make([]byte, SMB2HeaderSize+SMB2LeaseBreakRequestMinSize)
	h := NewHeader(data)
	h.SetCommand(SMB2_OPLOCK_BREAK)

	body := data[SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], structureSize)
	copy(body[8:24], key[:])
	binary.LittleEndian.PutUint32(body[24:28], state)

	return LeaseBreakRequest{Request: Request{data: data}}
}

func TestLeaseBreakRequest(t *testing.T) {
	lbr := newLeaseBreakRequest(SMB2LeaseBreakRequestStructureSize, testLeaseKey, SMB2_LEASE_READ_CACHING)
	if err := lbr.Validate(false); err != nil {
		t.Fatalf("a well-formed acknowledgment was rejected: %v", err)
	}
	if lbr.LeaseKey() != testLeaseKey {
		t.Errorf("LeaseKey = % x, want % x", lbr.LeaseKey(), testLeaseKey)
	}
	if lbr.LeaseState() != SMB2_LEASE_READ_CACHING {
		t.Errorf("LeaseState = %#x, want %#x", lbr.LeaseState(), SMB2_LEASE_READ_CACHING)
	}

	// The two acknowledgments share a command and are told apart by their structure size, so
	// each parser has to turn the other one away.
	oplock := newLeaseBreakRequest(SMB2OplockBreakRequestStructureSize, testLeaseKey, 0)
	if err := oplock.Validate(false); err != ErrWrongFormat {
		t.Errorf("an oplock break acknowledgment gave %v, want %v", err, ErrWrongFormat)
	}

	truncated := LeaseBreakRequest{Request: Request{data: lbr.data[:SMB2HeaderSize+SMB2LeaseBreakRequestMinSize-1]}}
	if err := truncated.Validate(false); err != ErrWrongLength {
		t.Errorf("a truncated acknowledgment gave %v, want %v", err, ErrWrongLength)
	}
}

func TestLeaseBreakResponse(t *testing.T) {
	req := newLeaseBreakRequest(SMB2LeaseBreakRequestStructureSize, testLeaseKey, SMB2_LEASE_NONE)

	resp := &LeaseBreakResponse{}
	resp.FromRequest(req)
	resp.Generate(testLeaseKey, SMB2_LEASE_NONE)

	buf := resp.Encode()
	if len(buf) != SMB2HeaderSize+SMB2LeaseBreakResponseMinSize {
		t.Fatalf("len = %d, want %d", len(buf), SMB2HeaderSize+SMB2LeaseBreakResponseMinSize)
	}

	h := resp.Header()
	if h.Command() != SMB2_OPLOCK_BREAK {
		t.Errorf("Command = %#x, want %#x", h.Command(), SMB2_OPLOCK_BREAK)
	}
	if h.Status() != STATUS_OK {
		t.Errorf("Status = %#x, want %#x", h.Status(), STATUS_OK)
	}

	body := buf[SMB2HeaderSize:]
	if ss := binary.LittleEndian.Uint16(body[0:2]); ss != SMB2LeaseBreakResponseStructureSize {
		t.Errorf("StructureSize = %d, want %d", ss, SMB2LeaseBreakResponseStructureSize)
	}
	if !bytes.Equal(body[8:24], testLeaseKey[:]) {
		t.Errorf("LeaseKey = % x, want % x", body[8:24], testLeaseKey)
	}
	if state := binary.LittleEndian.Uint32(body[24:28]); state != SMB2_LEASE_NONE {
		t.Errorf("LeaseState = %#x, want none", state)
	}
}
