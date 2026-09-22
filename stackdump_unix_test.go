//go:build !windows

package main

import (
	"strings"
	"testing"
)

// TestStackDumpHoldsEveryGoroutine verifies that a dump is what a stuck server is read from:
// the stacks of all goroutines, not only the one asking.
func TestStackDumpHoldsEveryGoroutine(t *testing.T) {
	started := make(chan struct{})
	done := make(chan struct{})
	defer close(done)
	go func() {
		close(started)
		<-done
	}()
	<-started

	dump := string(stackDump())
	if !strings.Contains(dump, "goroutine ") {
		t.Fatalf("the dump holds no goroutines: %.200s", dump)
	}
	if !strings.Contains(dump, "TestStackDumpHoldsEveryGoroutine") {
		t.Errorf("the dump leaves out the goroutine that asked for it: %.400s", dump)
	}
	if strings.Count(dump, "goroutine ") < 2 {
		t.Errorf("the dump holds one goroutine, want every one of them: %.400s", dump)
	}
}
