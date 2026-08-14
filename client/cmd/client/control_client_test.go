package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"cli-relay/client/internal/ptyagent"
	"cli-relay/client/internal/sessionmanager"
)

func TestRelayAgentURL(t *testing.T) {
	values := map[string]string{
		"http://127.0.0.1:13412":                "ws://127.0.0.1:13412/ws/agent",
		"https://relay.cookai.dev/":             "wss://relay.cookai.dev/ws/agent",
		"wss://relay.cookai.dev/ws/agent":       "wss://relay.cookai.dev/ws/agent",
		"https://edge.cookai.dev/relay-service": "wss://edge.cookai.dev/relay-service/ws/agent",
	}
	for input, want := range values {
		got, err := relayAgentURL(input)
		if err != nil || got != want {
			t.Fatalf("relayAgentURL(%q)=%q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := relayAgentURL("file:///tmp/relay"); err == nil {
		t.Fatal("accepted unsupported relay URL")
	}
}

func TestDeviceHeartbeatImmediatelyMarksHostOnline(t *testing.T) {
	var received struct {
		ObservedState   string            `json:"observedState"`
		ClientConnected bool              `json:"clientConnected"`
		ActiveSessions  int               `json:"activeSessions"`
		Metadata        map[string]string `json:"metadata"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/control/devices/device-a/heartbeat" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()
	client, err := newDeviceControlClient(server.URL, "pat", "device-a", "Linux A")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.heartbeat(context.Background(), 0, true, false); err != nil {
		t.Fatal(err)
	}
	if received.ObservedState != "online" || !received.ClientConnected || received.ActiveSessions != 0 {
		t.Fatalf("heartbeat=%+v", received)
	}
	if received.Metadata[aiRuntimeMetadataKey] == "" {
		t.Fatalf("heartbeat did not include AI runtime readiness: %+v", received.Metadata)
	}
}

func TestDeviceControlClientReconcilesAssignedSession(t *testing.T) {
	var mu sync.Mutex
	reported := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pat" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/control/devices/device-a/sessions":
			_ = json.NewEncoder(w).Encode([]desiredDeviceSession{{ID: "session-a", DeviceID: "device-a", ExecutionTarget: "local", Status: "starting"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/control/sessions/session-a/credential":
			_ = json.NewEncoder(w).Encode(deviceSessionCredential{Token: "host-jwt", RelayURL: "https://relay.cookai.dev"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/control/devices/device-a/sessions/session-a/status":
			var body struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			reported = body.Status
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"status": body.Status})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 1)
	runner := func(ctx context.Context, relayURL, _ string, token string, options ptyagent.Options) error {
		if relayURL != "wss://relay.cookai.dev/ws/agent" || token != "host-jwt" {
			t.Errorf("runner relay=%q token=%q", relayURL, token)
		}
		if options.OnRelayState != nil {
			options.OnRelayState("connected")
		}
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	manager := sessionmanager.New(ctx, "/tmp/pty-host.mjs", 4, runner)
	client, err := newDeviceControlClient(server.URL, "pat", "device-a", "Linux A")
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileDeviceSessions(ctx, client, manager); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("assigned session was not started")
	}
	// manager.Start 직후가 아니라 Relay 연결 콜백을 관찰한 다음 reconcile에서
	// ready를 보고해야 participant 요청이 host보다 먼저 전송되지 않는다.
	if err := reconcileDeviceSessions(ctx, client, manager); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := reported
	mu.Unlock()
	if got != "ready" {
		t.Fatalf("reported status=%q", got)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	if err := manager.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceReconcileReplacesFencedRelaySession(t *testing.T) {
	var mu sync.Mutex
	starts := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/control/devices/device-a/sessions":
			_ = json.NewEncoder(w).Encode([]desiredDeviceSession{{ID: "session-a", DeviceID: "device-a", ExecutionTarget: "local", Status: "reconnecting"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/control/sessions/session-a/credential":
			_ = json.NewEncoder(w).Encode(deviceSessionCredential{Token: "generation-2", RelayURL: "https://relay-b.cookai.dev"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/control/devices/device-a/sessions/session-a/status":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runner := func(ctx context.Context, _ string, _ string, token string, _ ptyagent.Options) error {
		mu.Lock()
		starts = append(starts, token)
		mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
	manager := sessionmanager.New(context.Background(), "/tmp/pty-host.mjs", 2, runner)
	if _, _, err := manager.Start(sessionmanager.Config{ID: "session-a", RelayURL: "wss://relay-a.cookai.dev/ws/agent", Token: "generation-1"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		count := len(starts)
		mu.Unlock()
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial session did not start")
		}
		time.Sleep(time.Millisecond)
	}
	control, err := newDeviceControlClient(server.URL, "pat", "device-a", "Linux A")
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileDeviceSessions(context.Background(), control, manager); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		mu.Lock()
		got := append([]string(nil), starts...)
		mu.Unlock()
		if len(got) == 2 {
			if got[0] != "generation-1" || got[1] != "generation-2" {
				t.Fatalf("tokens=%v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fenced session was not replaced: starts=%v", got)
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
