package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPRelayControlAuthenticatesAndTargetsConnection(t *testing.T) {
	var method, path, authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, authorization = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := HTTPRelayControl{Token: "control-secret"}
	if err := client.DisconnectConnection(context.Background(), server.URL+"/ignored/path", "connection-a"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodDelete || path != "/v1/control/connections/connection-a" || authorization != "Bearer control-secret" {
		t.Fatalf("method=%s path=%s authorization=%s", method, path, authorization)
	}
}

func TestHTTPRelayControlSetsDriver(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer control-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["room"] != "owner" || body["deviceId"] != "device-a" || body["sessionId"] != "session-a" || body["userId"] != "viewer" || body["relayGeneration"] != float64(4) {
			t.Fatalf("body=%v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"userId": "viewer", "generation": 3})
	}))
	defer server.Close()
	result, err := (HTTPRelayControl{Token: "control-secret"}).SetDriver(context.Background(), server.URL, "owner", "device-a", "session-a", "viewer", 4)
	if err != nil || result.UserID != "viewer" || result.Generation != 3 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
