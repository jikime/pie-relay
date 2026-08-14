package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const requestIDHeader = "X-Request-ID"

var requestIDSequence atomic.Uint64

type httpMetricKey struct {
	Method string
	Route  string
	Status int
}

type httpMetricValue struct {
	Count       uint64
	DurationSum float64
	Buckets     [7]uint64
}

type httpMetrics struct {
	mu       sync.Mutex
	requests map[httpMetricKey]httpMetricValue
	inFlight atomic.Int64
}

func newHTTPMetrics() *httpMetrics {
	return &httpMetrics{requests: make(map[httpMetricKey]httpMetricValue)}
}

func (m *httpMetrics) observe(method, route string, status int, duration time.Duration) {
	key := httpMetricKey{Method: method, Route: route, Status: status}
	seconds := duration.Seconds()
	m.mu.Lock()
	value := m.requests[key]
	value.Count++
	value.DurationSum += seconds
	for index, upper := range [...]float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 5} {
		if seconds <= upper {
			value.Buckets[index]++
		}
	}
	m.requests[key] = value
	m.mu.Unlock()
}

func (m *httpMetrics) writePrometheus(w http.ResponseWriter) {
	fmt.Fprintf(w, "pie_manager_http_requests_in_flight %d\n", m.inFlight.Load())
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, value := range m.requests {
		labels := fmt.Sprintf("method=%q,route=%q,status=%q", key.Method, key.Route, strconv.Itoa(key.Status))
		fmt.Fprintf(w, "pie_manager_http_requests_total{%s} %d\n", labels, value.Count)
		fmt.Fprintf(w, "pie_manager_http_request_duration_seconds_sum{%s} %g\n", labels, value.DurationSum)
		fmt.Fprintf(w, "pie_manager_http_request_duration_seconds_count{%s} %d\n", labels, value.Count)
		for index, upper := range [...]float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 5} {
			fmt.Fprintf(w, "pie_manager_http_request_duration_seconds_bucket{%s,le=%q} %d\n", labels, strconv.FormatFloat(upper, 'f', -1, 64), value.Buckets[index])
		}
		fmt.Fprintf(w, "pie_manager_http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, value.Count)
	}
}

func withRequestObservability(next http.Handler, metrics *httpMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := normalizedRequestID(r.Header.Get(requestIDHeader))
		r.Header.Set(requestIDHeader, requestID)
		w.Header().Set(requestIDHeader, requestID)
		writer := &observedResponseWriter{ResponseWriter: w, status: http.StatusOK}
		metrics.inFlight.Add(1)
		defer func() {
			metrics.inFlight.Add(-1)
			duration := time.Since(started)
			route := normalizedRoute(r.URL.Path)
			metrics.observe(r.Method, route, writer.status, duration)
			if route != "/healthz" && route != "/readyz" && route != "/metrics" {
				log.Printf("http_request request_id=%s method=%s route=%s status=%d duration_ms=%d bytes=%d", requestID, r.Method, route, writer.status, duration.Milliseconds(), writer.bytes)
			}
		}()
		next.ServeHTTP(writer, r)
	})
}

type observedResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *observedResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedResponseWriter) Write(value []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(value)
	w.bytes += int64(n)
	return n, err
}

func (w *observedResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *observedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (w *observedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func normalizedRequestID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 8 && len(value) <= 128 {
		valid := true
		for _, char := range value {
			if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && !strings.ContainsRune("._:-", char) {
				valid = false
				break
			}
		}
		if valid {
			return value
		}
	}
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err == nil {
		return hex.EncodeToString(buffer)
	}
	return fmt.Sprintf("pie-%d-%d", time.Now().UnixNano(), requestIDSequence.Add(1))
}

func normalizedRoute(path string) string {
	switch {
	case path == "/healthz", path == "/readyz", path == "/metrics", path == "/admin", strings.HasPrefix(path, "/admin/"):
		if strings.HasPrefix(path, "/admin") {
			return "/admin/*"
		}
		return path
	case strings.HasPrefix(path, "/v1/admin/"):
		return "/v1/admin/*"
	case strings.HasPrefix(path, "/v1/control/"):
		return "/v1/control/*"
	case path == "/v1/hooks/users":
		return path
	case path == "/v1/internal/previews/route":
		return path
	case strings.HasPrefix(path, "/v1/integrations/"):
		return "/v1/integrations/:integration/*"
	case strings.HasPrefix(path, "/v1/users/"):
		return "/v1/users/:user/*"
	case strings.HasPrefix(path, "/v1/jobs/"):
		return "/v1/jobs/:job/*"
	default:
		return "other"
	}
}
