package main

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// A client may have as many requests outstanding as it has credits, and it is answered with more on
// every response. What it is given is therefore how fast it is allowed to go, and getting it wrong
// is not a slow client but a stopped one.

// TestCreditsToGrantPacesTheClient is the pacing itself. A client may only have as many requests
// outstanding as it has credits, so what it is answered with is what decides how much it sends at a
// time — and unlike holding the write, it costs the client no waiting at all.
func TestCreditsToGrantPacesTheClient(t *testing.T) {
	// What the pipeline holds on a renterd share: sixteen parts of a sector apiece.
	const budget = 16 * 4 * 1024 * 1024
	if got := pacingCapacity(4 << 20); got != budget {
		t.Fatalf("a renterd pipeline holds %s, and this test is written for %s",
			traceBytes(got), traceBytes(budget))
	}

	for _, c := range []struct {
		what    string
		charge  uint16
		request uint16
		waiting uint64
		want    uint16
	}{
		// Nothing much on its way: the client is given what it asked for and may grow its window.
		{what: "an idle backend", charge: 16, request: 32, waiting: 0, want: 32},
		{what: "a backend keeping up", charge: 16, request: 32, waiting: budget / 4, want: 32},

		// Halfway through the budget: the client keeps the window it has and grows it no further.
		{what: "a backend falling behind", charge: 16, request: 32, waiting: budget / 2, want: 16},
		{what: "one falling further", charge: 16, request: 64, waiting: budget - 1, want: 16},

		// Over it: a request at a time until the backend has caught up, and never nothing at all.
		{what: "a backend that is behind", charge: 16, request: 32, waiting: budget, want: 1},
		{what: "one far behind", charge: 16, request: 32, waiting: 4 * budget, want: 1},

		// A charge of zero is a charge of one, which is the least a request can cost.
		{what: "a request charging nothing", charge: 0, request: 0, waiting: 0, want: 1},
	} {
		got := creditsToGrant(c.charge, c.request, c.waiting, budget)
		if got != c.want {
			t.Errorf("%s: a write charging %d and asking for %d with %s on its way is granted %d, want %d",
				c.what, c.charge, c.request, traceBytes(c.waiting), got, c.want)
		}
		if got == 0 {
			t.Errorf("%s: granted nothing, which leaves the client unable to send anything at all", c.what)
		}
	}
}

// TestIntegrationABackedUpBackendPacesTheWriter is the pacing on the wire. The write is answered at
// once either way — that is the point of pacing with credits rather than with waiting — and what
// changes is how much the client is given to send with.
func TestIntegrationABackedUpBackendPacesTheWriter(t *testing.T) {
	h := newSMBTest(t)

	// Parts small enough that a test can fill the pipeline they make up.
	cl := h.dial("alice")
	cl.tc.maxUploadSize = 1024
	capacity := pacingCapacity(cl.tc.maxUploadSize)

	handle, _ := cl.create("big.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	fid := createdFileID(handle)

	// Nothing reaches the store, so everything sent stays on its way to it.
	release := h.files.holdParts(0)
	defer release()

	// The first write finds an idle backend and is given what it asked for.
	first := creditsOnInterim(t, cl, fid, 0, 1024)
	if first <= 1 {
		t.Errorf("the first write was granted %d credit(s), want what it asked for", first)
	}

	// Past half of what the pipeline holds, the client is given what the write cost and no more, so
	// its window stops growing. Filling the pipeline outright is the business of the unit test: the
	// write that would overrun it waits for a slot, which is the last resort rather than the pacing.
	var last uint16
	for i := 1; i <= int(capacity/1024)/2+1; i++ {
		last = creditsOnInterim(t, cl, fid, uint64(i)*1024, 1024)
	}

	waiting := h.srv.globalOpenTable[openIDOf(fid)].file.waitingOnTheBackend()
	if last != 1 {
		t.Errorf("with %s of the %s the pipeline holds waiting on the store, the write was granted %d credit(s), want the 1 it spent",
			traceBytes(waiting), traceBytes(capacity), last)
	}
}

// creditsOnInterim writes through the handle and returns the credits the interim response granted,
// which is where a write's credits go back.
func creditsOnInterim(t *testing.T, cl *testClient, fid []byte, offset uint64, length int) uint16 {
	t.Helper()

	cl.mid++
	msg := writeRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, offset,
		bytes.Repeat([]byte("s"), length))
	smb2.Header(msg).SetCreditCharge(1)
	binary.LittleEndian.PutUint16(msg[14:16], 32) // the credits the client asks for

	resp, err := cl.send(msg)
	if err != nil {
		t.Fatalf("the write at %d failed: %v", offset, err)
	}
	if status := resp.Header().Status(); status != smb2.STATUS_PENDING {
		t.Fatalf("the write at %d was answered with %#x, want it taken on", offset, status)
	}

	granted := resp.Header().CreditRequest() // the credit field of a response
	cl.recv(5 * time.Second)                 // the final response, so the next write starts clean

	return granted
}

// TestTheSequenceWindowStaysBounded is the window that grew all transfer long. A client asks for
// credits on every request — a macOS client asks for 256 apiece — and spends a fraction of what it asks
// for. Granting every request in full opened a couple of hundred IDs per request and gave none of them
// back: sixty-six thousand of them within one file, bounded by nothing but the connection's life.
func TestTheSequenceWindowStaysBounded(t *testing.T) {
	h := newSMBTest(t)
	c := h.newTestConnection("mac")

	// A client that asks for 256 credits on every request and spends one, which is what the trace of a
	// real transfer shows.
	var last uint16
	for i := range 200 {
		granted, err := c.grantCredits(uint64(i), 256, 1)
		if err != nil {
			t.Fatalf("the server would not grant credits on request %d: %v", i, err)
		}
		if granted == 0 {
			t.Fatalf("request %d was granted nothing, which leaves the client unable to send", i)
		}
		last = granted

		// The one credit the request spent goes back.
		c.mu.Lock()
		delete(c.commandSequenceWindow, uint64(i))
		c.mu.Unlock()
	}

	c.mu.Lock()
	window := len(c.commandSequenceWindow)
	c.mu.Unlock()

	if window > maxSequenceWindow {
		t.Errorf("the connection holds %d IDs open, over the %d it may", window, maxSequenceWindow)
	}
	if last == 0 {
		t.Error("the last request was granted nothing at all")
	}
}

// TestWhatAResponseGrantsIsWhatTheWindowOpenedBy is the agreement the two sides keep. Told more credits
// than the window was opened by, the client sends beyond the window and the message it sends is one the
// server has to turn away; told fewer, it holds back for credits it has already been given.
func TestWhatAResponseGrantsIsWhatTheWindowOpenedBy(t *testing.T) {
	h := newSMBTest(t)
	c := h.newTestConnection("mac")

	// A request granted less than it asked for, because the window had no more room.
	c.mu.Lock()
	c.creditsGranted[7] = 4
	c.mu.Unlock()

	resp := smb2.NewErrorResponse(request(t, readRequest(7, 1, 1, make([]byte, 16), 0, 12)),
		smb2.STATUS_PENDING, 0, nil)
	resp.Header().SetCreditResponse(256) // what the response meant to grant

	c.grantOnResponse(resp)

	if got := resp.Header().CreditRequest(); got != 4 {
		t.Errorf("the response grants %d credits, want the 4 the window was opened by", got)
	}

	// The grant is taken by the first response to carry it, so the final response of the same request
	// grants nothing.
	final := smb2.NewErrorResponse(request(t, readRequest(7, 1, 1, make([]byte, 16), 0, 12)),
		smb2.STATUS_OK, 0, nil)
	final.Header().SetCreditResponse(0)
	c.grantOnResponse(final)

	if got := final.Header().CreditRequest(); got != 0 {
		t.Errorf("the final response grants %d credits, want none: they went back with the interim", got)
	}

	c.mu.Lock()
	remembered := len(c.creditsGranted)
	c.mu.Unlock()
	if remembered != 0 {
		t.Errorf("the connection still remembers %d grant(s) of requests it has answered", remembered)
	}
}

// TestEveryResponseOfACompoundIsReconciled is the hole a chain opens. A compound request is several
// requests in one message and is answered with one message carrying a header apiece — but only the
// header the message begins with used to have its credits settled against the window, because that is
// the one the send path sees. The rest went out granting whatever they meant to, which on a window
// that has no room left is more than the client may have: it is told it can send further than this
// server will accept, and the message it sends on the strength of that is turned away.
func TestEveryResponseOfACompoundIsReconciled(t *testing.T) {
	h := newSMBTest(t)
	c := h.newTestConnection("mac")

	// Three requests of a compound, each granted little because the window had no room.
	for mid := uint64(10); mid < 13; mid++ {
		c.mu.Lock()
		c.creditsGranted[mid] = 2
		c.mu.Unlock()
	}

	for mid := uint64(10); mid < 13; mid++ {
		resp := smb2.NewErrorResponse(request(t, readRequest(mid, 1, 1, make([]byte, 16), 0, 12)),
			smb2.STATUS_OK, 0, nil)
		resp.Header().SetCreditResponse(256) // what each response meant to grant

		c.grantOnResponse(resp)

		if got := resp.Header().CreditRequest(); got != 2 {
			t.Errorf("the response to request %d grants %d credits, want the 2 the window was opened by",
				mid, got)
		}
	}

	c.mu.Lock()
	remembered := len(c.creditsGranted)
	c.mu.Unlock()
	if remembered != 0 {
		t.Errorf("the connection still remembers %d grant(s) after answering all of them", remembered)
	}
}

// TestAFullWindowStillGivesBackWhatWasSpent is the starvation a bounded window can cause. A request
// spends its charge out of the window and is answered with what is granted back, so a full window
// that grants only the room it has left — which is none — answers every request with one credit. The
// client's credits fall away, it has fewer requests it may keep outstanding, and it ends up sending
// one at a time before giving up on what it was doing. What a full window means is that the client
// keeps what it holds, not that it loses it.
func TestAFullWindowStillGivesBackWhatWasSpent(t *testing.T) {
	h := newSMBTest(t)
	c := h.newTestConnection("mac")

	// Fill the window, as a client asking for more on every request does. Every request also spends
	// its charge, which the accepting path retires from the window, so that is done here too.
	for i := range 2000 {
		if _, err := c.grantCredits(uint64(i), 256, 1); err != nil {
			t.Fatalf("granting credits failed: %v", err)
		}

		c.mu.Lock()
		delete(c.commandSequenceWindow, uint64(i))
		c.mu.Unlock()
	}

	c.mu.Lock()
	full := len(c.commandSequenceWindow)
	c.mu.Unlock()
	if full < maxSequenceWindow/2 {
		t.Fatalf("the window holds %d IDs, which is not full enough to test what a full one does", full)
	}

	// A write charging sixteen, on a window with no room left, is still given its sixteen back.
	granted, err := c.grantCredits(9999, 256, 16)
	if err != nil {
		t.Fatalf("granting credits failed: %v", err)
	}
	if granted < 16 {
		t.Errorf("a request charging 16 was granted %d credits on a full window, want at least what it spent", granted)
	}

	// And the window does not run away for it: at the limit, what is granted is what was spent, so a
	// client that keeps asking keeps what it holds and no more.
	c.mu.Lock()
	after := len(c.commandSequenceWindow)
	c.mu.Unlock()
	if after > maxSequenceWindow+int(granted) {
		t.Errorf("the window holds %d IDs, over the %d it may", after, maxSequenceWindow)
	}
}

// TestPacingScalesWithThePartSize is why the measure is the pipeline and not a number of bytes. A
// part is a sector on renterd and a whole slab on indexd, so a figure that leaves room on the one is
// a figure three parts of the other overrun: a client writing to indexd would be cut back to a
// request at a time from the third part onwards, for no reason but the arithmetic.
func TestPacingScalesWithThePartSize(t *testing.T) {
	renterd := pacingCapacity(4 << 20) // a sector apiece
	indexd := pacingCapacity(40 << 20) // ten shards of a sector

	if renterd == 0 || indexd == 0 {
		t.Fatal("a pipeline that holds nothing paces everything")
	}

	// A slab on its way to indexd is a quarter of what that pipeline holds, and holds nobody back.
	if got := creditsToGrant(16, 256, 40<<20, indexd); got != 256 {
		t.Errorf("one slab on its way had the client granted %d credit(s) of the 256 asked for", got)
	}

	// Two of them is half of it, which stops the window growing without cutting it back.
	if got := creditsToGrant(16, 256, 2*(40<<20), indexd); got != 16 {
		t.Errorf("two slabs on their way had the client granted %d credit(s), want the 16 it spent", got)
	}

	// And the same amount on renterd, where it is the whole pipeline over, holds it right back.
	if got := creditsToGrant(16, 256, 2*(40<<20), renterd); got != 1 {
		t.Errorf("%s on their way to a pipeline of %s had the client granted %d credit(s), want one",
			traceBytes(2*(40<<20)), traceBytes(renterd), got)
	}

	// A backend whose part size is unknown paces nobody.
	if got := creditsToGrant(16, 256, 1<<30, 0); got != 256 {
		t.Errorf("with no pipeline to measure against the client was granted %d credit(s), want what it asked for", got)
	}
}
