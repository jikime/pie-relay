package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionRequestUsesStdinBodyAndBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sessions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("request=%s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"id":"session-a"}` {
			t.Fatalf("body=%q", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"state":"running"}`))
	}))
	defer server.Close()
	out, status, err := sessionRequest(context.Background(), server.URL, "secret", http.MethodPost, "/v1/sessions", []byte(`{"id":"session-a"}`))
	if err != nil || status != http.StatusCreated || string(out) != `{"state":"running"}` {
		t.Fatalf("out=%q status=%d err=%v", out, status, err)
	}
}

func TestSessionRequestRejectsPathTraversal(t *testing.T) {
	if _, _, err := sessionRequest(context.Background(), "http://127.0.0.1:1", "", http.MethodGet, "/../secret", nil); err == nil {
		t.Fatal("expected invalid path")
	}
}
