//go:build linux

package main

import "golang.org/x/sys/unix"

// congestionControl is what connections ask for: BBR paces to the path's capacity instead of
// filling its buffers, so small responses don't queue behind bulk data in the network.
const congestionControl = "bbr"

// setCongestion sets a socket's TCP congestion control algorithm.
func setCongestion(fd int, name string) error {
	return unix.SetsockoptString(fd, unix.IPPROTO_TCP, unix.TCP_CONGESTION, name)
}
