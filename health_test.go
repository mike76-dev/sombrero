package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mike76-dev/sombrero/api"
	"github.com/mike76-dev/sombrero/stores"
)

// TestHealthNeedsNoPassword verifies that the health check is served at
// /health without the password, that the API's metrics stay behind it, and
// that what the surfaces serve is counted under their names.
func TestHealthNeedsNoPassword(t *testing.T) {
	a := api.NewAPI(context.Background(), healthStore{}, nil, stores.Config{Mode: stores.ModeNormal}, "1.2.3")
	h := newHTTPHandler(context.Background(), a, nil, nil, a.Health(), "hunter2", false)

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.0.0.2:12345"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := get("/health"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Errorf("/health: want %d and ok, got %d (%s)", http.StatusOK, w.Code, w.Body.String())
	}
	if w := get("/api/metrics"); w.Code != http.StatusUnauthorized {
		t.Errorf("/api/metrics without the password: want %d, got %d", http.StatusUnauthorized, w.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.RemoteAddr = "10.0.0.2:12345"
	req.SetBasicAuth("", "hunter2")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/metrics: want %d, got %d (%s)", http.StatusOK, w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`sombrero_info{version="1.2.3",mode="normal"} 1`,
		`sombrero_http_requests_total{handler="api",code="4xx"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("want the metrics to carry %q, got:\n%s", want, body)
		}
	}
}

// healthStore is as little of a store as the health check and the metrics
// need: it answers, and it has no shares.
type healthStore struct{ api.Store }

func (healthStore) Ping(context.Context) error { return nil }

func (healthStore) GetAllShares() ([]stores.Share, error) { return nil, nil }
