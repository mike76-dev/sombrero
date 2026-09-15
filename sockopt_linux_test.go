//go:build linux

package main

import (
	"net"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// congestionOf reads a connection's TCP congestion control algorithm.
func congestionOf(t *testing.T, conn net.Conn) string {
	t.Helper()

	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var name string
	var gerr error
	raw.Control(func(fd uintptr) {
		name, gerr = unix.GetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION)
	})
	if gerr != nil {
		t.Fatalf("reading TCP_CONGESTION: %v", gerr)
	}
	return strings.TrimRight(name, "\x00")
}

// TestSetCongestionSetsTheAlgorithm uses reno, which every Linux lets any process choose.
func TestSetCongestionSetsTheAlgorithm(t *testing.T) {
	server, _ := slowTCPPair(t)
	defer server.Close()

	raw, err := server.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var serr error
	raw.Control(func(fd uintptr) { serr = setCongestion(int(fd), "reno") })
	if serr != nil {
		t.Fatalf("setCongestion: %v", serr)
	}
	if got := congestionOf(t, server); got != "reno" {
		t.Errorf("congestion control is %q, want reno", got)
	}
}

// TestTuneConnectionAsksForBBR is skipped where this process may not choose BBR.
func TestTuneConnectionAsksForBBR(t *testing.T) {
	allowed, _ := os.ReadFile("/proc/sys/net/ipv4/tcp_allowed_congestion_control")
	if !strings.Contains(" "+strings.TrimSpace(string(allowed))+" ", " bbr ") {
		t.Skipf("BBR isn't allowed to unprivileged processes here (allowed: %s)", strings.TrimSpace(string(allowed)))
	}

	server, _ := slowTCPPair(t)
	defer server.Close()

	if err := tuneConnection(server); err != nil {
		t.Fatalf("tuneConnection: %v", err)
	}
	if got := congestionOf(t, server); got != "bbr" {
		t.Errorf("congestion control is %q, want bbr", got)
	}
}
