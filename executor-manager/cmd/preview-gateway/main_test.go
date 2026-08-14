package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/previewtoken"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestPrivatePreviewExchangesLaunchTokenAndProxiesSession(t *testing.T) {
	hostname := "p-aaaaaaaaaaaaaaaaaaaaaaaaaa.preview.kroot.io"
	managerToken := strings.Repeat("m", 32)
	manager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+managerToken || r.URL.Query().Get("host") != hostname {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"preview-a","hostname":"`+hostname+`","backend":"preview-backend-a","port":20000,"visibility":"private","accessVersion":7}`)
	}))
	defer manager.Close()
	secret := []byte(strings.Repeat("s", 32))
	g := &gateway{
		managerURL: manager.URL, managerToken: managerToken, accessSecret: secret, domain: "preview.kroot.io",
		hostPattern: regexp.MustCompile(`^p-[a-z2-7]{26}\.preview\.kroot\.io$`), client: manager.Client(),
		transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Host != "preview-backend-a:20000" || request.Header.Get("X-Pie-Preview-Id") != "" || request.Header.Get("Cookie") != "app_session=preserved" {
				t.Fatalf("proxied request=%s headers=%v", request.URL, request.Header)
			}
			header := make(http.Header)
			header.Add("Set-Cookie", "__Host-pie_preview=must-not-overwrite; Path=/; Secure")
			header.Add("Set-Cookie", "app_session=updated; Path=/; Secure")
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader("preview-ok")), Request: request}, nil
		}),
		cacheTTL: time.Second, sessionTTL: time.Hour, maxBodyBytes: 1 << 20, slots: make(chan struct{}, 4), cache: map[string]cachedRoute{},
	}
	staleLaunch, err := previewtoken.Issue(secret, previewtoken.Claims{Type: "launch", PreviewID: "preview-a", Hostname: hostname, AccessVersion: 6}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	staleRequest := httptest.NewRequest(http.MethodGet, "https://"+hostname+"/?__pie_token="+staleLaunch, nil)
	staleResult := httptest.NewRecorder()
	g.serve(staleResult, staleRequest)
	if staleResult.Code != http.StatusUnauthorized {
		t.Fatalf("stale launch status=%d body=%s", staleResult.Code, staleResult.Body.String())
	}
	launch, err := previewtoken.Issue(secret, previewtoken.Claims{Type: "launch", PreviewID: "preview-a", Hostname: hostname, AccessVersion: 7}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRequest(http.MethodGet, "https://"+hostname+"/?__pie_token="+launch, nil)
	firstResult := httptest.NewRecorder()
	g.serve(firstResult, first)
	if firstResult.Code != http.StatusSeeOther {
		t.Fatalf("launch status=%d body=%s", firstResult.Code, firstResult.Body.String())
	}
	response := firstResult.Result()
	cookies := response.Cookies()
	if len(cookies) != 1 || cookies[0].Name != "__Host-pie_preview" || !cookies[0].Secure || !cookies[0].HttpOnly || firstResult.Header().Get("Location") != "https://"+hostname+"/" {
		t.Fatalf("cookies=%+v location=%q", cookies, firstResult.Header().Get("Location"))
	}
	request := httptest.NewRequest(http.MethodGet, "https://"+hostname+"/hello", nil)
	request.AddCookie(cookies[0])
	request.AddCookie(&http.Cookie{Name: "app_session", Value: "preserved"})
	result := httptest.NewRecorder()
	g.serve(result, request)
	if result.Code != http.StatusOK || result.Body.String() != "preview-ok" {
		t.Fatalf("proxy status=%d body=%q", result.Code, result.Body.String())
	}
	if values := result.Header().Values("Set-Cookie"); len(values) != 1 || !strings.HasPrefix(values[0], "app_session=") {
		t.Fatalf("backend was allowed to replace the gateway cookie: %v", values)
	}
}

func TestPrivatePreviewRejectsMissingSession(t *testing.T) {
	hostname := "p-bbbbbbbbbbbbbbbbbbbbbbbbbb.preview.kroot.io"
	g := &gateway{
		accessSecret: []byte(strings.Repeat("s", 32)), domain: "preview.kroot.io",
		hostPattern: regexp.MustCompile(`^p-[a-z2-7]{26}\.preview\.kroot\.io$`),
		cacheTTL:    time.Second, sessionTTL: time.Hour, maxBodyBytes: 1 << 20, slots: make(chan struct{}, 1),
		cache: map[string]cachedRoute{hostname: {route: route{ID: "preview-b", Hostname: hostname, Backend: "preview-backend-b", Port: 20000, Visibility: "private"}, found: true, expires: time.Now().Add(time.Minute)}},
	}
	request := httptest.NewRequest(http.MethodGet, "https://"+hostname+"/", nil)
	result := httptest.NewRecorder()
	g.serve(result, request)
	if result.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%q", result.Code, result.Body.String())
	}
}
