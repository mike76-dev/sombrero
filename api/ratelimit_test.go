package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

// okHandler counts the requests that make it past the middleware.
type okHandler struct{ served int }

func (h *okHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h.served++
	w.WriteHeader(http.StatusOK)
}

func request(h http.Handler, password string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/bans", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	if password != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+password)))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestBasicAuth verifies that only the configured password is accepted, and
// in particular that an unauthenticated request is turned away rather than
// matching an empty configured password.
func TestBasicAuth(t *testing.T) {
	next := &okHandler{}
	h := BasicAuth("hunter2")(next)

	if w := request(h, "hunter2"); w.Code != http.StatusOK {
		t.Errorf("the right password: want %d, got %d", http.StatusOK, w.Code)
	}
	if w := request(h, "wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("the wrong password: want %d, got %d", http.StatusUnauthorized, w.Code)
	}
	if w := request(h, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no credentials: want %d, got %d", http.StatusUnauthorized, w.Code)
	}

	// A password that shares a prefix with the configured one must not be
	// treated any differently from one that does not.
	if w := request(h, "hunter"); w.Code != http.StatusUnauthorized {
		t.Errorf("a prefix of the password: want %d, got %d", http.StatusUnauthorized, w.Code)
	}
}

// TestRatelimitAllowsNormalTraffic verifies that the limiter leaves a client
// alone at the request rates the web UI produces when it loads a page.
func TestRatelimitAllowsNormalTraffic(t *testing.T) {
	next := &okHandler{}
	h := Ratelimit(context.Background())(BasicAuth("hunter2")(next))

	for i := range maxRequestsPerSecond {
		if w := request(h, "hunter2"); w.Code != http.StatusOK {
			t.Fatalf("request %d: want %d, got %d", i, http.StatusOK, w.Code)
		}
	}
	if next.served != maxRequestsPerSecond {
		t.Fatalf("want %d requests served, got %d", maxRequestsPerSecond, next.served)
	}

	// The ceiling is still a ceiling.
	if w := request(h, "hunter2"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("beyond the ceiling: want %d, got %d", http.StatusTooManyRequests, w.Code)
	}
}

// TestRatelimitThrottlesAuthFailures verifies that a host guessing passwords
// runs out of attempts long before it reaches the general request ceiling,
// which is the case the limiter exists for.
func TestRatelimitThrottlesAuthFailures(t *testing.T) {
	next := &okHandler{}
	h := Ratelimit(context.Background())(BasicAuth("hunter2")(next))

	for i := range maxAuthFailuresPerMinute {
		if w := request(h, "wrong"); w.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d: want %d, got %d", i, http.StatusUnauthorized, w.Code)
		}
	}

	// Once the budget is spent the host is turned away before the password
	// is even looked at, so the right password does not get it back in.
	if w := request(h, "hunter2"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("after the failures: want %d, got %d", http.StatusTooManyRequests, w.Code)
	}
	if next.served != 0 {
		t.Fatalf("want no request served, got %d", next.served)
	}

	// A different host is unaffected.
	req := httptest.NewRequest(http.MethodGet, "/bans", nil)
	req.RemoteAddr = "10.0.0.2:12345"
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":hunter2")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("another host: want %d, got %d", http.StatusOK, w.Code)
	}
}
