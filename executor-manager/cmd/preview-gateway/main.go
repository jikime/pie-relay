package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/previewtoken"
)

type route struct {
	ID            string `json:"id"`
	Hostname      string `json:"hostname"`
	Backend       string `json:"backend"`
	Port          int    `json:"port"`
	Visibility    string `json:"visibility"`
	AccessVersion int64  `json:"accessVersion,omitempty"`
}

type cachedRoute struct {
	route   route
	found   bool
	expires time.Time
}

type gateway struct {
	managerURL   string
	managerToken string
	accessSecret []byte
	domain       string
	hostPattern  *regexp.Regexp
	client       *http.Client
	transport    http.RoundTripper
	cacheTTL     time.Duration
	sessionTTL   time.Duration
	maxBodyBytes int64
	slots        chan struct{}
	mu           sync.Mutex
	cache        map[string]cachedRoute
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	addr := envOr("PIE_PREVIEW_GATEWAY_ADDR", ":18080")
	managerURL := strings.TrimRight(strings.TrimSpace(os.Getenv("PIE_PREVIEW_MANAGER_URL")), "/")
	managerToken := strings.TrimSpace(os.Getenv("PIE_PREVIEW_GATEWAY_TOKEN"))
	accessSecret := []byte(os.Getenv("PIE_PREVIEW_ACCESS_SECRET"))
	domain := strings.ToLower(strings.TrimSpace(os.Getenv("PIE_PREVIEW_DOMAIN")))
	if managerURL == "" || len(managerToken) < 32 || len(accessSecret) < 32 || domain == "" || strings.ContainsAny(domain, "/:") {
		log.Fatal("PIE_PREVIEW_MANAGER_URL, PIE_PREVIEW_DOMAIN, and 32-byte gateway/access secrets are required")
	}
	parsedManager, err := url.Parse(managerURL)
	if err != nil || parsedManager.Scheme != "http" && parsedManager.Scheme != "https" || parsedManager.Host == "" || parsedManager.User != nil {
		log.Fatal("PIE_PREVIEW_MANAGER_URL must be an HTTP(S) origin")
	}
	g := &gateway{
		managerURL: managerURL, managerToken: managerToken, accessSecret: accessSecret, domain: domain,
		hostPattern: regexp.MustCompile(`^p-[a-z2-7]{26}\.` + regexp.QuoteMeta(domain) + `$`),
		client:      &http.Client{Timeout: 2 * time.Second}, transport: http.DefaultTransport,
		cacheTTL: durationEnv("PIE_PREVIEW_ROUTE_CACHE_TTL", 2*time.Second), sessionTTL: durationEnv("PIE_PREVIEW_SESSION_TTL", 8*time.Hour),
		maxBodyBytes: int64Env("PIE_PREVIEW_MAX_BODY_BYTES", 64<<20), slots: make(chan struct{}, intEnv("PIE_PREVIEW_MAX_CONNECTIONS", 512)), cache: map[string]cachedRoute{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", g.serve)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 0, WriteTimeout: 0, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	log.Printf("Pie Preview Gateway listening on %s for *.%s", addr, domain)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}
}

func (g *gateway) serve(w http.ResponseWriter, r *http.Request) {
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "preview gateway is busy", http.StatusServiceUnavailable)
		return
	}
	hostname := strings.ToLower(r.Host)
	if index := strings.LastIndex(hostname, ":"); index > -1 && !strings.Contains(hostname[index+1:], "]") {
		hostname = hostname[:index]
	}
	if !g.hostPattern.MatchString(hostname) {
		http.NotFound(w, r)
		return
	}
	route, found, err := g.lookup(r.Context(), hostname)
	if err != nil {
		http.Error(w, "preview route temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if !found {
		http.Error(w, "preview is not ready", http.StatusNotFound)
		return
	}
	if token := r.URL.Query().Get("__pie_token"); token != "" {
		claims, verifyErr := previewtoken.Verify(g.accessSecret, token, "launch", time.Now())
		if verifyErr != nil || claims.PreviewID != route.ID || !strings.EqualFold(claims.Hostname, route.Hostname) || claims.AccessVersion != route.AccessVersion {
			http.Error(w, "invalid or expired preview access link", http.StatusUnauthorized)
			return
		}
		sessionToken, issueErr := previewtoken.Issue(g.accessSecret, previewtoken.Claims{Type: "session", PreviewID: route.ID, Hostname: route.Hostname, AccessVersion: route.AccessVersion}, g.sessionTTL)
		if issueErr != nil {
			http.Error(w, "preview authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "__Host-pie_preview", Value: sessionToken, Path: "/", MaxAge: int(g.sessionTTL.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		clean := *r.URL
		query := clean.Query()
		query.Del("__pie_token")
		clean.RawQuery = query.Encode()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, clean.String(), http.StatusSeeOther)
		return
	}
	if route.Visibility != "public" {
		cookie, cookieErr := r.Cookie("__Host-pie_preview")
		if cookieErr != nil {
			http.Error(w, "preview authentication required", http.StatusUnauthorized)
			return
		}
		claims, verifyErr := previewtoken.Verify(g.accessSecret, cookie.Value, "session", time.Now())
		if verifyErr != nil || claims.PreviewID != route.ID || !strings.EqualFold(claims.Hostname, route.Hostname) || claims.AccessVersion != route.AccessVersion {
			http.Error(w, "preview session expired", http.StatusUnauthorized)
			return
		}
	}
	stripCookie(r, "__Host-pie_preview")
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, g.maxBodyBytes)
	}
	for _, header := range []string{"X-Pie-User-Id", "X-Pie-Preview-Id", "X-Pie-Backend"} {
		r.Header.Del(header)
	}
	target, _ := url.Parse("http://" + route.Backend + ":" + strconv.Itoa(route.Port))
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = g.transport
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, proxyErr error) {
		log.Printf("preview proxy %s -> %s:%d: %v", route.ID, route.Backend, route.Port, proxyErr)
		http.Error(w, "preview backend unavailable", http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		response.Header.Set("X-Content-Type-Options", "nosniff")
		stripSetCookie(response.Header, "__Host-pie_preview")
		return nil
	}
	proxy.ServeHTTP(w, r)
}

func (g *gateway) lookup(ctx context.Context, hostname string) (route, bool, error) {
	now := time.Now()
	g.mu.Lock()
	entry, ok := g.cache[hostname]
	if ok && entry.expires.After(now) {
		g.mu.Unlock()
		return entry.route, entry.found, nil
	}
	g.mu.Unlock()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.managerURL+"/v1/internal/previews/route?host="+url.QueryEscape(hostname), nil)
	if err != nil {
		return route{}, false, err
	}
	request.Header.Set("Authorization", "Bearer "+g.managerToken)
	response, err := g.client.Do(request)
	if err != nil {
		return route{}, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		g.store(hostname, route{}, false, now)
		return route{}, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return route{}, false, fmt.Errorf("manager route lookup returned HTTP %d", response.StatusCode)
	}
	var value route
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || value.ID == "" || value.Hostname != hostname || !validBackend(value.Backend) || value.Port < 1024 || value.Port > 65535 || value.Visibility != "private" && value.Visibility != "public" || value.AccessVersion < 0 {
		return route{}, false, errors.New("invalid preview route response")
	}
	g.store(hostname, value, true, now)
	return value, true, nil
}

func (g *gateway) store(hostname string, value route, found bool, now time.Time) {
	g.mu.Lock()
	g.cache[hostname] = cachedRoute{route: value, found: found, expires: now.Add(g.cacheTTL)}
	if len(g.cache) >= 10000 {
		for key, entry := range g.cache {
			if entry.expires.Before(now) {
				delete(g.cache, key)
			}
		}
		for key := range g.cache {
			if len(g.cache) < 10000 {
				break
			}
			delete(g.cache, key)
		}
	}
	g.mu.Unlock()
}

func stripCookie(request *http.Request, name string) {
	cookies := request.Cookies()
	request.Header.Del("Cookie")
	for _, cookie := range cookies {
		if cookie.Name != name {
			request.AddCookie(cookie)
		}
	}
}

func stripSetCookie(header http.Header, name string) {
	values := header.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}
	header.Del("Set-Cookie")
	for _, value := range values {
		key, _, found := strings.Cut(value, "=")
		if found && strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		header.Add("Set-Cookie", value)
	}
}

func validBackend(value string) bool {
	if value == "" || len(value) > 63 || strings.ContainsAny(value, ".:/\\") {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		log.Fatalf("%s must be a positive duration", name)
	}
	return parsed
}

func intEnv(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		log.Fatalf("%s must be a positive integer", name)
	}
	return parsed
}

func int64Env(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 {
		log.Fatalf("%s must be a positive integer", name)
	}
	return parsed
}
