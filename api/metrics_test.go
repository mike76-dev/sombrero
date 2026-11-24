package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/stores"
)

// TestHealth verifies that the health check says ok while the store answers
// and unavailable once it does not, and takes nothing but a GET.
func TestHealth(t *testing.T) {
	ms := &mockStore{}
	h := newTestAPI(ms).Health()
	call := func(method string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/health", nil))
		return w
	}

	w := call(http.MethodGet)
	checkStatus(t, w, http.StatusOK)
	if got := decodeJSON[HealthResponse](t, w); got != (HealthResponse{Status: "ok", Version: testVersion}) {
		t.Errorf("want ok with the version, got %+v", got)
	}

	ms.pingErr = errors.New("connection refused")
	w = call(http.MethodGet)
	checkStatus(t, w, http.StatusServiceUnavailable)
	if got := decodeJSON[HealthResponse](t, w); got.Status != "unavailable" {
		t.Errorf("store down: want unavailable, got %+v", got)
	}
	if w := call(http.MethodPost); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: want %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

// TestMetrics verifies that the metrics carry the server's statistics, the
// shares by backend, the store's state and the requests the surfaces served.
func TestMetrics(t *testing.T) {
	ms := &mockStore{
		getAllShares: func() ([]stores.Share, error) {
			return []stores.Share{{Name: "docs", Type: "indexd"}, {Name: "pub", Type: "renterd"}, {Name: "more", Type: "indexd"}}, nil
		},
	}
	stats := ServerStats{
		Start:      time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC),
		FOpens:     12,
		SOpens:     3,
		PwErrors:   1,
		PermErrors: 2,
		BytesSent:  1024,
		BytesRcvd:  2048,
		Backlog:    &BacklogStats{Buffered: 1 << 20, Limit: 1 << 30, OnDisk: 3 << 20},
	}
	transfers := []TransferStats{
		{Share: "docs", Workgroup: "4b8e1c2a-0000-4000-8000-000000000001", Uploaded: 5000, Downloaded: 700},
		{Share: "pub", Uploaded: 10, Downloaded: 20},
	}
	api := newTestAPIWithServer(ms, &mockServer{stats: stats, transfers: transfers})

	// Two requests through a measured handler: one quick and fine, one not found.
	measured := Measure("files")(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/gone" {
			http.NotFound(w, req)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, p := range []string{"/here", "/gone"} {
		measured.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}

	w := doRequest(api, http.MethodGet, "/metrics", nil)
	checkStatus(t, w, http.StatusOK)
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type: got %q", got)
	}
	body := w.Body.String()
	for _, want := range []string{
		`sombrero_info{version="1.2.3",mode="normal"} 1`,
		"sombrero_store_up 1",
		`sombrero_shares{type="indexd"} 2`,
		`sombrero_shares{type="renterd"} 1`,
		"sombrero_start_time_seconds 1795420800",
		"sombrero_smb_sessions 3",
		"sombrero_smb_opens_total 12",
		"sombrero_smb_password_errors_total 1",
		"sombrero_smb_permission_errors_total 2",
		"sombrero_smb_sent_bytes_total 1024",
		"sombrero_smb_received_bytes_total 2048",
		"sombrero_backlog_bytes 1048576",
		"sombrero_backlog_limit_bytes 1073741824",
		"sombrero_backlog_disk_bytes 3145728",
		`sombrero_network_uploaded_bytes_total{share="docs",workgroup="4b8e1c2a-0000-4000-8000-000000000001"} 5000`,
		`sombrero_network_downloaded_bytes_total{share="docs",workgroup="4b8e1c2a-0000-4000-8000-000000000001"} 700`,
		`sombrero_network_uploaded_bytes_total{share="pub",workgroup=""} 10`,
		`sombrero_network_downloaded_bytes_total{share="pub",workgroup=""} 20`,
		`sombrero_http_requests_total{handler="files",code="2xx"} 1`,
		`sombrero_http_requests_total{handler="files",code="4xx"} 1`,
		`sombrero_http_request_duration_seconds_bucket{handler="files",le="+Inf"} 2`,
		`sombrero_http_request_duration_seconds_count{handler="files"} 2`,
		"# TYPE sombrero_http_request_duration_seconds histogram",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("want the metrics to carry %q, got:\n%s", want, body)
		}
	}

	// A store that does not answer is reported, and the rest still comes.
	ms.pingErr = errors.New("connection refused")
	w = doRequest(api, http.MethodGet, "/metrics", nil)
	checkStatus(t, w, http.StatusOK)
	if body := w.Body.String(); !strings.Contains(body, "sombrero_store_up 0") || !strings.Contains(body, "sombrero_smb_sessions 3") {
		t.Errorf("store down: want store_up 0 with the rest, got:\n%s", body)
	}
}

// TestLabelSetEscapes verifies that a label value with quotes, backslashes
// or line breaks stays one series.
func TestLabelSetEscapes(t *testing.T) {
	got := labelSet([]string{"a", `x"y\z` + "\n", "b", "plain"})
	want := `{a="x\"y\\z\n",b="plain"}`
	if got != want {
		t.Errorf("want %s, got %s", want, got)
	}
	if got := labelSet(nil); got != "" {
		t.Errorf("no labels: want nothing, got %q", got)
	}
}
