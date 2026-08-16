package main

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/utils"
)

// putPipe makes a named pipe appear in the store. A pipe is held under a bare name rather than
// under a path with a leading slash, which is what the create path tells it apart by.
func (fc *fakeClient) putPipe(name string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	now := time.Now()
	fc.objects[name] = client.ObjectInfo{Key: name, CreatedAt: now, ModifiedAt: now}
}

// queryInfoRequest builds the bytes of an SMB2_QUERY_INFO request.
func queryInfoRequest(mid, sid uint64, tid uint32, fid []byte, infoType, class uint8, outputLen uint32) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2QueryInfoRequestMinSize)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_QUERY_INFO)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2QueryInfoRequestStructureSize)
	body[2] = infoType
	body[3] = class
	binary.LittleEndian.PutUint32(body[4:8], outputLen)
	copy(body[24:40], fid)

	return msg
}

// queryInfo asks for an information class over a handle and returns the response as it goes on
// the wire.
func (cl *testClient) queryInfo(fid []byte, class uint8, outputLen uint32) []byte {
	cl.h.t.Helper()

	cl.mid++
	resp, err := cl.send(queryInfoRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, smb2.INFO_FILE, class, outputLen))
	if err != nil {
		cl.h.t.Fatalf("query of class %#x: %v", class, err)
	}

	if resp.Header().Status() != smb2.STATUS_PENDING {
		return resp.Encode()
	}

	return cl.recv(20 * time.Second)
}

// queriedInfo returns the output buffer of a query info response, failing the test unless the
// query went through.
func queriedInfo(t *testing.T, buf []byte) []byte {
	t.Helper()

	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the query was answered with %#x", status)
	}

	off := binary.LittleEndian.Uint16(buf[smb2.SMB2HeaderSize+2 : smb2.SMB2HeaderSize+4])
	length := binary.LittleEndian.Uint32(buf[smb2.SMB2HeaderSize+4 : smb2.SMB2HeaderSize+8])
	if uint32(off)+length > uint32(len(buf)) {
		t.Fatalf("the output buffer runs past the response: %d bytes from %d, in %d", length, off, len(buf))
	}

	return buf[uint32(off) : uint32(off)+length]
}

// openedFile puts a file of the given size in the store and opens it, returning the handle.
func (h *smbTest) openedFile(cl *testClient, name string, size uint64) []byte {
	h.t.Helper()

	h.files.put(name, size)
	buf, _ := cl.create(name, smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		h.t.Fatalf("the create of %s was answered with %#x", name, status)
	}

	return createdFileID(buf)
}

// decoded string reads a counted UTF-16 name out of an info structure: a length in the first four
// bytes and the name behind it.
func countedName(t *testing.T, buf []byte, at int) string {
	t.Helper()

	if len(buf) < at+4 {
		t.Fatalf("the structure stops before the name at %d", at)
	}

	length := int(binary.LittleEndian.Uint32(buf[at : at+4]))
	if at+4+length > len(buf) {
		t.Fatalf("the name of %d bytes runs past the structure", length)
	}

	return utils.DecodeToString(buf[at+4 : at+4+length])
}

// TestIntegrationQueryAllInformation is everything a client asks about a file at once. The whole
// of it is one structure, which the client reads by walking the fields in order, so what matters
// is that each of them stands where it belongs.
func TestIntegrationQueryAllInformation(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	fid := h.openedFile(cl, "docs/report.txt", 4096)

	info := queriedInfo(t, cl.queryInfo(fid, smb2.FileAllInformation, 4096))

	// The basic information leads: four times the same time, and the attributes.
	if attr := binary.LittleEndian.Uint32(info[32:36]); attr != smb2.FILE_ATTRIBUTE_NORMAL {
		t.Errorf("the file is marked %#x, want an ordinary file", attr)
	}

	// The standard information follows at 40: the sizes, the link count, and the two flags.
	if alloc := binary.LittleEndian.Uint64(info[40:48]); alloc != 4096 {
		t.Errorf("the file takes up %d bytes, want 4096", alloc)
	}
	if size := binary.LittleEndian.Uint64(info[48:56]); size != 4096 {
		t.Errorf("the file holds %d bytes, want 4096", size)
	}
	if info[60] != 0 {
		t.Error("the file is marked for deletion")
	}
	if info[61] != 0 {
		t.Error("the file is marked as a directory")
	}

	// The access the create granted is carried back at 76, and the position at 80.
	if access := binary.LittleEndian.Uint32(info[76:80]); access != shareAccess {
		t.Errorf("the handle carries access %#x, want %#x", access, uint32(shareAccess))
	}
	if pos := binary.LittleEndian.Uint64(info[80:88]); pos != 4096 {
		t.Errorf("the handle stands at %d, want the end of the file", pos)
	}

	// The name is last, and is the name of the file rather than the path to it.
	if name := countedName(t, info, 96); name != "report.txt" {
		t.Errorf("the file is named %q, want %q", name, "report.txt")
	}
}

// TestIntegrationQueryStandardInformation is the shorter answer about the same file: what it
// holds, whether it is a directory, and whether it is on its way out.
func TestIntegrationQueryStandardInformation(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	fid := h.openedFile(cl, "report.txt", 8192)

	info := queriedInfo(t, cl.queryInfo(fid, smb2.FileStandardInformation, 4096))
	if len(info) != 24 {
		t.Fatalf("the structure is %d bytes long, want 24", len(info))
	}

	if alloc := binary.LittleEndian.Uint64(info[:8]); alloc != 8192 {
		t.Errorf("the file takes up %d bytes, want 8192", alloc)
	}
	if size := binary.LittleEndian.Uint64(info[8:16]); size != 8192 {
		t.Errorf("the file holds %d bytes, want 8192", size)
	}
	if links := binary.LittleEndian.Uint32(info[16:20]); links != 0 {
		t.Errorf("the file has %d links, want none counted for an ordinary file", links)
	}
	if info[20] != 0 {
		t.Error("the file is marked for deletion")
	}
	if info[21] != 0 {
		t.Error("the file is marked as a directory")
	}
}

// TestIntegrationQueryStandardInformationOfADirectory is the same question about a directory. A
// directory holds nothing of its own, so the sizes stay at zero however the entry was made.
func TestIntegrationQueryStandardInformationOfADirectory(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	buf := cl.openDir("docs")
	fid := createdFileID(buf)

	info := queriedInfo(t, cl.queryInfo(fid, smb2.FileStandardInformation, 4096))

	if alloc := binary.LittleEndian.Uint64(info[:8]); alloc != 0 {
		t.Errorf("the directory takes up %d bytes, want none", alloc)
	}
	if size := binary.LittleEndian.Uint64(info[8:16]); size != 0 {
		t.Errorf("the directory holds %d bytes, want none", size)
	}
	if info[21] != 1 {
		t.Error("the directory is not marked as one")
	}
}

// TestIntegrationQueryStandardInformationOfAPipe is the named pipe, which is not a file in the
// store and does not answer as one: it reports a size of its own and a link, and stands as always
// pending deletion.
func TestIntegrationQueryStandardInformationOfAPipe(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	h.files.putPipe("srvsvc")
	buf, _ := cl.create("srvsvc", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create of the pipe was answered with %#x", status)
	}

	info := queriedInfo(t, cl.queryInfo(createdFileID(buf), smb2.FileStandardInformation, 4096))

	if alloc := binary.LittleEndian.Uint64(info[:8]); alloc != 4096 {
		t.Errorf("the pipe takes up %d bytes, want 4096", alloc)
	}
	if links := binary.LittleEndian.Uint32(info[16:20]); links != 1 {
		t.Errorf("the pipe has %d links, want one", links)
	}
	if info[20] != 1 {
		t.Error("the pipe is not marked as pending deletion")
	}
}

// TestIntegrationQueryNetworkOpenInformation is what a client asks before it decides to open a
// file: the times and the sizes, without the name or the handle.
func TestIntegrationQueryNetworkOpenInformation(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	fid := h.openedFile(cl, "report.txt", 1024)

	info := queriedInfo(t, cl.queryInfo(fid, smb2.FileNetworkOpenInformation, 4096))
	if len(info) != 56 {
		t.Fatalf("the structure is %d bytes long, want 56", len(info))
	}

	// The four times are the one time the store keeps, so they are all the same and none of them
	// is zero.
	created := binary.LittleEndian.Uint64(info[:8])
	if created == 0 {
		t.Error("the file has no creation time")
	}
	for i, at := range []int{8, 16, 24} {
		if got := binary.LittleEndian.Uint64(info[at : at+8]); got != created {
			t.Errorf("time %d differs from the creation time, want the one the store keeps", i)
		}
	}

	if alloc := binary.LittleEndian.Uint64(info[32:40]); alloc != 1024 {
		t.Errorf("the file takes up %d bytes, want 1024", alloc)
	}
	if size := binary.LittleEndian.Uint64(info[40:48]); size != 1024 {
		t.Errorf("the file holds %d bytes, want 1024", size)
	}
	if attr := binary.LittleEndian.Uint32(info[48:52]); attr != smb2.FILE_ATTRIBUTE_NORMAL {
		t.Errorf("the file is marked %#x, want an ordinary file", attr)
	}
}

// TestIntegrationQueryNetworkOpenInformationOfADirectory is the same question about a directory,
// which reports no size whatever the entry says.
func TestIntegrationQueryNetworkOpenInformationOfADirectory(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	fid := createdFileID(cl.openDir("docs"))
	info := queriedInfo(t, cl.queryInfo(fid, smb2.FileNetworkOpenInformation, 4096))

	if size := binary.LittleEndian.Uint64(info[40:48]); size != 0 {
		t.Errorf("the directory holds %d bytes, want none", size)
	}
	if attr := binary.LittleEndian.Uint32(info[48:52]); attr&smb2.FILE_ATTRIBUTE_DIRECTORY == 0 {
		t.Errorf("the directory is marked %#x, want it marked as one", attr)
	}
}

// TestIntegrationQueryNormalizedNameInformation is the whole path to the file, which is what the
// client asks for when the name it opened by was not the one the file is kept under.
func TestIntegrationQueryNormalizedNameInformation(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	fid := h.openedFile(cl, "docs/report.txt", 512)

	info := queriedInfo(t, cl.queryInfo(fid, smb2.FileNormalizedNameInformation, 4096))

	if name := countedName(t, info, 0); name != "docs/report.txt" {
		t.Errorf("the file is at %q, want %q", name, "docs/report.txt")
	}
}

// TestIntegrationQueryEaInformation is the extended attributes, which this server keeps none of.
// The answer is a size of zero rather than a refusal, because a client that asks is entitled to
// be told there are none.
func TestIntegrationQueryEaInformation(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	fid := h.openedFile(cl, "report.txt", 512)

	info := queriedInfo(t, cl.queryInfo(fid, smb2.FileEaInformation, 4096))
	if len(info) != 4 {
		t.Fatalf("the structure is %d bytes long, want 4", len(info))
	}
	if size := binary.LittleEndian.Uint32(info); size != 0 {
		t.Errorf("the file carries %d bytes of extended attributes, want none", size)
	}
}

// TestIntegrationQueryStreamInformation is the list of streams of the file. There is only ever
// the one the file itself is, named the way the unnamed data stream is named.
func TestIntegrationQueryStreamInformation(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	fid := h.openedFile(cl, "report.txt", 2048)

	info := queriedInfo(t, cl.queryInfo(fid, smb2.FileStreamInformation, 4096))

	if next := binary.LittleEndian.Uint32(info[:4]); next != 0 {
		t.Errorf("the entry points at another %d bytes on, want it to be the only one", next)
	}
	if size := binary.LittleEndian.Uint64(info[8:16]); size != 2048 {
		t.Errorf("the stream holds %d bytes, want 2048", size)
	}
	if alloc := binary.LittleEndian.Uint64(info[16:24]); alloc != 2048 {
		t.Errorf("the stream takes up %d bytes, want 2048", alloc)
	}

	length := int(binary.LittleEndian.Uint32(info[4:8]))
	if name := utils.DecodeToString(info[24 : 24+length]); name != "::$DATA" {
		t.Errorf("the stream is named %q, want %q", name, "::$DATA")
	}
}

// TestIntegrationQueryInfoRefusesAnUnsupportedClass is the class this server does not answer. The
// client is told so rather than handed an empty structure it would read as an answer.
func TestIntegrationQueryInfoRefusesAnUnsupportedClass(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	fid := h.openedFile(cl, "report.txt", 512)

	buf := cl.queryInfo(fid, smb2.FileBasicInformation, 4096)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_NOT_SUPPORTED {
		t.Fatalf("the query was answered with %#x, want it refused", status)
	}
}

// TestIntegrationQueryInfoRefusesABufferThatIsTooSmall is the client that asked for less room
// than the answer takes. Handing back as much as fits would be handing back a structure that
// stops in the middle of a field, so nothing is handed back at all.
func TestIntegrationQueryInfoRefusesABufferThatIsTooSmall(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	fid := h.openedFile(cl, "report.txt", 512)

	// The standard information is twenty-four bytes; there is no answer that fits in eight.
	buf := cl.queryInfo(fid, smb2.FileStandardInformation, 8)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_INFO_LENGTH_MISMATCH {
		t.Fatalf("the query was answered with %#x, want the length refused", status)
	}
}

// TestIntegrationQueryInfoRefusesAnUnknownHandle is the query against a file the session never
// opened.
func TestIntegrationQueryInfoRefusesAnUnknownHandle(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	buf := cl.queryInfo(bytes.Repeat([]byte{0xab}, 16), smb2.FileStandardInformation, 4096)
	if status := smb2.Header(buf).Status(); status == smb2.STATUS_OK {
		t.Fatal("the server answered a query against a handle nobody holds")
	}
}

// TestIntegrationQueryInfoReportsAPendingDeletion is the file that has been marked to go when the
// last handle on it closes. A client asks before it decides what to do with the file, so what it
// is told has to be where the file actually stands — read off the open when the question is put,
// not settled when the file was opened.
func TestIntegrationQueryInfoReportsAPendingDeletion(t *testing.T) {
	for _, tt := range []struct {
		name string
		// mark puts the file on its way out, and returns the handle to ask about.
		mark func(t *testing.T, h *smbTest, cl *testClient) []byte
	}{
		{
			name: "marked by a disposition",
			mark: func(t *testing.T, h *smbTest, cl *testClient) []byte {
				fid := h.openedFile(cl, "report.txt", 512)
				if _, err := cl.markForDeletion(fid); err != nil {
					t.Fatalf("could not mark the file for deletion: %v", err)
				}
				return fid
			},
		},
		{
			name: "marked by the create that opened it",
			mark: func(t *testing.T, h *smbTest, cl *testClient) []byte {
				h.files.put("report.txt", 512)
				cl.mid++
				msg := createRequestWithOptions(cl.mid, cl.ss.sessionID, cl.tc.treeID, "report.txt",
					smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, writeAccess, smb2.FILE_DELETE_ON_CLOSE, nil)
				resp, err := cl.send(msg)
				if err != nil {
					t.Fatalf("could not open the file: %v", err)
				}
				if status := resp.Header().Status(); status != smb2.STATUS_OK {
					t.Fatalf("the create was answered with %#x", status)
				}
				return createdFileID(resp.Encode())
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			cl := h.dial("alice")
			fid := tt.mark(t, h, cl)

			// The standard information carries the flag at 20, and the whole of the information
			// carries the same structure from 40, which puts it at 60.
			if info := queriedInfo(t, cl.queryInfo(fid, smb2.FileStandardInformation, 4096)); info[20] != 1 {
				t.Error("the standard information does not report the file as being on its way out")
			}
			if info := queriedInfo(t, cl.queryInfo(fid, smb2.FileAllInformation, 4096)); info[60] != 1 {
				t.Error("the whole of the information does not report the file as being on its way out")
			}
		})
	}
}

// TestIntegrationQueryInfoReportsADeletionCalledOff is the client that changed its mind. Nothing
// is deleted until the handle closes, so up to that point the file is staying, and what a query
// reports has to follow it back.
func TestIntegrationQueryInfoReportsADeletionCalledOff(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	fid := h.openedFile(cl, "report.txt", 512)

	if _, err := cl.markForDeletion(fid); err != nil {
		t.Fatalf("could not mark the file for deletion: %v", err)
	}
	if _, err := cl.keepFile(fid); err != nil {
		t.Fatalf("could not call the deletion off: %v", err)
	}

	if info := queriedInfo(t, cl.queryInfo(fid, smb2.FileStandardInformation, 4096)); info[20] != 0 {
		t.Error("the file is still reported as being on its way out")
	}
}

// TestASecurityDescriptorTooBigForTheBufferIsRefused is the query whose buffer will not hold the
// answer. A security descriptor is never sent in part, so the client is told how much room it needs
// and asks again — and [MS-SMB2] 3.3.5.20.3 names the one status this must not carry:
// STATUS_BUFFER_OVERFLOW is what says a truncated answer follows, and there is none.
func TestASecurityDescriptorTooBigForTheBufferIsRefused(t *testing.T) {
	for _, tt := range []struct {
		what    string
		dialect uint16
	}{
		{"3.1.1, which carries the size in an error context", smb2.SMB_DIALECT_311},
		{"3.0, which carries it on its own", smb2.SMB_DIALECT_30},
	} {
		t.Run(tt.what, func(t *testing.T) {
			h := newSMBTest(t)
			h.files.put("file", 1024)

			cl := h.dial("alice")
			cl.conn.negotiateDialect = tt.dialect
			cl.conn.dialect = dialectName(tt.dialect)

			created, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
			fid := createdFileID(created)

			// A buffer of one byte, which no descriptor fits in.
			cl.mid++
			resp, err := cl.send(queryInfoRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid,
				smb2.INFO_SECURITY, 0, 1))
			if err != nil {
				t.Fatalf("the query failed: %v", err)
			}

			status := resp.Header().Status()
			if status == smb2.STATUS_BUFFER_OVERFLOW {
				t.Fatal("the query was answered STATUS_BUFFER_OVERFLOW, which promises a truncated descriptor")
			}
			if status != smb2.STATUS_BUFFER_TOO_SMALL {
				t.Errorf("the query was answered %#x, want STATUS_BUFFER_TOO_SMALL", status)
			}
		})
	}
}
