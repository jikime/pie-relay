package rooms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPBase(t *testing.T) {
	cases := map[string]string{
		"ws://127.0.0.1:13412/ws/agent":    "http://127.0.0.1:13412",
		"wss://relay.example.com/ws/agent": "https://relay.example.com",
		"http://localhost:8080":            "http://localhost:8080",
		"https://relay.example.com/x?q=1":  "https://relay.example.com",
	}
	for in, want := range cases {
		got, err := HTTPBase(in)
		if err != nil {
			t.Fatalf("HTTPBase(%q) error: %v", in, err)
		}
		if got != want {
			t.Errorf("HTTPBase(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := HTTPBase("ftp://nope"); err == nil {
		t.Error("expected error for non-ws/http scheme")
	}
}

func TestParticipantWSURL(t *testing.T) {
	cases := map[string]string{
		"ws://127.0.0.1:13412/ws/agent":      "ws://127.0.0.1:13412/ws/participant",
		"wss://relay.example.com/ws/agent":   "wss://relay.example.com/ws/participant",
		"http://localhost:8080":              "ws://localhost:8080/ws/participant",
		"https://relay.example.com/ws/agent": "wss://relay.example.com/ws/participant",
	}
	for in, want := range cases {
		got, err := ParticipantWSURL(in)
		if err != nil {
			t.Fatalf("ParticipantWSURL(%q) error: %v", in, err)
		}
		if got != want {
			t.Errorf("ParticipantWSURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateInvite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rooms/invites" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer host-tok" {
			t.Errorf("Authorization = %q, want Bearer host-tok", got)
		}
		_ = json.NewEncoder(w).Encode(CreateInviteResult{Code: "ABCD2345", ExpiresAt: 1234567890})
	}))
	defer srv.Close()

	res, err := CreateInvite(context.Background(), srv.URL, "host-tok")
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if res.Code != "ABCD2345" || res.ExpiresAt != 1234567890 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestCreateInviteForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "participant may not create invites", http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := CreateInvite(context.Background(), srv.URL, "guest-tok"); err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestJoin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rooms/join" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req joinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if req.Code != "ABCD2345" || req.Name != "bob" {
			t.Errorf("unexpected body: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(JoinResult{Token: "participant-jwt", Room: "room-1"})
	}))
	defer srv.Close()

	res, err := Join(context.Background(), srv.URL, "ABCD2345", "bob")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if res.Token != "participant-jwt" || res.Room != "room-1" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// joinRequest mirrors the server's decode shape so the test can assert the
// request body without importing the server package.
type joinRequest struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

func TestJoinExpiredCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := Join(context.Background(), srv.URL, "BADCODE1", "bob"); err == nil {
		t.Fatal("expected error on 401")
	}
}
