package main

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// window returns the message IDs the connection will accept, in order.
func (c *connection) window() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	ids := make([]uint64, 0, len(c.commandSequenceWindow))
	for id := range c.commandSequenceWindow {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	return ids
}

// cancelRequestFor builds the bytes of an SMB2_CANCEL request against a message ID. A cancel
// carries no body beyond its structure size: what it refers to is in the header.
func cancelRequestFor(mid, sid uint64, tid uint32) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2CancelRequestMinSize)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_CANCEL)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	body[0] = smb2.SMB2CancelRequestStructureSize

	return msg
}

// asyncCancelRequest is cancelRequestFor against a request the server answered asynchronously,
// which is referred to by the async ID it was given rather than by its message ID.
func asyncCancelRequest(aid, sid uint64, tid uint32) []byte {
	msg := cancelRequestFor(0, sid, tid)
	smb2.Header(msg).SetFlag(smb2.FLAGS_ASYNC_COMMAND)
	smb2.Header(msg).SetAsyncID(aid)

	return msg
}

// waiting puts a request on the connection in the state the server leaves one in when it has
// answered with an interim response and gone off to do the work: on the async command list, under
// an async ID, with a channel to stop it by. It returns the request and that channel.
func (cl *testClient) waiting(mid, aid uint64, command uint16) (*smb2.Request, chan struct{}) {
	cl.h.t.Helper()

	msg := echoRequest(mid, cl.ss.sessionID, cl.tc.treeID)
	smb2.Header(msg).SetCommand(command)
	smb2.Header(msg).SetFlag(smb2.FLAGS_ASYNC_COMMAND)
	smb2.Header(msg).SetAsyncID(aid)

	req := request(cl.h.t, msg)
	stop := make(chan struct{})

	cl.conn.mu.Lock()
	cl.conn.asyncCommandList[aid] = req
	cl.conn.stopChans[req.CancelRequestID()] = stop
	cl.conn.mu.Unlock()

	return req, stop
}

// cancel hands a cancel to the server. A cancel never goes through the dispatcher the way every
// other request does: it is picked out of the queue before that, because it is about a request
// already in hand rather than something to be worked through on its own.
func (cl *testClient) cancel(msg []byte) error {
	cl.h.t.Helper()

	return cl.conn.cancelRequest(request(cl.h.t, msg))
}

// cancelled reports whether the stop channel of a request has been closed, which is how the
// goroutine doing the work is told to give up.
func cancelled(stop chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// TestGrantCredits is the window of message IDs moving on. A client may only send under an ID the
// server has handed out, so granting credits is what lets it send at all.
func TestGrantCredits(t *testing.T) {
	h := newSMBTest(t)
	c := h.newTestConnection("credits")

	// A connection starts off able to take the one request that opens the window.
	if got := c.window(); !slices.Equal(got, []uint64{0}) {
		t.Fatalf("a new connection accepts %v, want only the request that opens the window", got)
	}

	if granted, err := c.grantCredits(0, 3, 1); err != nil {
		t.Fatalf("the server would not grant credits: %v", err)
	} else if granted != 3 {
		t.Fatalf("the server granted %d credits of the three asked for", granted)
	}
	if got := c.window(); !slices.Equal(got, []uint64{0, 1, 2, 3}) {
		t.Fatalf("the connection accepts %v, want three more IDs on top of the first", got)
	}

	// The next grant carries on from the highest ID handed out rather than from the one the
	// request came in under.
	if _, err := c.grantCredits(1, 2, 1); err != nil {
		t.Fatalf("the server would not grant credits: %v", err)
	}
	if got := c.window(); !slices.Equal(got, []uint64{0, 1, 2, 3, 4, 5}) {
		t.Fatalf("the connection accepts %v, want the window carried on from where it stood", got)
	}
}

// TestGrantCreditsAlwaysGrantsOne is the client that asked for nothing. A client that is granted
// nothing can never send again, so a request is always worth at least one credit.
func TestGrantCreditsAlwaysGrantsOne(t *testing.T) {
	h := newSMBTest(t)
	c := h.newTestConnection("credits")

	if _, err := c.grantCredits(0, 0, 1); err != nil {
		t.Fatalf("the server would not grant credits: %v", err)
	}
	if got := c.window(); !slices.Equal(got, []uint64{0, 1}) {
		t.Fatalf("the connection accepts %v, want one credit granted anyway", got)
	}
}

// TestGrantCreditsFromAnEmptyWindow is the window with nothing left in it. The IDs carry on from
// the request that was just taken, because there is nothing else left to say where the window
// stands.
func TestGrantCreditsFromAnEmptyWindow(t *testing.T) {
	h := newSMBTest(t)
	c := h.newTestConnection("credits")

	c.mu.Lock()
	clear(c.commandSequenceWindow)
	c.mu.Unlock()

	if _, err := c.grantCredits(9, 2, 1); err != nil {
		t.Fatalf("the server would not grant credits: %v", err)
	}
	if got := c.window(); !slices.Equal(got, []uint64{10, 11}) {
		t.Fatalf("the connection accepts %v, want the window carried on from the request it took", got)
	}
}

// TestGrantCreditsRefusesToRunPastTheEnd is the window that cannot move any further. The IDs are
// counted in a fixed width, and one that wrapped round would name a request that has already been
// sent, so the connection is given up on instead.
func TestGrantCreditsRefusesToRunPastTheEnd(t *testing.T) {
	h := newSMBTest(t)
	c := h.newTestConnection("credits")

	c.mu.Lock()
	clear(c.commandSequenceWindow)
	c.commandSequenceWindow[^uint64(0)-1] = struct{}{}
	c.mu.Unlock()

	if _, err := c.grantCredits(0, 4, 1); !errors.Is(err, errCommandSecuenceWindowExceeded) {
		t.Fatalf("the server answered %v, want it to refuse to run past the end of the window", err)
	}
}

// TestCancelAnswersTheRequestItStops is the client giving up on something it asked
// for. What it gets back is an answer to the request it cancelled, under that request's own
// message ID, because that is the one it is waiting on.
func TestCancelAnswersTheRequestItStops(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	target, stop := cl.waiting(7, 42, smb2.SMB2_CREATE)

	if err := cl.cancel(asyncCancelRequest(42, cl.ss.sessionID, cl.tc.treeID)); err != nil {
		t.Fatalf("the server gave up on the cancel: %v", err)
	}

	buf := cl.recv(20 * time.Second)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_CANCELLED {
		t.Fatalf("the request was answered with %#x, want it cancelled", status)
	}
	if mid := smb2.Header(buf).MessageID(); mid != target.Header().MessageID() {
		t.Fatalf("the answer carries message ID %d, want that of the request it stops", mid)
	}
	if aid := smb2.Header(buf).AsyncID(); aid != 42 {
		t.Fatalf("the answer carries async ID %d, want that of the request it stops", aid)
	}
	if !smb2.Header(buf).IsFlagSet(smb2.FLAGS_ASYNC_COMMAND) {
		t.Error("the answer to an asynchronous request does not say it is one")
	}

	// The work is told to stop, and nothing is left behind that a second cancel could find.
	if !cancelled(stop) {
		t.Error("the work behind the request was not told to stop")
	}

	cl.conn.mu.Lock()
	_, listed := cl.conn.asyncCommandList[42]
	_, kept := cl.conn.stopChans[target.CancelRequestID()]
	cl.conn.mu.Unlock()

	if listed {
		t.Error("the cancelled request is still on the async command list")
	}
	if kept {
		t.Error("the stop channel of the cancelled request was kept")
	}
}

// TestCancelOfSomethingThatIsNotThere is the cancel that names nothing the server is
// working on — the answer arrived first, or the client made the ID up. A cancel is never
// answered, so there is nothing to send back and nothing to go wrong.
func TestCancelOfSomethingThatIsNotThere(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	if err := cl.cancel(asyncCancelRequest(99, cl.ss.sessionID, cl.tc.treeID)); err != nil {
		t.Fatalf("the server gave up on a cancel that names nothing: %v", err)
	}

	cl.quiet(100*time.Millisecond, "the server answered a cancel that names nothing")
}

// TestCancelFindsASynchronousRequestByItsMessageID is the request the server never answered
// asynchronously. It has no async ID to be named by, so the cancel names it by its message ID,
// and that only means anything on the connection it was sent on.
func TestCancelFindsASynchronousRequestByItsMessageID(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	target, _ := cl.waiting(7, 42, smb2.SMB2_CREATE)
	smb2.Header(target.Header()).ClearFlag(smb2.FLAGS_ASYNC_COMMAND)

	cr := smb2.CancelRequest{Request: *request(t, cancelRequestFor(7, cl.ss.sessionID, cl.tc.treeID))}

	found, owner := cl.conn.findCancelTarget(cr, cl.ss)
	if found != target {
		t.Fatal("the cancel did not find the request it names")
	}
	if owner != cl.conn {
		t.Fatal("the request was found on a connection other than the one it was sent on")
	}

	// A message ID means nothing outside its own connection, so the search never leaves it.
	other := cl.addChannel()
	if found, _ := other.conn.findCancelTarget(cr, cl.ss); found != nil {
		t.Fatal("a synchronous request was found from another connection")
	}
}

// TestCancelFindsAnAsynchronousRequestOverAnotherChannel is the client cancelling over a channel
// other than the one it asked on. An async ID means the same thing everywhere on the server, so
// every channel of the session is searched.
func TestCancelFindsAnAsynchronousRequestOverAnotherChannel(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	alt := cl.addChannel()

	target, _ := cl.waiting(7, 42, smb2.SMB2_CREATE)

	cr := smb2.CancelRequest{Request: *request(t, asyncCancelRequest(42, cl.ss.sessionID, cl.tc.treeID))}

	found, owner := alt.conn.findCancelTarget(cr, cl.ss)
	if found != target {
		t.Fatal("the cancel did not find the request it names over another channel")
	}
	if owner != cl.conn {
		t.Fatal("the request was not found on the connection that carries it")
	}
}

// TestCancelOverAnotherChannelAnswersOverTheFirst is the same cancel carried through.
// The request is answered on the connection that carries it, which is the one the client is
// waiting on, rather than on the one the cancel happened to arrive by.
func TestCancelOverAnotherChannelAnswersOverTheFirst(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	alt := cl.addChannel()

	_, stop := cl.waiting(7, 42, smb2.SMB2_CREATE)

	if err := alt.cancel(asyncCancelRequest(42, cl.ss.sessionID, cl.tc.treeID)); err != nil {
		t.Fatalf("the server gave up on the cancel: %v", err)
	}

	buf := cl.recv(20 * time.Second)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_CANCELLED {
		t.Fatalf("the request was answered with %#x, want it cancelled", status)
	}
	if !cancelled(stop) {
		t.Error("the work behind the request was not told to stop")
	}

	alt.quiet(100*time.Millisecond, "the answer went out over the connection the cancel arrived on")
}

// TestCancelRefusesASignedRequestItCannotVerify is the cancel that claims to be signed and is
// not. A cancel is never answered, so a signature that does not check out can only be dropped —
// but it must not stop anything on the way.
func TestCancelRefusesASignedRequestItCannotVerify(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	cl.keyed(0x11)

	_, stop := cl.waiting(7, 42, smb2.SMB2_CREATE)

	msg := asyncCancelRequest(42, cl.ss.sessionID, cl.tc.treeID)
	signed(t, msg, []byte("0123456789abcdef"), smb2.SMB_DIALECT_311, 0)

	if err := cl.conn.cancelRequest(request(t, msg)); !errors.Is(err, errInvalidSignature) {
		t.Fatalf("the server answered %v, want it to refuse a cancel it cannot verify", err)
	}
	if cancelled(stop) {
		t.Error("a cancel that could not be verified stopped the request all the same")
	}
}

// TestCancelRefusesASignedRequestWithoutASession is the signed cancel naming a session that is
// not on the connection. There is no key to check it with, so it goes no further.
func TestCancelRefusesASignedRequestWithoutASession(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()

	_, stop := cl.waiting(7, 42, smb2.SMB2_CREATE)

	msg := asyncCancelRequest(42, cl.ss.sessionID+1000, cl.tc.treeID)
	signed(t, msg, cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)

	if err := cl.conn.cancelRequest(request(t, msg)); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("the server answered %v, want it to refuse a cancel with no session behind it", err)
	}
	if cancelled(stop) {
		t.Error("a cancel with no session behind it stopped the request all the same")
	}
}

// TestFindOpenByGroupID is the open behind a response the server is holding on to, which is what
// the credit accounting of a compound request is settled against.
func TestFindOpenByGroupID(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	h.files.put("report.txt", 512)
	buf, _ := cl.create("report.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	fid := createdFileID(buf)

	// A pending response holds the ID of the open it is about, which is how the open is reached
	// once the request itself is gone.
	resp := smb2.NewErrorResponse(request(t, echoRequest(1, cl.ss.sessionID, cl.tc.treeID)), smb2.STATUS_OK, 0, nil)
	resp.SetOpenID(fid)

	cl.conn.mu.Lock()
	cl.conn.pendingResponses[3] = resp
	cl.conn.mu.Unlock()

	op := cl.conn.findOpenByGroupID(3)
	if op == nil {
		t.Fatal("the open behind the response was not found")
	}
	if op.durableFileID != openIDOf(fid) {
		t.Fatal("the open found is not the one the response is about")
	}
}

// TestFindOpenByGroupIDWithoutAnOpen is the group that names no response, and the response that
// names no open. Neither has anything behind it.
func TestFindOpenByGroupIDWithoutAnOpen(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	if op := cl.conn.findOpenByGroupID(99); op != nil {
		t.Error("an open was found behind a group nobody is holding a response for")
	}

	resp := smb2.NewErrorResponse(request(t, echoRequest(1, cl.ss.sessionID, cl.tc.treeID)), smb2.STATUS_OK, 0, nil)

	cl.conn.mu.Lock()
	cl.conn.pendingResponses[3] = resp
	cl.conn.mu.Unlock()

	if op := cl.conn.findOpenByGroupID(3); op != nil {
		t.Error("an open was found behind a response that is about no open")
	}
}

// TestIsStale walks the states a connection may be in when the server looks over its connections
// for ones worth keeping. A connection is worth keeping while anybody is using it.
func TestIsStale(t *testing.T) {
	long := staleThreshold + time.Minute

	for _, tt := range []struct {
		name string
		// setUp puts the connection in the state to be judged.
		setUp func(h *smbTest, c *connection)
		want  bool
	}{
		{
			name: "nobody has ever used it",
			setUp: func(h *smbTest, c *connection) {
				c.creationTime = time.Now().Add(-long)
			},
			want: true,
		},
		{
			name:  "nobody has used it yet, and it has only just arrived",
			setUp: func(h *smbTest, c *connection) {},
			want:  false,
		},
		{
			name: "the session on it went quiet long ago",
			setUp: func(h *smbTest, c *connection) {
				ss := newSessionState(1, c)
				ss.idleTime = time.Now().Add(-long)
				c.sessionTable[1] = ss
			},
			want: true,
		},
		{
			name: "the session on it is being used",
			setUp: func(h *smbTest, c *connection) {
				ss := newSessionState(1, c)
				ss.idleTime = time.Now()
				c.sessionTable[1] = ss
			},
			want: false,
		},
		{
			name: "one of the sessions on it is being used",
			setUp: func(h *smbTest, c *connection) {
				quiet := newSessionState(1, c)
				quiet.idleTime = time.Now().Add(-long)
				c.sessionTable[1] = quiet

				busy := newSessionState(2, c)
				busy.idleTime = time.Now()
				c.sessionTable[2] = busy
			},
			want: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			c := h.newTestConnection("stale")
			tt.setUp(h, c)

			if got := c.isStale(); got != tt.want {
				t.Fatalf("the connection is judged stale = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIntegrationACancelledCreateIsNotAnsweredTwice is a create the client gives up on while it
// waits for somebody else's oplock to be broken. The cancel answers it, so the work behind it must
// not answer it again: two responses to the one message ID is not a protocol a client can follow,
// and it drops the connection rather than try.
func TestIntegrationACancelledCreateIsNotAnsweredTwice(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)
	timeout := h.impatient(300 * time.Millisecond)

	// Alice holds the file, so bob's create has to wait for her to give it up. She never answers,
	// which is what leaves the create outstanding long enough to be cancelled.
	alice := h.dial("alice")
	if _, async := alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN); async {
		t.Fatal("alice's own create had to wait for something")
	}

	bob := h.dial("bob")
	bob.mid++
	interim, err := bob.send(createRequest(bob.mid, bob.ss.sessionID, bob.tc.treeID, "dir/file",
		smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, writeAccess, nil))
	if err != nil {
		t.Fatalf("the server gave up on the create: %v", err)
	}
	if status := interim.Header().Status(); status != smb2.STATUS_PENDING {
		t.Fatalf("the create was answered %#x, want it held open behind the break", status)
	}

	// Bob gives up on it.
	if err := bob.cancel(asyncCancelRequest(interim.Header().AsyncID(), bob.ss.sessionID, bob.tc.treeID)); err != nil {
		t.Fatalf("the server gave up on the cancel: %v", err)
	}

	answer := bob.recv(5 * time.Second)
	if status := smb2.Header(answer).Status(); status != smb2.STATUS_CANCELLED {
		t.Fatalf("the create was answered %#x, want it cancelled", status)
	}

	// The break runs out while nobody is waiting on it any more, and the work finishes. What it
	// worked out has nowhere to go: the request it answers has been answered.
	bob.quiet(4*timeout, "the cancelled create was answered a second time")
}
