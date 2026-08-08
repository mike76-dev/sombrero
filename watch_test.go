package main

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// A directory watch is a request the client is left waiting on: the server answers it when the
// directory changes, and until then the client counts it among the searches it has outstanding on the
// connection. So whatever ends the open behind it has to answer it - and has to stop it, or it goes
// on listing a directory nobody is waiting to hear about for as long as the server runs.

// changeNotifyRequest asks to be told when a directory changes.
func changeNotifyRequest(mid, sid uint64, tid uint32, fid []byte) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2ChangeNotifyRequestMinSize)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_CHANGE_NOTIFY)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2ChangeNotifyRequestStructureSize)
	binary.LittleEndian.PutUint32(body[4:8], 4096)
	copy(body[8:24], fid)
	binary.LittleEndian.PutUint32(body[24:28], smb2.FILE_NOTIFY_CHANGE_FILE_NAME|smb2.FILE_NOTIFY_CHANGE_SIZE)

	return msg
}

// watching arms a watch on the directory the handle is on, and returns the interim response the
// server holds it behind.
func (cl *testClient) watching(fid []byte) []byte {
	cl.h.t.Helper()

	cl.mid++
	resp, err := cl.send(changeNotifyRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid))
	if err != nil {
		cl.h.t.Fatalf("the watch failed outright: %v", err)
	}

	return resp.Encode()
}

// watchesOn is how many directory watches the connection is still holding.
func (cl *testClient) watchesOn() int {
	cl.conn.mu.Lock()
	defer cl.conn.mu.Unlock()

	var n int
	for _, r := range cl.conn.asyncCommandList {
		if r.Header().Command() == smb2.SMB2_CHANGE_NOTIFY {
			n++
		}
	}

	return n
}

// armedWatch opens a directory and arms a watch on it, checking that the server took it on.
func (h *smbTest) armedWatch(cl *testClient, dir string) []byte {
	h.t.Helper()

	h.files.putDir(dir)
	h.files.put(dir+"/file", 1024)

	handle := cl.openDir(dir)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		h.t.Fatalf("opening the directory was answered with %#x", status)
	}

	interim := cl.watching(createdFileID(handle))
	if status := smb2.Header(interim).Status(); status != smb2.STATUS_PENDING {
		h.t.Fatalf("the watch was answered with %#x, want it held open", status)
	}
	if got := cl.watchesOn(); got != 1 {
		h.t.Fatalf("the connection holds %d watches, want the one just armed", got)
	}

	return handle
}

// TestIntegrationDisconnectingTheTreeAnswersTheWatches is the client that would not let go of the
// share.
func TestIntegrationDisconnectingTheTreeAnswersTheWatches(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	h.armedWatch(cl, "dir")

	if err := cl.ss.closeTreeConnect(cl.tc.treeID); err != nil {
		t.Fatalf("disconnecting the tree failed: %v", err)
	}

	// The watch is answered, and with the status that says the open it was on is gone.
	answer := cl.recv(2 * time.Second)
	if answer == nil {
		t.Fatal("nothing came back for the watch when the tree was disconnected")
	}
	if status := smb2.Header(answer).Status(); status != smb2.STATUS_NOTIFY_CLEANUP {
		t.Errorf("the watch was answered with %#x, want the open it was on reported gone", status)
	}
	if got := cl.watchesOn(); got != 0 {
		t.Errorf("the connection still holds %d watch(es) after the tree was disconnected", got)
	}
}

// TestIntegrationClosingTheDirectoryStopsTheWatch is the same watch left running by a close.
func TestIntegrationClosingTheDirectoryStopsTheWatch(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	handle := h.armedWatch(cl, "dir")

	watched := h.srv.globalOpenTable[openIDOf(createdFileID(handle))]
	if watched == nil {
		t.Fatal("the directory has no open behind it")
	}

	if _, err := cl.closeHandle(createdFileID(handle)); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	answer := cl.recv(2 * time.Second)
	if answer == nil {
		t.Fatal("nothing came back for the watch when the directory was closed")
	}
	if status := smb2.Header(answer).Status(); status != smb2.STATUS_NOTIFY_CLEANUP {
		t.Errorf("the watch was answered with %#x, want the open it was on reported gone", status)
	}

	// And the watch is over: the open it was armed on is done with, which is what the goroutine
	// behind it waits on now.
	select {
	case <-watched.ctx.Done():
	default:
		t.Error("the open the watch was armed on is still live after the close")
	}
	if got := cl.watchesOn(); got != 0 {
		t.Errorf("the connection still holds %d watch(es) after the directory was closed", got)
	}
}

// TestIntegrationAWatchStopsWithTheDirectoryItWatched is the watch that outlived its open.
func TestIntegrationAWatchStopsWithTheDirectoryItWatched(t *testing.T) {
	h := newSMBTest(t)
	h.srv.watchInterval = 10 * time.Millisecond

	cl := h.dial("alice")
	handle := h.armedWatch(cl, "dir")

	if _, err := cl.closeHandle(createdFileID(handle)); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	// The close answers the watch, which is the one message expected here.
	answer := cl.recv(2 * time.Second)
	if status := smb2.Header(answer).Status(); status != smb2.STATUS_NOTIFY_CLEANUP {
		t.Fatalf("the watch was answered with %#x, want the open it was on reported gone", status)
	}

	// The directory changes, several times over what the watch used to look at it. A watch that is
	// still running answers again, on a request that is over.
	h.files.put("dir/another", 2048)

	cl.quiet(200*time.Millisecond, "an answer to a watch whose directory had been closed")
}
