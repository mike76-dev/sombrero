package api

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// maxRequestsPerSecond is a ceiling on ordinary traffic. It is set well
	// above what the web UI needs when it loads a page: the limiter is not
	// meant to pace a legitimate client.
	maxRequestsPerSecond = 100

	// maxAuthFailuresPerMinute is the budget a host gets for requests that
	// fail authentication. This is the limit that actually matters, since
	// slowing down password guessing is the only thing the limiter protects
	// against once the API is bound to localhost by default.
	maxAuthFailuresPerMinute = 10
)

// ratelimiter keeps the API request stats and determines whether
// to allow the request or not.
type ratelimiter struct {
	requests     map[string]int
	authFailures map[string]int
	mu           sync.Mutex
}

func newRatelimiter(ctx context.Context) *ratelimiter {
	rl := &ratelimiter{
		requests:     make(map[string]int),
		authFailures: make(map[string]int),
	}

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		var ticks int
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			rl.mu.Lock()
			rl.requests = make(map[string]int)
			ticks++
			if ticks == 60 {
				rl.authFailures = make(map[string]int)
				ticks = 0
			}
			rl.mu.Unlock()
		}
	}()

	return rl
}

// limitExceeded returns true if there are too many requests from the given host.
func (rl *ratelimiter) limitExceeded(addr string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.authFailures[addr] >= maxAuthFailuresPerMinute {
		return true
	}

	rl.requests[addr]++
	return rl.requests[addr] > maxRequestsPerSecond
}

// recordAuthFailure notes that a request from the given host failed to
// authenticate.
func (rl *ratelimiter) recordAuthFailure(addr string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.authFailures[addr]++
}

// statusRecorder remembers the status code a handler wrote, so that the
// ratelimiter can tell a rejected request from an accepted one.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	return sr.ResponseWriter.Write(b)
}

// Ratelimit wraps an http.Handler to reject hosts that flood the API. It is
// meant to sit outside BasicAuth, so that it can count the requests that fail
// to authenticate separately from the rest.
func Ratelimit(ctx context.Context) func(http.Handler) http.Handler {
	rl := newRatelimiter(ctx)
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			host := getRemoteHost(req)
			if rl.limitExceeded(host) {
				writeError(w, "too many requests", http.StatusTooManyRequests)
				return
			}

			rec := &statusRecorder{ResponseWriter: w}
			h.ServeHTTP(rec, req)
			if rec.status == http.StatusUnauthorized {
				rl.recordAuthFailure(host)
			}
		})
	}
}

// getRemoteHost returns the address of the remote host.
func getRemoteHost(r *http.Request) (host string) {
	host, _, _ = net.SplitHostPort(r.RemoteAddr)
	if host == "127.0.0.1" || host == "localhost" {
		xff := r.Header.Values("X-Forwarded-For")
		if len(xff) > 0 {
			host = xff[0]
		}
	}
	return
}
