package main

import (
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// A read or a write that names a channel other than none is asking for its payload to travel
// over RDMA rather than in the request itself. This server has no SMB Direct behind it, so there
// is no way to reach the buffer the client is describing, and the request has to be refused
// ([MS-SMB2] 3.3.5.12, 3.3.5.13).

func TestIntegrationWriteOverRDMAChannelIsRefused(t *testing.T) {
	channels := map[string]uint32{
		"RDMA v1":            smb2.SMB2_CHANNEL_RDMA_V1,
		"RDMA v1 invalidate": smb2.SMB2_CHANNEL_RDMA_V1_INVALIDATE,
		"RDMA transform":     smb2.SMB2_CHANNEL_RDMA_TRANSFORM,
		// A value the spec gives no meaning to fails by the same rule: the field has to hold
		// exactly one of the ones it names.
		"unknown": 0x99,
	}

	for name, channel := range channels {
		t.Run(name, func(t *testing.T) {
			h := newSMBTest(t)
			h.files.put("dir/file", 1024)

			alice := h.dial("alice")
			held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

			resp, err := alice.writeOver(createdFileID(held), []byte("hello"), channel)
			if err != nil {
				t.Fatalf("the write did not come back: %v", err)
			}
			if status := smb2.Header(resp).Status(); status != smb2.STATUS_INVALID_PARAMETER {
				t.Errorf("the write returned %#x, want invalid parameter", status)
			}
		})
	}
}

func TestIntegrationReadOverRDMAChannelIsRefused(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	resp, err := alice.readOver(createdFileID(held), 512, smb2.SMB2_CHANNEL_RDMA_V1)
	if err != nil {
		t.Fatalf("the read did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Errorf("the read returned %#x, want invalid parameter", status)
	}
}

func TestIntegrationReadAndWriteOverNoChannelAreAllowed(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	resp, err := alice.writeOver(createdFileID(held), []byte("hello"), smb2.SMB2_CHANNEL_NONE)
	if err != nil {
		t.Fatalf("the write did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Errorf("a write carrying its own data returned %#x, want success", status)
	}

	resp, err = alice.readOver(createdFileID(held), 512, smb2.SMB2_CHANNEL_NONE)
	if err != nil {
		t.Fatalf("the read did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Errorf("a read wanting its data in the response returned %#x, want success", status)
	}
}

// Before the 3.x dialects the field is reserved rather than meaningful: the client sets it to
// zero and the server takes whatever arrives as nothing at all. Refusing there would turn a
// stray byte from an old client into a failed write.
func TestIntegrationChannelIsIgnoredBeforeSMB3(t *testing.T) {
	for _, dialect := range []uint16{smb2.SMB_DIALECT_202, smb2.SMB_DIALECT_21} {
		t.Run(dialectName(dialect), func(t *testing.T) {
			h := newSMBTest(t)
			h.files.put("dir/file", 1024)

			alice := h.dial("alice").speaking(dialect)
			held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

			resp, err := alice.writeOver(createdFileID(held), []byte("hello"), smb2.SMB2_CHANNEL_RDMA_V1)
			if err != nil {
				t.Fatalf("the write did not come back: %v", err)
			}
			if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
				t.Errorf("the write returned %#x on a dialect where the field is reserved, want success", status)
			}
		})
	}
}

// Once the data is known to travel in the request itself, it has to start somewhere the request
// can plausibly have put it. The fixed part ends at 0x70, so everything up to 0x100 is padding
// the client is free to insert; beyond that the request is malformed ([MS-SMB2] 3.3.5.13).
func TestIntegrationWriteFromTooFarInIsRefused(t *testing.T) {
	// The bound is written out here rather than taken from smb2.MaxWriteDataOffset: a test that
	// derives its boundaries from the constant under test moves along with it, and would pass
	// whatever the constant were changed to.
	offsets := map[string]struct {
		dataOff int
		want    uint32
	}{
		"straight after the fixed part": {smb2.SMB2HeaderSize + smb2.SMB2WriteRequestMinSize, smb2.STATUS_OK},
		"as far in as padding may go":   {0x100, smb2.STATUS_OK},
		"one byte too far":              {0x101, smb2.STATUS_INVALID_PARAMETER},
		"far past the fixed part":       {4096, smb2.STATUS_INVALID_PARAMETER},
	}

	for name, tc := range offsets {
		t.Run(name, func(t *testing.T) {
			h := newSMBTest(t)
			h.files.put("dir/file", 1024)

			alice := h.dial("alice")
			held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

			resp, err := alice.writeFrom(createdFileID(held), []byte("hello"), tc.dataOff)
			if err != nil {
				t.Fatalf("the write did not come back: %v", err)
			}
			if status := smb2.Header(resp).Status(); status != tc.want {
				t.Errorf("a write with its data at %#x returned %#x, want %#x", tc.dataOff, status, tc.want)
			}
		})
	}
}

// Writes to a hidden file are answered as though they had gone through, since there is nothing
// behind them to store. That shortcut must not swallow a request the server has already found it
// cannot carry out: the client would be told its bytes had reached a buffer nobody read from.
func TestIntegrationRefusedChannelWriteToHiddenFile(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/.hidden", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/.hidden", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	// An ordinary write to it is accepted and goes nowhere, which is what makes the shortcut a
	// hazard for the one below.
	resp, err := alice.write(createdFileID(held), 0, []byte("hello"))
	if err != nil {
		t.Fatalf("the write did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Fatalf("an ordinary write to a hidden file returned %#x, want success", status)
	}

	resp, err = alice.writeOver(createdFileID(held), []byte("hello"), smb2.SMB2_CHANNEL_RDMA_V1)
	if err != nil {
		t.Fatalf("the write did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Errorf("the write returned %#x, want invalid parameter", status)
	}
}

// A write the server is going to refuse must not cost anybody their cache first: the file is
// left exactly as it was, so what the other clients hold is still good.
func TestIntegrationRefusedChannelWriteBreaksNothing(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.createReading("dir/file", smb2.OPLOCK_LEVEL_II)
	if level := createdOplockLevel(held); level != smb2.OPLOCK_LEVEL_II {
		t.Fatalf("alice was granted %#x rather than a level II oplock", level)
	}

	bob := h.dial("bob")
	opened, _ := bob.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	resp, err := bob.writeOver(createdFileID(opened), []byte("hello"), smb2.SMB2_CHANNEL_RDMA_V1)
	if err != nil {
		t.Fatalf("the write did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Fatalf("the write returned %#x, want invalid parameter", status)
	}

	alice.quiet(200*time.Millisecond, "a write that was refused broke somebody else's oplock")
}
