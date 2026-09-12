//go:build darwin

package main

// congestionControl is left to the system: macOS doesn't let a socket choose it.
const congestionControl = ""

// setCongestion does nothing on macOS.
func setCongestion(int, string) error { return nil }
