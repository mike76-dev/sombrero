//go:build linux || darwin

package main

import (
	"net"

	"golang.org/x/sys/unix"
)

// notSentLowWater caps the unsent data the kernel holds per connection. Anything beyond it waits in
// the send queue, where small responses can overtake file data.
const notSentLowWater = 128 << 10

// tuneConnection applies notSentLowWater to a TCP connection, and congestionControl where the
// system allows it.
func tuneConnection(conn net.Conn) error {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return nil
	}

	raw, err := tc.SyscallConn()
	if err != nil {
		return err
	}

	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, notSentLowWater)

		// Best effort: without the privilege or the kernel module, the system default stays.
		_ = setCongestion(int(fd), congestionControl)
	}); err != nil {
		return err
	}

	return serr
}
