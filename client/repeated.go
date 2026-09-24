package client

import (
	"log"
	"sync"
	"time"
)

// repeatLogInterval is how often a failure that keeps coming back is named again.
const repeatLogInterval = time.Minute

// repeatedFailure collapses a run of the same failure into one line. A backend with no
// hosts to write to fails every job of every worker, which is the same line many times
// a second for as long as the outage lasts.
type repeatedFailure struct {
	logf func(string, ...any) // a field so that a test can read what was written

	mu         sync.Mutex
	last       string
	suppressed int
	lastLogged time.Time
}

func newRepeatedFailure() *repeatedFailure {
	return &repeatedFailure{logf: log.Printf}
}

// report names the failure, unless the same one was named a moment ago. What is left
// out is counted and reported with the next line, so nothing goes by unsaid.
func (r *repeatedFailure) report(what string, delay time.Duration, err error) {
	msg := err.Error()

	r.mu.Lock()
	if msg == r.last && time.Since(r.lastLogged) < repeatLogInterval {
		r.suppressed++
		r.mu.Unlock()

		return
	}

	skipped := r.suppressed
	r.suppressed = 0
	r.last = msg
	r.lastLogged = time.Now()
	r.mu.Unlock()

	if skipped > 0 {
		r.logf("%s, retrying in %s: %v (and %d more like it)", what, delay, err, skipped)

		return
	}

	r.logf("%s, retrying in %s: %v", what, delay, err)
}
