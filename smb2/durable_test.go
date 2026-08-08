package smb2

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestParseDurableHandleRequestV2(t *testing.T) {
	data := make([]byte, durableHandleRequestV2Size)
	binary.LittleEndian.PutUint32(data[0:4], 30_000) // Timeout
	binary.LittleEndian.PutUint32(data[4:8], DHANDLE_FLAG_PERSISTENT)
	binary.LittleEndian.PutUint64(data[8:16], 0xdeadbeefdeadbeef) // Reserved, ignored
	guid := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	copy(data[16:32], guid)

	req, ok := ParseDurableHandleRequestV2(data)
	if !ok {
		t.Fatal("a context of the full size was rejected")
	}
	if req.Timeout != 30_000 {
		t.Errorf("Timeout = %d, want 30000", req.Timeout)
	}
	if req.Flags != DHANDLE_FLAG_PERSISTENT {
		t.Errorf("Flags = %#x, want %#x", req.Flags, DHANDLE_FLAG_PERSISTENT)
	}
	if !bytes.Equal(req.CreateGuid[:], guid) {
		t.Errorf("CreateGuid = % x, want % x", req.CreateGuid, guid)
	}

	// A truncated context must be refused rather than read past its end.
	if _, ok := ParseDurableHandleRequestV2(data[:durableHandleRequestV2Size-1]); ok {
		t.Error("a truncated context was accepted")
	}
	if _, ok := ParseDurableHandleRequestV2(nil); ok {
		t.Error("an empty context was accepted")
	}
}

func TestParseDurableHandleReconnectV2(t *testing.T) {
	data := make([]byte, durableHandleReconnectV2Size)

	// The file ID is the one the server handed out, in the order the rest of the server
	// reads it: the volatile half first, the durable half second.
	binary.LittleEndian.PutUint64(data[0:8], 0x1111222233334444)
	binary.LittleEndian.PutUint64(data[8:16], 0x5555666677778888)
	guid := []byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	copy(data[16:32], guid)
	binary.LittleEndian.PutUint32(data[32:36], DHANDLE_FLAG_PERSISTENT)

	rec, ok := ParseDurableHandleReconnectV2(data)
	if !ok {
		t.Fatal("a context of the full size was rejected")
	}
	if rec.FileID != 0x1111222233334444 {
		t.Errorf("FileID = %#x, want %#x", rec.FileID, uint64(0x1111222233334444))
	}
	if rec.DurableID != 0x5555666677778888 {
		t.Errorf("DurableID = %#x, want %#x", rec.DurableID, uint64(0x5555666677778888))
	}
	if !bytes.Equal(rec.CreateGuid[:], guid) {
		t.Errorf("CreateGuid = % x, want % x", rec.CreateGuid, guid)
	}
	if rec.Flags != DHANDLE_FLAG_PERSISTENT {
		t.Errorf("Flags = %#x, want %#x", rec.Flags, DHANDLE_FLAG_PERSISTENT)
	}

	if _, ok := ParseDurableHandleReconnectV2(data[:durableHandleReconnectV2Size-1]); ok {
		t.Error("a truncated context was accepted")
	}
}

func TestHandleCreateDurableHandleRequestV2(t *testing.T) {
	resp := HandleCreateDurableHandleRequestV2(60_000)
	if len(resp) != 8 {
		t.Fatalf("len = %d, want 8", len(resp))
	}
	if timeout := binary.LittleEndian.Uint32(resp[0:4]); timeout != 60_000 {
		t.Errorf("Timeout = %d, want 60000", timeout)
	}

	// The server grants no persistent handles, so the flags stay clear.
	if flags := binary.LittleEndian.Uint32(resp[4:8]); flags != 0 {
		t.Errorf("Flags = %#x, want 0", flags)
	}
}
