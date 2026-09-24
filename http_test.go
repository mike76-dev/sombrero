package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPHandlerRouting verifies how the API and the web UI divide up the
// address space: the API answers under /api with the prefix taken off, and
// everything else reaches the UI without a password.
func TestHTTPHandlerRouting(t *testing.T) {
	var seen string
	stub := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen = req.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	h := newHTTPHandler(context.Background(), stub, "hunter2", false)

	get := func(path, password string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.0.0.1:12345"
		if password != "" {
			req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+password)))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// The API sees the path without the /api prefix, so the routes stay
	// as they are registered.
	if w := get("/api/bans", "hunter2"); w.Code != http.StatusNoContent {
		t.Fatalf("an API call: want %d, got %d", http.StatusNoContent, w.Code)
	}
	if seen != "/bans" {
		t.Errorf("want the API to see %q, got %q", "/bans", seen)
	}

	// The API is not reachable without the password.
	seen = ""
	if w := get("/api/bans", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("an API call without a password: want %d, got %d", http.StatusUnauthorized, w.Code)
	}
	if seen != "" {
		t.Errorf("want the API left untouched, got a call to %q", seen)
	}

	// The UI is served without one, so that it can ask for it itself.
	seen = ""
	w := get("/", "")
	if w.Code == http.StatusUnauthorized {
		t.Error("the web UI: want it served without a password, got 401")
	}
	if seen != "" {
		t.Errorf("want the UI served by the UI handler, got an API call to %q", seen)
	}

	// A UI route must not be mistaken for an API call.
	seen = ""
	if w := get("/shares", ""); w.Code == http.StatusUnauthorized {
		t.Error("a UI route: want it served without a password, got 401")
	}
	if seen != "" {
		t.Errorf("want a UI route kept away from the API, got a call to %q", seen)
	}
}

// TestHTTPHandlerServesTheProfilesInDebugOnly verifies that the profiles are there to be asked
// for in the debug mode, behind the password, and are not registered at all without it.
func TestHTTPHandlerServesTheProfilesInDebugOnly(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	get := func(h http.Handler, path, password string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.0.0.1:12345"
		if password != "" {
			req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+password)))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	debug := newHTTPHandler(context.Background(), stub, "hunter2", true)
	if w := get(debug, "/api/debug/pprof/goroutine?debug=1", "hunter2"); w.Code != http.StatusOK {
		t.Errorf("the goroutine profile was answered %d, want it served", w.Code)
	}

	// Without the password the profiles are as unreachable as the rest of the API.
	if w := get(debug, "/api/debug/pprof/goroutine?debug=1", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the goroutine profile without a password was answered %d, want it refused", w.Code)
	}

	// The API itself is still served in the debug mode, profiles or no profiles.
	if w := get(debug, "/api/bans", "hunter2"); w.Code != http.StatusNoContent {
		t.Errorf("the API in the debug mode was answered %d, want it served", w.Code)
	}

	// Off, the path reaches the API instead, which knows nothing of it.
	plain := newHTTPHandler(context.Background(), stub, "hunter2", false)
	if w := get(plain, "/api/debug/pprof/goroutine?debug=1", "hunter2"); w.Code != http.StatusNoContent {
		t.Errorf("without the debug mode the profile path was answered %d, want it left to the API", w.Code)
	}
}

// TestHTTPHandlerRatelimitsOnlyTheAPI verifies that loading the UI cannot
// exhaust the request budget that guards the API.
func TestHTTPHandlerRatelimitsOnlyTheAPI(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := newHTTPHandler(context.Background(), stub, "hunter2", false)

	for i := range 500 {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d for the UI was ratelimited", i)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/bans", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":hunter2")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("the API after the UI loaded: want %d, got %d (body: %s)",
			http.StatusNoContent, w.Code, strings.TrimSpace(w.Body.String()))
	}
}
