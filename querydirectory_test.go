package main

import (
	"encoding/binary"
	"fmt"
	"slices"
	"sync"
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

	return cl.queryDirectoryAs(fid, smb2.FILE_DIRECTORY_INFORMATION, pattern)
}

// queryDirectoryAs is the same search asking for a particular information class, which is what the
// client chooses and the server has to know how to lay a listing out in.
func (cl *testClient) queryDirectoryAs(fid []byte, class uint8, pattern string) []byte {
	cl.h.t.Helper()

	cl.mid++
	resp, err := cl.send(queryDirectoryRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, class, pattern, 4096))
	if err != nil {
		cl.h.t.Fatalf("search of %q: %v", pattern, err)
	}

	if resp.Header().Status() != smb2.STATUS_PENDING {
		return resp.Encode()
	}

	return cl.recv(20 * time.Second)
}

// listedNames returns the names carried by a query directory response laid out as
// FILE_DIRECTORY_INFORMATION, whose fixed part runs to 64 bytes.
func listedNames(t *testing.T, buf []byte) []string {
	t.Helper()

	return listedNamesOfWidth(t, buf, 64)
}

// listedNamesOfWidth returns the names carried by a query directory response, which are held one
// after another in the output buffer, each entry saying how far the next one lies. The width is the
// fixed part of an entry, which differs from one information class to the next; the name follows it.
// FileNameLength lies at offset 60 in every class here, whatever trails it before the name itself.
func listedNamesOfWidth(t *testing.T, buf []byte, width uint32) []string {
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
	for uint32(len(entries)) >= width {
		nameLen := binary.LittleEndian.Uint32(entries[60:64])
		if width+nameLen > uint32(len(entries)) {
			t.Fatalf("an entry names %d bytes of file name, in %d", nameLen, uint32(len(entries))-width)
		}

		names = append(names, utils.DecodeToString(entries[width:width+nameLen]))

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
// a shell glob reads as syntax.
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

// TestIntegrationQueryDirectoryFullDirectoryInformation is the information class a client may ask a
// listing in that the server used to turn away. The encoder for it was written and complete, and
// nothing was wired to it: the class was refused with STATUS_NOT_SUPPORTED before it ever reached
// the layout, and had the refusal been dropped alone the switch behind it would have answered an
// empty listing instead. Both ends are checked here - the class is accepted, and what comes back
// carries the files.
func TestIntegrationQueryDirectoryFullDirectoryInformation(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	h.files.putDir("docs")
	h.files.put("docs/first.txt", 1024)
	h.files.put("docs/second.txt", 2048)

	// The listing is held against the class that was already answered, on the same directory. A
	// search exhausts the handle it runs on, so each of the two gets one of its own.
	want := listedNames(t, cl.queryDirectory(createdFileID(cl.openDir("docs")), "*"))

	// The fixed part of a FILE_FULL_DIR_INFORMATION entry runs to 68 bytes: FILE_DIRECTORY_INFORMATION
	// with EaSize inserted ahead of the name ([MS-FSCC] 2.4.14). Reading the names out at that width
	// is itself the check on the layout - at any other, the names would not come out whole.
	fid := createdFileID(cl.openDir("docs"))
	got := listedNamesOfWidth(t, cl.queryDirectoryAs(fid, smb2.FILE_FULL_DIRECTORY_INFORMATION, "*"), 68)

	// The two are held against each other as sets: nothing here fixes the order a listing comes
	// back in, and the classes differ in their layout rather than in what they enumerate.
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the listing carries %v, want the %v the answered class gives", got, want)
	}

	// And the files themselves are in it, so that two classes agreeing on an empty listing cannot
	// pass for two classes agreeing on the directory.
	var files int
	for _, name := range got {
		if name == "first.txt" || name == "second.txt" {
			files++
		}
	}
	if files != 2 {
		t.Errorf("the listing is %v, want both files the directory holds", got)
	}
}

// TestIntegrationTwoSearchesOnOneHandleDoNotOvertakeEachOther carries one enumeration on from two
// places at once, as two channels of a session may. What each search sends has to be taken off the
// handle by the same act that works out how much to send: counted against the results as they were
// read and taken off them afterwards, the second search takes a count that the first has already
// made too large, and either sends a name twice or takes more than is there.
func TestIntegrationTwoSearchesOnOneHandleDoNotOvertakeEachOther(t *testing.T) {
	h := newSMBTest(t)
	h.files.putDir("dir")
	for i := range 200 {
		h.files.put(fmt.Sprintf("dir/file%03d", i), 1024)
	}

	alice := h.dial("alice")
	handle := alice.openDir("dir")
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("opening the directory was answered with %#x", status)
	}
	fid := createdFileID(handle)

	// What the enumeration comes to with nothing racing it, read on a handle of its own.
	want := make(map[string]int)
	ref := createdFileID(alice.openDir("dir"))
	for range 100 {
		answer := alice.queryDirectory(ref, "*")
		if smb2.Header(answer).Status() != smb2.STATUS_OK {
			break
		}
		for _, name := range listedNames(t, answer) {
			want[name]++
		}
	}

	// The first search runs the enumeration and leaves what would not fit on the handle.
	seen := make(map[string]int)
	for _, name := range listedNames(t, alice.queryDirectory(fid, "*")) {
		seen[name]++
	}

	// Two searches carry it on at the same moment, each with room for everything that is left.
	reqs := make([]*smb2.Request, 2)
	for i := range reqs {
		alice.mid++
		msg := queryDirectoryRequest(alice.mid, alice.ss.sessionID, alice.tc.treeID, fid,
			smb2.FILE_DIRECTORY_INFORMATION, "*", 65536)
		parsed, err := smb2.GetRequests(msg, 0, false)
		if err != nil {
			t.Fatalf("the search did not parse as a request: %v", err)
		}
		reqs[i] = parsed[0]
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	answers := make([][]byte, len(reqs))
	for i, r := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if resp, _, err := alice.conn.processRequest(r); err == nil {
				answers[i] = resp.Encode()
			}
		}()
	}
	close(start)
	wg.Wait()

	for i, answer := range answers {
		if answer == nil {
			t.Fatalf("the server gave up on search %d", i)
		}
		if status := smb2.Header(answer).Status(); status != smb2.STATUS_OK {
			continue // One of the two may find the enumeration already finished.
		}
		for _, name := range listedNames(t, answer) {
			seen[name]++
		}
	}

	for name, n := range seen {
		if n != 1 {
			t.Errorf("%s was listed %d times, want once", name, n)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("the enumeration listed %d entries, want the %d it holds", len(seen), len(want))
	}
}

// TestAnErrorOnALiveSessionIsStillEncrypted is the account that disappears while its session is
// still running. The request fails, but it fails on a session that encrypts everything it carries,
// so the answer has to be encrypted as well: answered as though there were no session at all, it
// goes out in the clear on a session whose client will not read it that way.
func TestAnErrorOnALiveSessionIsStillEncrypted(t *testing.T) {
	h := newSMBTest(t)
	h.files.putDir("dir")
	h.files.put("dir/file", 1024)

	alice := h.dial("alice").encrypting()
	fid := createdFileID(alice.openDir("dir"))

	// The account behind the session is no longer one the store knows, which is what the search
	// is about to fail on.
	alice.ss.userName = "nobody"

	alice.mid++
	msg := queryDirectoryRequest(alice.mid, alice.ss.sessionID, alice.tc.treeID, fid,
		smb2.FILE_DIRECTORY_INFORMATION, "*", 4096)
	reqs, err := smb2.GetRequests(msg, 0, false)
	if err != nil {
		t.Fatalf("the search did not parse as a request: %v", err)
	}

	resp, ss, err := alice.conn.processRequest(reqs[0])
	if err != nil {
		t.Fatalf("the server gave up on the search: %v", err)
	}
	if status := resp.Header().Status(); status != smb2.STATUS_USER_SESSION_DELETED {
		t.Fatalf("the search was answered %#x, want it failed on the missing account", status)
	}

	buf := h.srv.encodeResponse(alice.conn, ss, resp)
	if id := smb2.Header(buf).ProtocolID(); id != smb2.PROTOCOL_SMB2_ENCRYPTED {
		t.Errorf("the answer carries protocol ID %#x, want it encrypted like the rest of the session", id)
	}
}
