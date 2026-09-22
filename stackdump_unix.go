//go:build !windows

package main

import (
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
)

// maxStackDump bounds what a dump may grow to, for a server holding enough goroutines that
// the stacks of all of them would otherwise be megabytes of log.
const maxStackDump = 64 << 20

// watchStackDumps writes the stacks of every goroutine to the log on SIGUSR1, and leaves the
// server running. It is what a stuck server is looked at with, in place of killing it.
func watchStackDumps() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)

	go func() {
		for range ch {
			log.Printf("goroutine dump:\n%s", stackDump())
		}
	}()
}

// stackDump returns the stacks of all goroutines, growing the buffer until they fit.
func stackDump() []byte {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) || len(buf) >= maxStackDump {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}
