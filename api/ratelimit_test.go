package api

import (
	"context"
	"encoding/base64"
	"fmt"
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

// proxied builds a request as a reverse proxy on the same machine would pass
// it on: it arrives from loopback, and the header carries whatever the client
// sent with the address the proxy saw appended to it.
func proxied(h http.Handler, from, sent, seen, password string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/bans", nil)
	req.RemoteAddr = from
	if sent != "" {
		req.Header.Add("X-Forwarded-For", sent)
	}
	req.Header.Add("X-Forwarded-For", seen)
	if password != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+password)))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestTheForwardedHeaderIsIgnoredFromElsewhere verifies that a client reaching
// the API directly is counted as one host however it labels itself, since with
// nothing in front to append to the header, none of it is trustworthy.
func TestTheForwardedHeaderIsIgnoredFromElsewhere(t *testing.T) {
	next := &okHandler{}
	h := Ratelimit(context.Background())(BasicAuth("hunter2")(next))

	for i := range maxAuthFailuresPerMinute {
		if w := proxied(h, "10.0.0.1:12345", "", fmt.Sprintf("10.1.1.%d", i), "wrong"); w.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d: want %d, got %d", i, http.StatusUnauthorized, w.Code)
		}
	}

	if w := proxied(h, "10.0.0.1:12345", "", "10.1.1.99", "wrong"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("beyond the budget: want %d, got %d", http.StatusTooManyRequests, w.Code)
	}
}

// TestBehindAProxyTheAppendedAddressIsWhatCounts verifies that the entry the
// proxy appended is what the guesses are counted against, so that a client
// cannot mint a fresh budget by sending a header of its own, and that another
// client behind the same proxy keeps its own budget.
func TestBehindAProxyTheAppendedAddressIsWhatCounts(t *testing.T) {
	next := &okHandler{}
	h := Ratelimit(context.Background())(BasicAuth("hunter2")(next))

	for i := range maxAuthFailuresPerMinute {
		if w := proxied(h, "127.0.0.1:12345", fmt.Sprintf("10.1.1.%d", i), "10.2.2.2", "wrong"); w.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d: want %d, got %d", i, http.StatusUnauthorized, w.Code)
		}
	}

	if w := proxied(h, "127.0.0.1:12345", "10.1.1.99", "10.2.2.2", "wrong"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("beyond the budget: want %d, got %d", http.StatusTooManyRequests, w.Code)
	}

	// The admin, coming through the same proxy from somewhere else, is not
	// caught in the guesser's bucket.
	if w := proxied(h, "127.0.0.1:12345", "", "10.2.2.3", "hunter2"); w.Code != http.StatusOK {
		t.Fatalf("another client: want %d, got %d", http.StatusOK, w.Code)
	}
	if next.served != 1 {
		t.Fatalf("want 1 request served, got %d", next.served)
	}
}

// TestTheForwardedHeaderIsReadFromTheEnd covers the shapes the header arrives
// in: several header lines, several entries on one line, an address with a
// port on it, and values a proxy would never have written.
func TestTheForwardedHeaderIsReadFromTheEnd(t *testing.T) {
	tests := []struct {
		name string
		from string
		xff  []string
		want string
	}{
		{"not from loopback", "10.0.0.1:12345", []string{"10.1.1.1"}, "10.0.0.1"},
		{"no header", "127.0.0.1:12345", nil, "127.0.0.1"},
		{"one entry", "127.0.0.1:12345", []string{"10.2.2.2"}, "10.2.2.2"},
		{"IPv6 loopback", "[::1]:12345", []string{"10.2.2.2"}, "10.2.2.2"},
		{"several lines", "127.0.0.1:12345", []string{"10.1.1.1", "10.2.2.2"}, "10.2.2.2"},
		{"one line", "127.0.0.1:12345", []string{"10.1.1.1, 10.2.2.2"}, "10.2.2.2"},
		{"mixed", "127.0.0.1:12345", []string{"10.1.1.1, 10.1.1.2", "10.2.2.2"}, "10.2.2.2"},
		{"with a port", "127.0.0.1:12345", []string{"10.2.2.2:4321"}, "10.2.2.2"},
		{"IPv6", "127.0.0.1:12345", []string{"2001:db8::1"}, "2001:db8::1"},
		{"IPv6 with a port", "127.0.0.1:12345", []string{"[2001:db8::1]:4321"}, "2001:db8::1"},
		{"not an address", "127.0.0.1:12345", []string{"10.1.1.1, unknown"}, "127.0.0.1"},
		{"empty", "127.0.0.1:12345", []string{""}, "127.0.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/bans", nil)
			req.RemoteAddr = tt.from
			for _, v := range tt.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if host := getRemoteHost(req); host != tt.want {
				t.Errorf("want %q, got %q", tt.want, host)
			}
		})
	}
}
