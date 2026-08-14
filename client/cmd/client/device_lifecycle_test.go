package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cli-relay/client/internal/devicecredentials"
)

func TestPairDevicePersistsCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent-runtimes/pairings/exchange" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(pairingExchangeResponse{
			DeviceID: "control-device", RuntimeDeviceID: "runtime-device", DeviceName: "테스트 PC",
			WorkspaceID: "workspace", AccessToken: "access", AccessTokenExpiresIn: 900,
			RefreshToken: "refresh", RefreshTokenExpiresIn: 3600, ControlURL: "https://control.example",
			ApplicationID: "pie-canvas", PoolID: "default",
		})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "device.json")
	credentials, err := pairDevice(context.Background(), server.Client(), server.URL, "ABCD-EFGH", "테스트 PC", path)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.DeviceID != "control-device" || credentials.RuntimeDeviceID != "runtime-device" {
		t.Fatalf("credentials=%#v", credentials)
	}
	stored, err := devicecredentials.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "refresh" || stored.Name != "테스트 PC" {
		t.Fatalf("stored=%#v", stored)
	}
}

func TestNormalizeStartArgsSupportsPublicCredentialsFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.json")
	args, gotPath, err := normalizeStartArgs([]string{"--credentials", path, "--listen", "127.0.0.1:19999"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != path {
		t.Fatalf("credentials path=%q", gotPath)
	}
	want := []string{"--device-credentials", path, "--listen", "127.0.0.1:19999", "--control-mode", "device"}
	if len(args) != len(want) {
		t.Fatalf("args=%q", args)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("args=%q", args)
		}
	}
}

func TestManagedRuntimeStartStatusAndStop(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	dir := t.TempDir()
	credentialsPath := filepath.Join(dir, "device.json")
	t.Setenv("PIE_CLIENT_RUNTIME_STATE", filepath.Join(dir, "runtime.json"))
	t.Setenv("PIE_DEVICE_CREDENTIALS", credentialsPath)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runManagedSessionManager([]string{"--listen", address, "--device-credentials", credentialsPath}, credentialsPath)
	}()

	var state runtimeState
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, err = loadRuntimeState(runtimeStatePath(credentialsPath))
		if err == nil && runtimeEndpointAlive(state) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil || !runtimeEndpointAlive(state) {
		t.Fatalf("runtime did not start: state=%#v err=%v", state, err)
	}
	status, err := readRuntimeStatus(credentialsPath)
	if err != nil || !status.Running || status.PID != os.Getpid() {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if err := ensureDeviceRuntimeStopped(credentialsPath); err == nil {
		t.Fatal("running client must reject credential replacement")
	}
	stopped, err := stopManagedRuntime(credentialsPath, 5*time.Second)
	if err != nil || !stopped {
		t.Fatalf("stopped=%v err=%v", stopped, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not stop")
	}
	if err := ensureDeviceRuntimeStopped(credentialsPath); err != nil {
		t.Fatalf("stopped client rejected reconnect: %v", err)
	}
}

func TestDisconnectRemoteDeviceUsesRefreshCredential(t *testing.T) {
	var received string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent-runtimes/disconnect" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		received = body["refreshToken"]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	err := disconnectRemoteDevice(context.Background(), server.Client(), devicecredentials.Credentials{
		BuilderURL: server.URL, RefreshToken: "refresh-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if received != "refresh-secret" {
		t.Fatalf("refresh token=%q", received)
	}
}
