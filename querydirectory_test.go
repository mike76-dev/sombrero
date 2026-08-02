package main

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/utils"
)

// queryDirectoryRequest builds the bytes of an SMB2_QUERY_DIRECTORY request. The search pattern
// goes in the buffer that follows the fixed part of the body, which is where the offset and the
// length in the body point.
func queryDirectoryRequest(mid, sid uint64, tid uint32, fid []byte, class uint8, pattern string, outputLen uint32) []byte {
	name := utils.EncodeStringToBytes(pattern)
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2QueryDirectoryRequestMinSize+len(name))
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_QUERY_DIRECTORY)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2QueryDirectoryRequestStructureSize)
	body[2] = class
	copy(body[8:24], fid)
	binary.LittleEndian.PutUint16(body[24:26], uint16(smb2.SMB2HeaderSize+smb2.SMB2QueryDirectoryRequestMinSize))
	binary.LittleEndian.PutUint16(body[26:28], uint16(len(name)))
	binary.LittleEndian.PutUint32(body[28:32], outputLen)
	copy(msg[smb2.SMB2HeaderSize+smb2.SMB2QueryDirectoryRequestMinSize:], name)

	return msg
}

// queryDirectory searches a directory handle for a pattern and returns the response as it goes on
// the wire.
func (cl *testClient) queryDirectory(fid []byte, pattern string) []byte {
	cl.h.t.Helper()

	cl.mid++
	resp, err := cl.send(queryDirectoryRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, smb2.FILE_DIRECTORY_INFORMATION, pattern, 4096))
	if err != nil {
		cl.h.t.Fatalf("search of %q: %v", pattern, err)
	}

	if resp.Header().Status() != smb2.STATUS_PENDING {
		return resp.Encode()
	}

	return cl.recv(20 * time.Second)
}

// listedNames returns the names carried by a query directory response, which are held one after
// another in the output buffer, each entry saying how far the next one lies.
func listedNames(t *testing.T, buf []byte) []string {
	t.Helper()

	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the search was answered with %#x", status)
	}

	off := binary.LittleEndian.Uint16(buf[smb2.SMB2HeaderSize+2 : smb2.SMB2HeaderSize+4])
	length := binary.LittleEndian.Uint32(buf[smb2.SMB2HeaderSize+4 : smb2.SMB2HeaderSize+8])
	if uint32(off)+length > uint32(len(buf)) {
		t.Fatalf("the output buffer runs past the response: %d bytes from %d, in %d", length, off, len(buf))
	}

	var names []string
	entries := buf[uint32(off) : uint32(off)+length]
	for len(entries) >= 64 {
		nameLen := binary.LittleEndian.Uint32(entries[60:64])
		if 64+nameLen > uint32(len(entries)) {
			t.Fatalf("an entry names %d bytes of file name, in %d", nameLen, len(entries)-64)
		}

		names = append(names, utils.DecodeToString(entries[64:64+nameLen]))

		next := binary.LittleEndian.Uint32(entries[:4])
		if next == 0 { // The last entry says so by pointing nowhere.
			break
		}
		if next > uint32(len(entries)) {
			t.Fatalf("an entry points %d bytes on, in %d", next, len(entries))
		}
		entries = entries[next:]
	}

	return names
}

// TestIntegrationQueryDirectoryFindsABracketedName is the file whose name carries the punctuation
// a shell glob reads as syntax. "[MS-SMB2].pdf" was uploaded and listed, and the client then asked
// for it by name the way a client does when it refreshes a window - a search whose pattern is the
// whole name and nothing else. The pattern used to be handed to a glob matcher, which read the
// brackets as a set of characters to choose one of, found nothing that matched and answered that
// no such file existed. The file went out of the window on the client while sitting in the store
// untouched.
func TestIntegrationQueryDirectoryFindsABracketedName(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	h.files.putDir("docs")
	h.files.put("docs/[MS-SMB2].pdf", 1024)
	fid := createdFileID(cl.openDir("docs"))

	names := listedNames(t, cl.queryDirectory(fid, "[MS-SMB2].pdf"))

	if len(names) != 1 || names[0] != "[MS-SMB2].pdf" {
		t.Fatalf("the search for the file by name found %v, want the file itself", names)
	}
}

// TestIntegrationQueryDirectoryAnswersTheFirstSearch is the answer to the search that runs it,
// rather than to the one after. The results used to be read off the handle before the search
// filled it, so the first answer carried an empty buffer under a success status: a client that
// reads that as the end of the enumeration is told the directory holds nothing.
func TestIntegrationQueryDirectoryAnswersTheFirstSearch(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	h.files.putDir("docs")
	h.files.put("docs/notes.txt", 12)
	fid := createdFileID(cl.openDir("docs"))

	names := listedNames(t, cl.queryDirectory(fid, "*"))

	var found bool
	for _, name := range names {
		if name == "notes.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the first answer to the search carries %v, want the file the directory holds", names)
	}

	// The handle has now given out everything it found, and says as much rather than starting
	// the same search over.
	buf := cl.queryDirectory(fid, "*")
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_NO_MORE_FILES {
		t.Errorf("the search was asked again and answered with %#x, want no more files", status)
	}
}

// TestIntegrationQueryDirectoryMissesWhatIsNotThere is the other side of it: a name the directory
// does not hold is still answered with no such file, so that a matcher taking names literally has
// not simply been made to match everything.
func TestIntegrationQueryDirectoryMissesWhatIsNotThere(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	h.files.putDir("docs")
	h.files.put("docs/[MS-SMB2].pdf", 1024)
	fid := createdFileID(cl.openDir("docs"))

	buf := cl.queryDirectory(fid, "M.pdf")
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_NO_SUCH_FILE {
		t.Errorf("the search for a name the directory does not hold was answered with %#x, want no such file", status)
	}
}
