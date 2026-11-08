package main

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/utils"
)

// ioctlRequest formats an SMB2_IOCTL request for an FSCTL that carries no input and
// takes up to maxOutput bytes of output.
func ioctlRequest(mid, sid uint64, tid uint32, fid []byte, code, maxOutput uint32) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2IoctlRequestMinSize)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_IOCTL)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2IoctlRequestStructureSize)
	binary.LittleEndian.PutUint32(body[4:8], code)
	copy(body[8:24], fid)
	binary.LittleEndian.PutUint32(body[44:48], maxOutput)
	binary.LittleEndian.PutUint32(body[48:52], smb2.IOCTL_IS_FSCTL)

	return msg
}

// ioctl sends an FSCTL on the handle and returns the response.
func (cl *testClient) ioctl(fid []byte, code, maxOutput uint32) ([]byte, error) {
	cl.mid++
	resp, err := cl.send(ioctlRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, code, maxOutput))
	if err != nil {
		return nil, err
	}
	return resp.Encode(), nil
}

// ioctlOutput sends an FSCTL on the handle and returns the output buffer of the
// response, which must succeed.
func ioctlOutput(t *testing.T, cl *testClient, fid []byte, code, maxOutput uint32) []byte {
	t.Helper()

	buf, err := cl.ioctl(fid, code, maxOutput)
	if err != nil {
		t.Fatalf("the ioctl failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the ioctl was answered with %#x", status)
	}
	body := buf[smb2.SMB2HeaderSize:]
	offset := binary.LittleEndian.Uint32(body[32:36])
	length := binary.LittleEndian.Uint32(body[36:40])
	if int(offset+length) > len(buf) {
		t.Fatalf("the ioctl names %d bytes at offset %d of a %d byte response", length, offset, len(buf))
	}
	return buf[offset : offset+length]
}

// snapshotArray reads an SRV_SNAPSHOT_ARRAY: the counts, and the names it carries.
func snapshotArray(t *testing.T, out []byte) (total, returned, size uint32, names []string) {
	t.Helper()

	if len(out) < 12 {
		t.Fatalf("the snapshot array is %d bytes, too few for its header", len(out))
	}
	total = binary.LittleEndian.Uint32(out[0:4])
	returned = binary.LittleEndian.Uint32(out[4:8])
	size = binary.LittleEndian.Uint32(out[8:12])
	rest := out[12:]
	for len(rest) >= 2 {
		end := 0
		for end+1 < len(rest) && (rest[end] != 0 || rest[end+1] != 0) {
			end += 2
		}
		if end == 0 {
			break
		}
		names = append(names, utils.DecodeToString(rest[:end]))
		rest = rest[end+2:]
	}
	return
}

// createdSize returns the EndOfFile field of a create response.
func createdSize(buf []byte) uint64 {
	return binary.LittleEndian.Uint64(buf[smb2.SMB2HeaderSize+48 : smb2.SMB2HeaderSize+56])
}

// TestIntegrationPreviousVersionsAreEnumerated verifies that a file's versions are
// listed by their snapshot names, newest first, once the client has room for them.
func TestIntegrationPreviousVersionsAreEnumerated(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("dir/file", []byte("now"))
	older := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 11, 5, 17, 30, 0, 0, time.UTC)
	h.files.putVersion("dir/file", older, []byte("last week"))
	h.files.putVersion("dir/file", newer, []byte("yesterday"))
	h.files.putData("dir/plain", []byte("x"))

	alice := h.dial("alice")
	buf, _ := alice.createReading("dir/file", smb2.OPLOCK_LEVEL_NONE)
	fid := createdFileID(buf)

	// Asked with room for the count alone, the server says how much room the names need.
	out := ioctlOutput(t, alice, fid, smb2.FSCTL_SRV_ENUMERATE_SNAPSHOTS, 16)
	total, returned, size, names := snapshotArray(t, out)
	if total != 2 || returned != 0 || size != 2*50+2 || len(names) != 0 {
		t.Errorf("counting: got %d versions, %d returned, %d bytes, names %q", total, returned, size, names)
	}

	out = ioctlOutput(t, alice, fid, smb2.FSCTL_SRV_ENUMERATE_SNAPSHOTS, 65536)
	total, returned, size, names = snapshotArray(t, out)
	want := []string{"@GMT-2026.11.05-17.30.00", "@GMT-2026.11.01-09.00.00"}
	if total != 2 || returned != 2 || size != 2*50+2 || len(names) != 2 || names[0] != want[0] || names[1] != want[1] {
		t.Errorf("listing: got %d versions, %d returned, %d bytes, names %q, want %q", total, returned, size, names, want)
	}

	// Too little room even for the count is a bad request.
	buf, err := alice.ioctl(fid, smb2.FSCTL_SRV_ENUMERATE_SNAPSHOTS, 8)
	if err != nil {
		t.Fatalf("the ioctl failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Errorf("a buffer of 8 bytes: Status = %#x, want %#x", status, smb2.STATUS_INVALID_PARAMETER)
	}

	// A file with no versions has none to list.
	buf, _ = alice.createReading("dir/plain", smb2.OPLOCK_LEVEL_NONE)
	out = ioctlOutput(t, alice, createdFileID(buf), smb2.FSCTL_SRV_ENUMERATE_SNAPSHOTS, 65536)
	if total, returned, size, names := snapshotArray(t, out); total != 0 || returned != 0 || size != 2 || len(names) != 0 {
		t.Errorf("no versions: got %d versions, %d returned, %d bytes, names %q", total, returned, size, names)
	}
}

// TestIntegrationAPreviousVersionOpens verifies that a version opens under its snapshot
// name or by a timewarp token, reads as it was, and is given no more than reading.
func TestIntegrationAPreviousVersionOpens(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("dir/file", []byte("now"))
	at := time.Date(2026, 11, 5, 17, 30, 0, 0, time.UTC)
	h.files.putVersion("dir/file", at, []byte("before"))

	alice := h.dial("alice")
	name := smb2.SnapshotToken(at) + "/dir/file"
	buf, err := alice.createAccessing(name, smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, readAccess, nil)
	if err != nil {
		t.Fatalf("opening the version: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("opening the version: Status = %#x, want %#x", status, smb2.STATUS_OK)
	}
	if size := createdSize(buf); size != 6 {
		t.Errorf("the version opened at %d bytes, want 6", size)
	}
	fid := createdFileID(buf)
	buf, err = alice.readOver(fid, 16, 0)
	if err != nil {
		t.Fatalf("reading the version: %v", err)
	}
	if got := string(readData(t, buf)); got != "before" {
		t.Errorf("the version read as %q, want %q", got, "before")
	}
	if _, err := alice.closeHandle(fid); err != nil {
		t.Fatalf("closing the version: %v", err)
	}

	// The file itself is as it is now.
	buf, _ = alice.createReading("dir/file", smb2.OPLOCK_LEVEL_NONE)
	buf, _ = alice.readOver(createdFileID(buf), 16, 0)
	if got := string(readData(t, buf)); got != "now" {
		t.Errorf("the file read as %q, want %q", got, "now")
	}

	// A version is not there to be written, made, or found under a name it does not go by.
	buf, _ = alice.createAccessing(name, smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, writeAccess, nil)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_ACCESS_DENIED {
		t.Errorf("writing a version: Status = %#x, want %#x", status, smb2.STATUS_ACCESS_DENIED)
	}
	buf, _ = alice.createAccessing(name, smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OVERWRITE_IF, readAccess, nil)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_ACCESS_DENIED {
		t.Errorf("overwriting a version: Status = %#x, want %#x", status, smb2.STATUS_ACCESS_DENIED)
	}
	other := smb2.SnapshotToken(at.Add(time.Hour)) + "/dir/file"
	buf, _ = alice.createAccessing(other, smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, readAccess, nil)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OBJECT_NAME_NOT_FOUND {
		t.Errorf("a snapshot the file has no version at: Status = %#x, want %#x", status, smb2.STATUS_OBJECT_NAME_NOT_FOUND)
	}
	buf, _ = alice.createAccessing("@GMT-not.a.time/dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, readAccess, nil)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Errorf("a malformed snapshot name: Status = %#x, want %#x", status, smb2.STATUS_INVALID_PARAMETER)
	}

	// A timewarp token names the same version without touching the path.
	token := make([]byte, 8)
	binary.LittleEndian.PutUint64(token, utils.UnixToFiletime(at))
	buf, err = alice.createAccessing("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, readAccess, createContext(smb2.CREATE_TIMEWARP_TOKEN, token))
	if err != nil {
		t.Fatalf("opening the version by token: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("opening the version by token: Status = %#x, want %#x", status, smb2.STATUS_OK)
	}
	if size := createdSize(buf); size != 6 {
		t.Errorf("the version opened by token at %d bytes, want 6", size)
	}
}

// TestIntegrationOverwritingKeepsTheVersions verifies that a create overwriting
// a file leaves its versions readable, and makes the overwritten contents one.
func TestIntegrationOverwritingKeepsTheVersions(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("dir/file", []byte("now"))
	at := time.Date(2026, 11, 5, 17, 30, 0, 0, time.UTC)
	h.files.putVersion("dir/file", at, []byte("before"))

	alice := h.dial("alice")
	name := smb2.SnapshotToken(at) + "/dir/file"
	buf, _ := alice.createAccessing(name, smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, readAccess, nil)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("opening the version: Status = %#x, want %#x", status, smb2.STATUS_OK)
	}
	version := createdFileID(buf)

	buf, _ = alice.createAccessing("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OVERWRITE_IF, writeAccess, nil)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("overwriting the file: Status = %#x, want %#x", status, smb2.STATUS_OK)
	}
	live := createdFileID(buf)

	buf, err := alice.readOver(version, 16, 0)
	if err != nil {
		t.Fatalf("reading the version after the overwrite: %v", err)
	}
	if got := string(readData(t, buf)); got != "before" {
		t.Errorf("the version read as %q, want %q", got, "before")
	}

	if _, err := alice.write(live, 0, []byte("after")); err != nil {
		t.Fatalf("writing the file: %v", err)
	}
	if _, err := alice.closeHandle(live); err != nil {
		t.Fatalf("closing the file: %v", err)
	}
	if _, err := alice.closeHandle(version); err != nil {
		t.Fatalf("closing the version: %v", err)
	}

	h.files.mu.Lock()
	contents := string(h.files.contents["dir/file"])
	versions := h.files.versions["dir/file"]
	h.files.mu.Unlock()
	if contents != "after" {
		t.Errorf("the file holds %q, want %q", contents, "after")
	}
	if len(versions) != 2 || string(versions[0].data) != "now" {
		t.Errorf("want the overwritten contents kept as the newest version, got %d versions", len(versions))
	}
}
