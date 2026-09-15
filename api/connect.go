package api

import (
	"context"
	"encoding/hex"
	"sync"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/core/types"
)

// ConnectState is the phase a connection attempt is in.
type ConnectState string

const (
	// ConnectIdle is reported for a workgroup and share with neither a running
	// attempt nor a connection, and ConnectConnected for one that is connected
	// with nothing in flight.
	ConnectIdle      ConnectState = "idle"
	ConnectConnected ConnectState = "connected"

	// The phases of an attempt in flight: waiting for the admin to approve the
	// request with the indexer, deriving and registering the app key, and
	// bringing up the client, which warms up a connection to every host.
	ConnectAwaitingApproval ConnectState = "awaiting-approval"
	ConnectRegistering      ConnectState = "registering"
	ConnectConnecting       ConnectState = "connecting"

	// ConnectFailed is where an attempt ends that could not be completed.
	ConnectFailed ConnectState = "failed"
)

// connectApprovalTimeout bounds the wait for the admin to approve a connection
// request, and connectRetention is how long a finished attempt is kept for its
// outcome to be read.
const (
	connectApprovalTimeout = 10 * time.Minute
	connectRetention       = 10 * time.Minute
)

// connectAttempt is the progress of one workgroup's connection to one share. It
// outlives the request that starts it: the work runs in the background, and what
// follows it is GET /connect/:workgroup/:share.
type connectAttempt struct {
	mu      sync.Mutex
	state   ConnectState
	started time.Time
	since   time.Time
	url     string
	appKey  string
	err     string
}

// requested records the URL the admin has to visit to approve the attempt.
func (a *connectAttempt) requested(url string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.url = url
	a.since = time.Now()
}

// advance moves the attempt on to the given phase.
func (a *connectAttempt) advance(state ConnectState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = state
	a.since = time.Now()
}

// finish ends the attempt as connected. A non-empty app key is one the caller
// does not have yet, and is held for the single status read that hands it over.
func (a *connectAttempt) finish(appKey types.PrivateKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = ConnectConnected
	a.since = time.Now()
	a.url = ""
	if len(appKey) > 0 {
		a.appKey = hex.EncodeToString(appKey)
	}
}

// fail ends the attempt with the reason it could not be completed.
func (a *connectAttempt) fail(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = ConnectFailed
	a.since = time.Now()
	a.url = ""
	a.err = reason
}

// done reports whether the attempt has ended, one way or the other.
func (a *connectAttempt) done() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state == ConnectConnected || a.state == ConnectFailed
}

// status reports the attempt, and takes the app key with it: the key is handed
// over once, and is gone from the server as soon as it has been read.
func (a *connectAttempt) status() ConnectStatusResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	started, since := a.started, a.since
	res := ConnectStatusResponse{
		State:   a.state,
		Started: &started,
		Since:   &since,
		URL:     a.url,
		AppKey:  a.appKey,
		Error:   a.err,
	}
	a.appKey = ""
	return res
}

// connectTracker keeps the connection attempts, one per workgroup and share.
type connectTracker struct {
	mu       sync.Mutex
	attempts map[string]*connectAttempt
}

// connectKey names the attempt of a workgroup's connection to a share.
func connectKey(wg stores.Workgroup, share stores.Share) string {
	return wg.UUID.String() + "/" + share.Name
}

// begin starts an attempt in the given phase. A workgroup connects to a share
// once at a time, so an attempt that is still running is returned as it is,
// with false to say that it is not the caller's to drive.
func (t *connectTracker) begin(key string, state ConnectState) (*connectAttempt, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if a, running := t.attempts[key]; running && !a.done() {
		return a, false
	}

	now := time.Now()
	a := &connectAttempt{state: state, started: now, since: now}
	if t.attempts == nil {
		t.attempts = make(map[string]*connectAttempt)
	}
	t.attempts[key] = a

	return a, true
}

// get returns the tracked attempt, or nil if there is none.
func (t *connectTracker) get(key string) *connectAttempt {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.attempts[key]
}

// drop forgets the attempt right away.
func (t *connectTracker) drop(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.attempts, key)
}

// forget drops the attempt once its outcome has had the time to be read, unless
// a later attempt has taken its place by then.
func (t *connectTracker) forget(ctx context.Context, key string, a *connectAttempt) {
	select {
	case <-time.After(connectRetention):
	case <-ctx.Done():
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.attempts[key] == a {
		delete(t.attempts, key)
	}
}
