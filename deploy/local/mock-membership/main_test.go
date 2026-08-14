package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestMembership() *membershipServer {
	return &membershipServer{
		clientID:     "pie-local",
		clientSecret: "01234567890123456789012345678901",
		controlToken: "abcdefghijklmnopqrstuvwxyz012345",
		slowDelay:    time.Millisecond,
		revoked:      map[string]bool{},
	}
}

func introspectRequest(t *testing.T, handler http.Handler, token string) principal {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/oauth/introspect", strings.NewReader(url.Values{"token": {token}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth("pie-local", "01234567890123456789012345678901")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var value principal
	if err := json.NewDecoder(recorder.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestIntrospectionAndRevocation(t *testing.T) {
	server := newTestMembership()
	handler := server.routes()
	if value := introspectRequest(t, handler, "pat-local-user"); !value.Active || value.Sub != "local-user" {
		t.Fatalf("principal=%+v", value)
	}
	if value := introspectRequest(t, handler, "pat-pie-canvas-agent"); !value.Active || value.Sub != "pie-canvas-agent" || value.Scope != "pie:operate" {
		t.Fatalf("Pie Canvas Agent principal=%+v", value)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tokens/revocation", strings.NewReader(`{"token":"pat-local-user","revoked":true}`))
	request.Header.Set("Authorization", "Bearer abcdefghijklmnopqrstuvwxyz012345")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if value := introspectRequest(t, handler, "pat-local-user"); value.Active {
		t.Fatalf("revoked token remained active: %+v", value)
	}
}

func TestIntrospectionRequiresClientAuthentication(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/oauth/introspect", strings.NewReader("token=pat-local-user"))
	recorder := httptest.NewRecorder()
	newTestMembership().routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", recorder.Code)
	}
}
