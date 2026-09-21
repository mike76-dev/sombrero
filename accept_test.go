package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/stores"
)

// acceptTest serves a listener on the loopback with a server allowing max connections.
func acceptTest(t *testing.T, max int) (*server, *stores.JSONStore, net.Listener, chan struct{}) {
	t.Helper()

	store, err := stores.NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStore: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := newServerState(ctx, store, stores.Config{MaxConnections: max})
	s.applyCapabilities()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.acceptConnections(l)
	}()

	return s, store, l, done
}

// dial connects and reports whether the server hung up within wait.
func dial(t *testing.T, addr string, wait time.Duration) bool {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	conn.SetReadDeadline(time.Now().Add(wait))
	_, err = conn.Read(make([]byte, 1))
	var ne net.Error
	return !errors.As(err, &ne) || !ne.Timeout()
}

// How long a connection has to stay up to count as served, and to go to count as turned away.
const (
	servedFor    = 200 * time.Millisecond
	turnedAwayIn = 2 * time.Second
)

func banned(t *testing.T, store *stores.JSONStore) bool {
	t.Helper()
	b, _, err := store.IsBanned("127.0.0.1")
	if err != nil {
		t.Fatalf("IsBanned: %v", err)
	}
	return b
}

// TestAcceptWhileShuttingDown verifies that a client reconnecting to a server that is shutting
// down is turned away without being counted, and so is never banned for it.
func TestAcceptWhileShuttingDown(t *testing.T) {
	s, store, l, _ := acceptTest(t, 2)
	s.mu.Lock()
	s.enabled = false
	s.mu.Unlock()

	for i := range 10 {
		if !dial(t, l.Addr().String(), turnedAwayIn) {
			t.Fatalf("connection %d was served during the shutdown", i)
		}
	}

	if banned(t, store) {
		t.Fatal("a client reconnecting during the shutdown was banned")
	}
	s.mu.Lock()
	n := s.connectionCount["127.0.0.1"]
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("want no connections counted, got %d", n)
	}
}

// TestAcceptBansTooManyConnections is the control: a running server still bans at the limit.
func TestAcceptBansTooManyConnections(t *testing.T) {
	_, store, l, _ := acceptTest(t, 2)

	for i := range 2 {
		if dial(t, l.Addr().String(), servedFor) {
			t.Fatalf("connection %d was turned away below the limit", i)
		}
	}
	if !dial(t, l.Addr().String(), turnedAwayIn) {
		t.Fatal("the connection over the limit was served")
	}
	if !banned(t, store) {
		t.Fatal("the host over the limit was not banned")
	}
}

// TestAcceptStopsWhenTheListenerCloses verifies that the loop ends rather than spins.
func TestAcceptStopsWhenTheListenerCloses(t *testing.T) {
	_, _, l, done := acceptTest(t, 2)
	l.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("acceptConnections did not return after the listener closed")
	}
}
