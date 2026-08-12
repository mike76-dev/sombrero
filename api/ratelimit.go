package api

import (
	"context"
	"net"
	"net/http"
	"strings"
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

// getRemoteHost returns the address of the remote host. A reverse proxy on the
// same machine makes every request arrive from loopback, and the client it was
// made for is the last entry of X-Forwarded-For: a proxy appends the peer it
// saw to whatever the client sent, so that entry is the only one the client
// cannot write. Coming from anywhere else the header is the client's own to
// make up, and is ignored.
func getRemoteHost(r *http.Request) (host string) {
	host, _, _ = net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return
	}

	xff := r.Header.Values("X-Forwarded-For")
	if len(xff) == 0 {
		return
	}

	last := xff[len(xff)-1]
	if i := strings.LastIndex(last, ","); i >= 0 {
		last = last[i+1:]
	}
	last = strings.TrimSpace(last)
	if h, _, err := net.SplitHostPort(last); err == nil {
		last = h
	}
	if net.ParseIP(last) == nil {
		return
	}

	return last
}
