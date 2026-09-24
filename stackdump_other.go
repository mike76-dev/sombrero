//go:build windows

package main

// watchStackDumps does nothing on Windows, which has no SIGUSR1 to listen for.
func watchStackDumps() {}
