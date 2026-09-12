//go:build linux || darwin

package main

import (
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
	"golang.org/x/sys/unix"
)

// slowTCPPair returns a loopback TCP connection whose client end has a small receive buffer, so that
// what the server sends backs up in the server's socket as it does on a busy link.
func slowTCPPair(t *testing.T) (server, client net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	d := net.Dialer{Control: func(_, _ string, raw syscall.RawConn) error {
		var serr error
		if err := raw.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 64<<10)
		}); err != nil {
			return err
		}
		return serr
	}}

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := ln.Accept()
		accepted <- conn
	}()

	client, err = d.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return <-accepted, client
}

func TestTuneConnectionSetsTheLowWater(t *testing.T) {
	server, _ := slowTCPPair(t)
	defer server.Close()

	if err := tuneConnection(server); err != nil {
		t.Fatalf("tuneConnection: %v", err)
	}

	raw, err := server.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var gerr error
	raw.Control(func(fd uintptr) {
		got, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT)
	})
	if gerr != nil || got != notSentLowWater {
		t.Errorf("TCP_NOTSENT_LOWAT is %d (%v), want %d", got, gerr, notSentLowWater)
	}
}

// TestSmallResponsesDoNotWaitInTheSocket checks a small response waits behind about one read, not
// behind every read the kernel was handed while the link was busy.
func TestSmallResponsesDoNotWaitInTheSocket(t *testing.T) {
	const reads = 8

	h := newSMBTest(t)
	h.files.putData("movie.mp4", make([]byte, 24<<20))

	server, wire := slowTCPPair(t)
	if err := tuneConnection(server); err != nil {
		t.Fatalf("tuneConnection: %v", err)
	}
	cl := h.dialOn("alice", server)
	sid, tid := cl.ss.sessionID, cl.tc.treeID

	// One read fills the cache, so the reads below are answered at once.
	cl.enqueue(createRequestSharing(1, sid, tid, "movie.mp4", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, readAccess, 0, everySharing, nil))
	fid := createdFileID(readWire(t, wire))
	cl.enqueue(readRequest(2, sid, tid, fid, 0, 64<<10))
	for smb2.Header(readWire(t, wire)).Status() == smb2.STATUS_PENDING {
	}

	// Let the server hand the kernel all it will take, then ask for an echo.
	for i := 0; i < reads; i++ {
		cl.enqueue(readRequest(uint64(3+i), sid, tid, fid, uint64(i)*smb2.MaxReadSize, smb2.MaxReadSize))
	}
	time.Sleep(300 * time.Millisecond)
	const echoMID = 3 + reads
	cl.enqueue(echoRequest(echoMID, sid, tid))
	time.Sleep(300 * time.Millisecond)

	var before int
	for {
		msg := readWire(t, wire)
		if smb2.Header(msg).MessageID() == echoMID {
			break
		}
		before += len(msg)
	}

	// The read on the wire, the low-water mark and the client's receive buffer: well under 2 MiB.
	// Without the low-water mark, three whole reads (3 MiB) sit in the socket ahead of the echo.
	t.Logf("%d KiB of reads came out before the echo", before>>10)
	if before > 2<<20 {
		t.Errorf("%d KiB of reads came out before the echo; want no more than about one read", before>>10)
	}
}
