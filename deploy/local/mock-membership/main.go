package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type principal struct {
	Active         bool     `json:"active"`
	Sub            string   `json:"sub,omitempty"`
	OrganizationID string   `json:"organizationId,omitempty"`
	Roles          []string `json:"roles,omitempty"`
	Scope          string   `json:"scope,omitempty"`
}

var localTokens = map[string]principal{
	"pat-local-admin":        {Active: true, Sub: "local-admin", OrganizationID: "org-local", Roles: []string{"pie-admin"}},
	"pat-local-operator":     {Active: true, Sub: "local-operator", OrganizationID: "org-local", Scope: "pie:operate"},
	"pat-local-viewer":       {Active: true, Sub: "local-viewer", OrganizationID: "org-local", Scope: "pie:admin:view"},
	"pat-local-user":         {Active: true, Sub: "local-user", OrganizationID: "org-local"},
	"pat-pie-canvas-agent":   {Active: true, Sub: "pie-canvas-agent", OrganizationID: "org-local", Scope: "pie:operate"},
	"pat-kroot-studio-agent": {Active: true, Sub: "kroot-studio-agent", OrganizationID: "org-kroot-studio", Scope: "pie:operate"},
	"pat-local-guest":        {Active: true, Sub: "local-guest", OrganizationID: "org-local"},
	"pat-local-inactive":     {Active: false},
	"pat-local-slow":         {Active: true, Sub: "local-slow", OrganizationID: "org-local"},
}

type membershipServer struct {
	clientID     string
	clientSecret string
	controlToken string
	slowDelay    time.Duration
	mu           sync.RWMutex
	revoked      map[string]bool
}

func (s *membershipServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/oauth/introspect", s.introspect)
	mux.HandleFunc("/v1/tokens/revocation", s.revocation)
	return mux
}

func (s *membershipServer) introspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.validClient(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="pie-local-membership"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(r.Form.Get("token"))
	if token == "pat-local-slow" {
		timer := time.NewTimer(s.slowDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
	}
	s.mu.RLock()
	revoked := s.revoked[token]
	s.mu.RUnlock()
	value, ok := localTokens[token]
	if !ok || revoked {
		value = principal{Active: false}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func (s *membershipServer) revocation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !constantEqual(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), s.controlToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	defer r.Body.Close()
	var request struct {
		Token   string `json:"token"`
		Revoked bool   `json:"revoked"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || request.Token == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if _, exists := localTokens[request.Token]; !exists {
		http.Error(w, "unknown local token", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	if request.Revoked {
		s.revoked[request.Token] = true
	} else {
		delete(s.revoked, request.Token)
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "revoked": request.Revoked})
}

func (s *membershipServer) validClient(r *http.Request) bool {
	providedID, providedSecret, ok := r.BasicAuth()
	return ok && constantEqual(providedID, s.clientID) && constantEqual(providedSecret, s.clientSecret)
}

func constantEqual(provided, expected string) bool {
	return expected != "" && len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func main() {
	addr := flag.String("addr", ":18080", "listen address")
	healthcheck := flag.String("healthcheck", "", "probe URL and exit")
	flag.Parse()
	if *healthcheck != "" {
		client := &http.Client{Timeout: 2 * time.Second}
		response, err := client.Get(*healthcheck)
		if err != nil {
			log.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			log.Fatalf("healthcheck status %d", response.StatusCode)
		}
		return
	}
	clientID := strings.TrimSpace(os.Getenv("MOCK_AUTH_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("MOCK_AUTH_CLIENT_SECRET"))
	controlToken := strings.TrimSpace(os.Getenv("MOCK_AUTH_CONTROL_TOKEN"))
	if clientID == "" || len(clientSecret) < 32 || len(controlToken) < 32 {
		log.Fatal("MOCK_AUTH_CLIENT_ID and 32-byte MOCK_AUTH_CLIENT_SECRET/MOCK_AUTH_CONTROL_TOKEN are required")
	}
	delay := 3 * time.Second
	if raw := strings.TrimSpace(os.Getenv("MOCK_AUTH_SLOW_DELAY")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			log.Fatal("MOCK_AUTH_SLOW_DELAY must be a positive duration")
		}
		delay = parsed
	}
	app := &membershipServer{clientID: clientID, clientSecret: clientSecret, controlToken: controlToken, slowDelay: delay, revoked: map[string]bool{}}
	server := &http.Server{Addr: *addr, Handler: app.routes(), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	log.Printf("Pie local membership mock listening on %s", *addr)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}
}
