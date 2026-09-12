//go:build !linux && !darwin

package main

import "net"

// tuneConnection does nothing where TCP_NOTSENT_LOWAT isn't available.
func tuneConnection(net.Conn) error { return nil }
