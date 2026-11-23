package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/julienschmidt/httprouter"
)

// Pinger is the part of a store that can say whether it is reachable, for
// the health check.
type Pinger interface {
	Ping(ctx context.Context) error
}

// HealthResponse is the response type of GET /health.
type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// healthTimeout bounds the store check of a health call.
const healthTimeout = 5 * time.Second

// Health returns the handler of the health check, served without the
// password for the proxies and the monitors that need it: the server is up
// and its store answers, or it is not.
func (api *API) Health() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if p, ok := api.store.(Pinger); ok {
			ctx, cancel := context.WithTimeout(req.Context(), healthTimeout)
			defer cancel()
			if err := p.Ping(ctx); err != nil {
				log.Printf("health: the store does not answer: %v", err)
				writeJSONStatus(w, http.StatusServiceUnavailable, HealthResponse{Status: "unavailable", Version: api.version})
				return
			}
		}
		writeJSON(w, HealthResponse{Status: "ok", Version: api.version})
	})
}

// durationBuckets are the upper bounds of the request duration histogram, in seconds.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// requestStats counts the requests one handler served, by the class of their
// status, and how long they took.
type requestStats struct {
	codes   map[string]uint64
	buckets []uint64
	sum     float64
	count   uint64
}

// requestMetrics is what the HTTP surfaces have served since the start, by
// handler. It is one for the process, as the surfaces are.
var requestMetrics = struct {
	mu       sync.Mutex
	handlers map[string]*requestStats
}{handlers: make(map[string]*requestStats)}

// Measure wraps a handler so that its requests are counted under the name.
func Measure(name string) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			start := time.Now()
			mw := &measuredWriter{ResponseWriter: w, code: http.StatusOK}
			h.ServeHTTP(mw, req)
			record(name, mw.code, time.Since(start))
		})
	}
}

func record(name string, code int, took time.Duration) {
	requestMetrics.mu.Lock()
	defer requestMetrics.mu.Unlock()
	s := requestMetrics.handlers[name]
	if s == nil {
		s = &requestStats{codes: make(map[string]uint64), buckets: make([]uint64, len(durationBuckets))}
		requestMetrics.handlers[name] = s
	}
	s.codes[fmt.Sprintf("%dxx", code/100)]++
	secs := took.Seconds()
	for i, bound := range durationBuckets {
		if secs <= bound {
			s.buckets[i]++
		}
	}
	s.sum += secs
	s.count++
}

// measuredWriter notes the status a handler answered with. The handler may
// still reach the writer underneath through Unwrap, as a streaming one does.
type measuredWriter struct {
	http.ResponseWriter
	code int
}

func (m *measuredWriter) WriteHeader(code int) {
	m.code = code
	m.ResponseWriter.WriteHeader(code)
}

func (m *measuredWriter) Flush() {
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (m *measuredWriter) Unwrap() http.ResponseWriter {
	return m.ResponseWriter
}

// metricsHandlerGET handles the GET /metrics calls, in the text format the
// monitoring systems scrape.
func (api *API) metricsHandlerGET(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	api.writeMetrics(&b)
	io.WriteString(w, b.String())
}

// writeMetrics writes every series the server reports.
func (api *API) writeMetrics(w io.Writer) {
	gauge := func(name, help string, value any, labels ...string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s%s %v\n", name, help, name, name, labelSet(labels), value)
	}
	counter := func(name, help string, value any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %v\n", name, help, name, name, value)
	}

	gauge("sombrero_info", "The version and the mode of the server.", 1, "version", api.version, "mode", api.mode.String())
	up := 1
	if p, ok := api.store.(Pinger); ok {
		ctx, cancel := context.WithTimeout(api.ctx, healthTimeout)
		if err := p.Ping(ctx); err != nil {
			up = 0
		}
		cancel()
	}
	gauge("sombrero_store_up", "Whether the store answers.", up)

	if shares, err := api.store.GetAllShares(); err == nil {
		byType := map[string]int{"indexd": 0, "renterd": 0}
		for _, s := range shares {
			byType[s.Type]++
		}
		types := make([]string, 0, len(byType))
		for t := range byType {
			types = append(types, t)
		}
		sort.Strings(types)
		fmt.Fprintf(w, "# HELP sombrero_shares The shares registered, by backend.\n# TYPE sombrero_shares gauge\n")
		for _, t := range types {
			fmt.Fprintf(w, "sombrero_shares%s %d\n", labelSet([]string{"type", t}), byType[t])
		}
	}
	if conns, ok := api.store.(Connections); ok {
		if list, err := conns.KeyedConnections(); err == nil {
			gauge("sombrero_connections", "The connections of workgroups to indexd shares.", len(list))
		}
	}

	if api.server != nil {
		stats := api.server.Stats()
		gauge("sombrero_start_time_seconds", "When the server started, in seconds since the epoch.", stats.Start.Unix())
		gauge("sombrero_smb_sessions", "The SMB sessions established now.", stats.SOpens)
		counter("sombrero_smb_opens_total", "The files and folders opened over SMB since the start.", stats.FOpens)
		counter("sombrero_smb_password_errors_total", "The SMB logins refused for a wrong password.", stats.PwErrors)
		counter("sombrero_smb_permission_errors_total", "The SMB requests refused for a missing right.", stats.PermErrors)
		counter("sombrero_smb_sent_bytes_total", "The bytes sent to SMB clients.", stats.BytesSent)
		counter("sombrero_smb_received_bytes_total", "The bytes received from SMB clients.", stats.BytesRcvd)
		if stats.Backlog != nil {
			gauge("sombrero_backlog_bytes", "The data waiting in the database to be uploaded to the network.", stats.Backlog.Buffered)
			gauge("sombrero_backlog_limit_bytes", "The backlog at which writes are held back; 0 is no limit.", stats.Backlog.Limit)
			gauge("sombrero_backlog_disk_bytes", "The database space the upload buffers take up.", stats.Backlog.OnDisk)
		}
	}

	writeRequestMetrics(w)
}

// writeRequestMetrics writes the request counts and durations by handler.
func writeRequestMetrics(w io.Writer) {
	requestMetrics.mu.Lock()
	defer requestMetrics.mu.Unlock()
	names := make([]string, 0, len(requestMetrics.handlers))
	for name := range requestMetrics.handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return
	}

	fmt.Fprintf(w, "# HELP sombrero_http_requests_total The HTTP requests served, by handler and class of status.\n# TYPE sombrero_http_requests_total counter\n")
	for _, name := range names {
		s := requestMetrics.handlers[name]
		codes := make([]string, 0, len(s.codes))
		for code := range s.codes {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		for _, code := range codes {
			fmt.Fprintf(w, "sombrero_http_requests_total%s %d\n", labelSet([]string{"handler", name, "code", code}), s.codes[code])
		}
	}
	fmt.Fprintf(w, "# HELP sombrero_http_request_duration_seconds How long the HTTP requests took, by handler.\n# TYPE sombrero_http_request_duration_seconds histogram\n")
	for _, name := range names {
		s := requestMetrics.handlers[name]
		for i, bound := range durationBuckets {
			fmt.Fprintf(w, "sombrero_http_request_duration_seconds_bucket%s %d\n", labelSet([]string{"handler", name, "le", fmt.Sprint(bound)}), s.buckets[i])
		}
		fmt.Fprintf(w, "sombrero_http_request_duration_seconds_bucket%s %d\n", labelSet([]string{"handler", name, "le", "+Inf"}), s.count)
		fmt.Fprintf(w, "sombrero_http_request_duration_seconds_sum%s %g\n", labelSet([]string{"handler", name}), s.sum)
		fmt.Fprintf(w, "sombrero_http_request_duration_seconds_count%s %d\n", labelSet([]string{"handler", name}), s.count)
	}
}

// labelSet formats the label pairs as a series' labels, empty for none.
func labelSet(pairs []string) string {
	if len(pairs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		value := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(pairs[i+1])
		fmt.Fprintf(&b, `%s="%s"`, pairs[i], value)
	}
	b.WriteByte('}')
	return b.String()
}
