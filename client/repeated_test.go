package client

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// lines collects what a repeatedFailure writes.
type lines struct {
	mu sync.Mutex
	on []string
}

func (l *lines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.on = append(l.on, format)
	_ = args
}

func (l *lines) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.on)
}

// TestRepeatedFailureCollapsesARun verifies that a backend failing every job is named
// once rather than once per job, and that what was left out is counted.
func TestRepeatedFailureCollapsesARun(t *testing.T) {
	var got lines
	r := &repeatedFailure{logf: got.logf}
	down := errors.New("no more hosts available")

	for range 50 {
		r.report("failed to run upload job", time.Second, down)
	}
	if n := got.count(); n != 1 {
		t.Fatalf("50 of the same failure wrote %d line(s), want 1", n)
	}

	// Another failure is another thing to know about, so it is named at once.
	r.report("failed to run upload job", time.Second, errors.New("database is closed"))
	if n := got.count(); n != 2 {
		t.Fatalf("a different failure wrote %d line(s) in total, want 2", n)
	}

	// Once the interval has passed, the failure that is still there is named again,
	// with what was left out in the meantime.
	r.report("failed to run upload job", time.Second, down)
	for range 9 {
		r.report("failed to run upload job", time.Second, down)
	}
	r.mu.Lock()
	r.lastLogged = time.Now().Add(-2 * repeatLogInterval)
	r.mu.Unlock()
	r.report("failed to run upload job", time.Second, down)

	got.mu.Lock()
	last := got.on[len(got.on)-1]
	got.mu.Unlock()
	if !strings.Contains(last, "more like it") {
		t.Errorf("the line after a run does not say what was left out: %q", last)
	}
}

// TestRepeatedFailureIsSafeForManyWorkers is the eight upload workers reporting at once.
func TestRepeatedFailureIsSafeForManyWorkers(t *testing.T) {
	var got lines
	r := &repeatedFailure{logf: got.logf}
	down := errors.New("no more hosts available")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				r.report("failed to run upload job", time.Second, down)
			}
		}()
	}
	wg.Wait()

	if n := got.count(); n != 1 {
		t.Fatalf("800 reports of one failure wrote %d line(s), want 1", n)
	}
}
