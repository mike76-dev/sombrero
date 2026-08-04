package main

import (
	"encoding/binary"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// newTestRequest builds a bare SMB2 request with the given command, channel sequence, and
// replay flag, which is all that the verification of the channel sequence number looks at.
func newTestRequest(t *testing.T, command uint16, cs uint16, replay bool) *smb2.Request {
	t.Helper()

	buf := make([]byte, smb2.SMB2HeaderSize)
	h := smb2.Header(buf)
	h.SetProtocolID(smb2.PROTOCOL_SMB2)
	binary.LittleEndian.PutUint16(buf[4:6], smb2.SMB2HeaderStructureSize)
	h.SetCommand(command)
	h.SetMessageID(1)
	binary.LittleEndian.PutUint16(buf[8:10], cs)
	if replay {
		h.SetFlag(smb2.FLAGS_REPLAY_OPERATION)
	}

	reqs, err := smb2.GetRequests(buf, 0, 0, false)
	if err != nil {
		t.Fatalf("couldn't build request: %v", err)
	}

	return reqs[0]
}

func TestVerifyChannelSequence(t *testing.T) {
	tests := []struct {
		name string

		// The state of the Open before the request.
		channelSequence uint16
		outstanding     uint32
		previous        uint32

		// The request.
		command uint16
		cs      uint16
		replay  bool

		// The expected outcome.
		status          uint32
		wantSequence    uint16
		wantOutstanding uint32
		wantPrevious    uint32
		wantCounted     bool
	}{
		{
			name:            "same sequence",
			channelSequence: 7, outstanding: 2, previous: 1,
			command: smb2.SMB2_WRITE, cs: 7,
			status: smb2.STATUS_OK, wantSequence: 7, wantOutstanding: 3, wantPrevious: 1, wantCounted: true,
		},
		{
			name:            "newer sequence carries the outstanding requests over",
			channelSequence: 7, outstanding: 2, previous: 1,
			command: smb2.SMB2_WRITE, cs: 9,
			status: smb2.STATUS_OK, wantSequence: 9, wantOutstanding: 1, wantPrevious: 3, wantCounted: true,
		},
		{
			name:            "newer sequence wrapping around",
			channelSequence: 0xffff, outstanding: 2, previous: 0,
			command: smb2.SMB2_WRITE, cs: 1,
			status: smb2.STATUS_OK, wantSequence: 1, wantOutstanding: 1, wantPrevious: 2, wantCounted: true,
		},
		{
			name:            "older sequence fails a write",
			channelSequence: 7, outstanding: 2, previous: 1,
			command: smb2.SMB2_WRITE, cs: 6,
			status: smb2.STATUS_FILE_NOT_AVAILABLE, wantSequence: 7, wantOutstanding: 2, wantPrevious: 1, wantCounted: false,
		},
		{
			name:            "older sequence lets a read through uncounted",
			channelSequence: 7, outstanding: 2, previous: 1,
			command: smb2.SMB2_READ, cs: 6,
			status: smb2.STATUS_OK, wantSequence: 7, wantOutstanding: 2, wantPrevious: 1, wantCounted: false,
		},
		{
			name:            "replay of the same sequence with nothing pending",
			channelSequence: 7, outstanding: 2, previous: 0,
			command: smb2.SMB2_WRITE, cs: 7, replay: true,
			status: smb2.STATUS_OK, wantSequence: 7, wantOutstanding: 3, wantPrevious: 0, wantCounted: true,
		},
		{
			name:            "replay of the same sequence with requests pending",
			channelSequence: 7, outstanding: 2, previous: 1,
			command: smb2.SMB2_SET_INFO, cs: 7, replay: true,
			status: smb2.STATUS_FILE_NOT_AVAILABLE, wantSequence: 7, wantOutstanding: 2, wantPrevious: 1, wantCounted: false,
		},
		{
			name:            "replay of a newer sequence with nothing pending",
			channelSequence: 7, outstanding: 0, previous: 0,
			command: smb2.SMB2_IOCTL, cs: 8, replay: true,
			status: smb2.STATUS_OK, wantSequence: 8, wantOutstanding: 1, wantPrevious: 0, wantCounted: true,
		},
		{
			name:            "replay of a newer sequence with requests pending",
			channelSequence: 7, outstanding: 2, previous: 0,
			command: smb2.SMB2_IOCTL, cs: 8, replay: true,
			status: smb2.STATUS_FILE_NOT_AVAILABLE, wantSequence: 8, wantOutstanding: 0, wantPrevious: 2, wantCounted: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := &connection{
				negotiateDialect: smb2.SMB_DIALECT_311,
				requestOpens:     make(map[uint64]*open),
			}
			op := &open{
				file:                            &fileState{},
				channelSequence:                 test.channelSequence,
				outstandingRequestCount:         test.outstanding,
				outstandingPreviousRequestCount: test.previous,
			}
			req := newTestRequest(t, test.command, test.cs, test.replay)

			status := c.verifyChannelSequence(op, req)
			if status != test.status {
				t.Errorf("status = %#x, want %#x", status, test.status)
			}
			if op.channelSequence != test.wantSequence {
				t.Errorf("Open.ChannelSequence = %d, want %d", op.channelSequence, test.wantSequence)
			}
			if op.outstandingRequestCount != test.wantOutstanding {
				t.Errorf("Open.OutstandingRequestCount = %d, want %d", op.outstandingRequestCount, test.wantOutstanding)
			}
			if op.outstandingPreviousRequestCount != test.wantPrevious {
				t.Errorf("Open.OutstandingPreRequestCount = %d, want %d", op.outstandingPreviousRequestCount, test.wantPrevious)
			}

			// A counted request must be remembered, so that the counter it was added to is
			// decremented when the response goes out; an uncounted one must not be.
			_, remembered := c.requestOpens[req.Header().MessageID()]
			if remembered != test.wantCounted {
				t.Errorf("request remembered = %v, want %v", remembered, test.wantCounted)
			}
		})
	}
}

func TestVerifyChannelSequenceSkippedForOldDialects(t *testing.T) {
	c := &connection{
		negotiateDialect: smb2.SMB_DIALECT_21,
		requestOpens:     make(map[uint64]*open),
	}
	op := &open{file: &fileState{}, channelSequence: 7, outstandingRequestCount: 2}
	req := newTestRequest(t, smb2.SMB2_WRITE, 9, false)

	if status := c.verifyChannelSequence(op, req); status != smb2.STATUS_OK {
		t.Fatalf("status = %#x, want STATUS_OK", status)
	}
	if op.channelSequence != 7 || op.outstandingRequestCount != 2 {
		t.Errorf("the counters of the Open were touched: %d, %d", op.channelSequence, op.outstandingRequestCount)
	}
	if len(c.requestOpens) != 0 {
		t.Error("the request was remembered, but it wasn't counted")
	}
}

// TestDialectName pins the spelling of each dialect. The name is not decoration: several rules
// are written as a comparison against it, so a wrong string here silently turns a per-dialect
// check off.
func TestDialectName(t *testing.T) {
	names := map[uint16]string{
		smb2.SMB_DIALECT_202:     "2.0.2",
		smb2.SMB_DIALECT_21:      "2.1",
		smb2.SMB_DIALECT_30:      "3.0",
		smb2.SMB_DIALECT_302:     "3.0.2",
		smb2.SMB_DIALECT_311:     "3.1.1",
		smb2.SMB_DIALECT_UNKNOWN: "Unknown",
	}

	for dialect, want := range names {
		if got := dialectName(dialect); got != want {
			t.Errorf("dialectName(%#x) = %q, want %q", dialect, got, want)
		}
	}
}
