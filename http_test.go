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
	h := newHTTPHandler(context.Background(), stub, "hunter2")

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

// TestHTTPHandlerRatelimitsOnlyTheAPI verifies that loading the UI cannot
// exhaust the request budget that guards the API.
func TestHTTPHandlerRatelimitsOnlyTheAPI(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := newHTTPHandler(context.Background(), stub, "hunter2")

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
