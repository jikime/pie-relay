package main

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestObservabilityPreservesSafeRequestIDAndRecordsBoundedRoute(t *testing.T) {
	metrics := newHTTPMetrics()
	handler := withRequestObservability(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(requestIDHeader); got != "browser-request-123" {
			t.Fatalf("request id=%q", got)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}), metrics)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/integrations/private-id/users/private-user/projects/private-project", nil)
	request.Header.Set(requestIDHeader, "browser-request-123")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || recorder.Header().Get(requestIDHeader) != "browser-request-123" {
		t.Fatalf("status=%d requestID=%q", recorder.Code, recorder.Header().Get(requestIDHeader))
	}
	var output bytes.Buffer
	metrics.writePrometheus(&bufferResponseWriter{header: make(http.Header), buffer: &output})
	text := output.String()
	if !strings.Contains(text, `route="/v1/integrations/:integration/*"`) || strings.Contains(text, "private-user") || strings.Contains(text, "private-project") {
		t.Fatalf("metrics=%s", text)
	}
}

func TestRequestObservabilityReplacesUnsafeRequestID(t *testing.T) {
	handler := withRequestObservability(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), newHTTPMetrics())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set(requestIDHeader, "bad id\nvalue")
	handler.ServeHTTP(recorder, request)
	got := recorder.Header().Get(requestIDHeader)
	if got == "bad id\nvalue" || len(got) < 8 {
		t.Fatalf("generated request id=%q", got)
	}
}

func TestObservedResponseWriterSupportsStreaming(t *testing.T) {
	base := &flushResponseWriter{header: make(http.Header)}
	wrapped := &observedResponseWriter{ResponseWriter: base, status: http.StatusOK}
	wrapped.Flush()
	if !base.flushed {
		t.Fatal("underlying flusher was not called")
	}
}

type bufferResponseWriter struct {
	header http.Header
	buffer *bytes.Buffer
}

func (w *bufferResponseWriter) Header() http.Header             { return w.header }
func (w *bufferResponseWriter) WriteHeader(int)                 {}
func (w *bufferResponseWriter) Write(value []byte) (int, error) { return w.buffer.Write(value) }

type flushResponseWriter struct {
	header  http.Header
	flushed bool
}

func (w *flushResponseWriter) Header() http.Header           { return w.header }
func (*flushResponseWriter) WriteHeader(int)                 {}
func (*flushResponseWriter) Write(value []byte) (int, error) { return len(value), nil }
func (w *flushResponseWriter) Flush()                        { w.flushed = true }
func (*flushResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}
