package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cli-relay/client/internal/devicecredentials"
)

func TestDeviceCredentialSourceRotatesAndPersistsRefreshToken(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent-runtimes/token/refresh" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		call := calls.Add(1)
		if call == 1 && body["refreshToken"] != "refresh-1" {
			t.Fatalf("first refresh token = %q", body["refreshToken"])
		}
		if call == 2 && body["refreshToken"] != "refresh-2" {
			t.Fatalf("rotated refresh token = %q", body["refreshToken"])
		}
		_ = json.NewEncoder(w).Encode(pairingExchangeResponse{
			AccessToken: fmt.Sprintf("access-%d", call), AccessTokenExpiresIn: 900,
			RefreshToken: fmt.Sprintf("refresh-%d", call+1), RefreshTokenExpiresIn: 3600,
			ControlURL: "https://control.example",
		})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "device.json")
	if err := devicecredentials.Save(path, devicecredentials.Credentials{
		BuilderURL: server.URL, ControlURL: "https://control.example", DeviceID: "device-a",
		WorkspaceID: "workspace-a", RefreshToken: "refresh-1", AccessTokenExpiresAt: time.Now().Add(-time.Minute),
		RefreshTokenExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	source, err := loadDeviceCredentialSource(path)
	if err != nil {
		t.Fatal(err)
	}
	if token, err := source.Token(context.Background(), false); err != nil || token != "access-1" {
		t.Fatalf("first token=%q err=%v", token, err)
	}
	if token, err := source.Token(context.Background(), false); err != nil || token != "access-1" || calls.Load() != 1 {
		t.Fatalf("cached token=%q calls=%d err=%v", token, calls.Load(), err)
	}
	if token, err := source.Token(context.Background(), true); err != nil || token != "access-2" || calls.Load() != 2 {
		t.Fatalf("forced token=%q calls=%d err=%v", token, calls.Load(), err)
	}
	stored, err := devicecredentials.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "refresh-3" || stored.AccessToken != "access-2" {
		t.Fatalf("stored credentials=%#v", stored)
	}
}

func TestPairingServiceURLPrefersProductNeutralEnvironment(t *testing.T) {
	t.Setenv("PIE_CANVAS_URL", "https://legacy-canvas.example")
	t.Setenv("PIE_PAIRING_URL", " https://studio.example ")
	if got := pairingServiceURL(); got != "https://studio.example" {
		t.Fatalf("pairingServiceURL() = %q", got)
	}
}

func TestPairingServiceURLKeepsLegacyCanvasFallback(t *testing.T) {
	t.Setenv("PIE_PAIRING_URL", "")
	t.Setenv("PIE_CANVAS_URL", " https://canvas.example ")
	if got := pairingServiceURL(); got != "https://canvas.example" {
		t.Fatalf("pairingServiceURL() = %q", got)
	}
}

func TestRequestPairingJSONExplainsWrongManagerURL(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	err := requestPairingJSON(
		context.Background(),
		server.Client(),
		server.URL+"/api/agent-runtimes/pairings/exchange",
		map[string]string{"code": "ABCD-EFGH"},
		&pairingExchangeResponse{},
	)
	if err == nil || !strings.Contains(err.Error(), "Relay/Manager가 아니라") {
		t.Fatalf("requestPairingJSON() error = %v", err)
	}
}
