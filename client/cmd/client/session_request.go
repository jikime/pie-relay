package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// runSessionRequest is the narrow control helper used by the host Manager via
// `docker exec -i`. The request JSON (including the short-lived Relay token)
// travels on stdin, so it is not exposed in process arguments or Docker events.
func runSessionRequest(args []string) {
	fs := flag.NewFlagSet("sessions request", flag.ExitOnError)
	base := fs.String("url", envOr("PIE_SESSION_MANAGER_URL", "http://127.0.0.1:19091"), "session manager origin")
	method := fs.String("method", http.MethodGet, "HTTP method")
	path := fs.String("path", "/v1/sessions", "absolute API path")
	token := fs.String("auth-token", os.Getenv("PIE_SESSION_MANAGER_TOKEN"), "session manager bearer token")
	timeout := fs.Duration("timeout", 15*time.Second, "request timeout")
	_ = fs.Parse(args)

	body, err := io.ReadAll(io.LimitReader(os.Stdin, 128<<10))
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	out, status, err := sessionRequest(ctx, *base, *token, *method, *path, body)
	if err != nil {
		log.Fatal(err)
	}
	if status < 200 || status >= 300 {
		log.Fatalf("session manager HTTP %d: %s", status, strings.TrimSpace(string(out)))
	}
	_, _ = os.Stdout.Write(out)
}

func sessionRequest(ctx context.Context, base, token, method, path string, body []byte) ([]byte, int, error) {
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return nil, 0, errors.New("session manager URL must be an http origin")
	}
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return nil, 0, errors.New("invalid session manager path")
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method != http.MethodGet && method != http.MethodPost && method != http.MethodDelete {
		return nil, 0, errors.New("unsupported session manager method")
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String()+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("session manager request: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return out, resp.StatusCode, err
}
