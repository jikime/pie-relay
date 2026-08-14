package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestEnroll_MintsHostToken walks the happy path: a caller who knows the enroll
// secret exchanges it for a host token scoped to the requested room, and the
// returned token verifies to role=host / that room.
func TestEnroll_MintsHostToken(t *testing.T) {
	auth := JWTAuth{Secret: []byte("jwt-secret")}
	en := NewEnroller(auth, "op-secret", time.Hour, false)

	body, _ := json.Marshal(enrollRequest{Secret: "op-secret", Room: "room_42", Name: "Alice"})
	rec := httptest.NewRecorder()
	en.handleEnroll(rec, httptest.NewRequest(http.MethodPost, "/host/enroll", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: got %d, body=%s", rec.Code, rec.Body)
	}
	var resp enrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode enroll: %v", err)
	}
	if resp.Room != "room_42" {
		t.Fatalf("room = %q, want room_42", resp.Room)
	}
	if resp.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expiresAt %d should be in the future", resp.ExpiresAt)
	}

	id, err := auth.Verify(resp.Token)
	if err != nil {
		t.Fatalf("host token should verify: %v", err)
	}
	if id.Room != "room_42" || id.Role != RoleHost {
		t.Fatalf("identity %+v, want room_42/host", id)
	}
	if id.UserID != "alice" {
		t.Fatalf("sub = %q, want sanitized 'alice'", id.UserID)
	}
}

// TestEnroll_WrongSecret confirms a mismatched secret is rejected with 401 and
// mints nothing. httptest.NewRequest defaults RemoteAddr to a non-loopback
// TEST-NET address (192.0.2.1), so the secret gate applies.
func TestEnroll_WrongSecret(t *testing.T) {
	auth := JWTAuth{Secret: []byte("jwt-secret")}
	en := NewEnroller(auth, "op-secret", time.Hour, false)

	body, _ := json.Marshal(enrollRequest{Secret: "nope"})
	rec := httptest.NewRecorder()
	en.handleEnroll(rec, httptest.NewRequest(http.MethodPost, "/host/enroll", bytes.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret should be 401, got %d", rec.Code)
	}
}

// TestEnroll_LoopbackNoSecret confirms a same-PC (loopback) caller may enroll
// WITHOUT presenting a secret — even when a secret IS configured — because
// loopback access is already the trust boundary (personal-use relay=host=PC).
func TestEnroll_LoopbackNoSecret(t *testing.T) {
	auth := JWTAuth{Secret: []byte("jwt-secret")}
	// A secret is configured, but a loopback caller bypasses it entirely.
	en := NewEnroller(auth, "op-secret", time.Hour, true)

	for _, addr := range []string{"127.0.0.1:54321", "[::1]:54321"} {
		body, _ := json.Marshal(enrollRequest{Room: "room_local"}) // no secret
		req := httptest.NewRequest(http.MethodPost, "/host/enroll", bytes.NewReader(body))
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		en.handleEnroll(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("loopback %s enroll without secret should be 200, got %d body=%s", addr, rec.Code, rec.Body)
		}
		var resp enrollResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		id, err := auth.Verify(resp.Token)
		if err != nil || id.Role != RoleHost || id.Room != "room_local" {
			t.Fatalf("loopback %s should mint a host token for room_local, got %+v err=%v", addr, id, err)
		}
	}
}

// TestEnroll_LoopbackDisabledStillWorks confirms loopback enroll works even when
// NO secret is configured (the common personal-use case: relay started with no
// HOST_ENROLL_SECRET, yet the local GUI can still make a room).
func TestEnroll_LoopbackDisabledStillWorks(t *testing.T) {
	auth := JWTAuth{Secret: []byte("jwt-secret")}
	en := NewEnroller(auth, "", time.Hour, true) // enrollment "disabled" for the public

	body, _ := json.Marshal(enrollRequest{}) // no secret, no room
	req := httptest.NewRequest(http.MethodPost, "/host/enroll", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:44444"
	rec := httptest.NewRecorder()
	en.handleEnroll(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback enroll with no secret configured should be 200, got %d", rec.Code)
	}
}

// TestEnroll_LoopbackRequiresExplicitOptIn covers the reverse-proxy boundary:
// a public request forwarded by a same-machine proxy also has a loopback
// RemoteAddr, so loopback alone must never bypass HOST_ENROLL_SECRET.
func TestEnroll_LoopbackRequiresExplicitOptIn(t *testing.T) {
	auth := JWTAuth{Secret: []byte("jwt-secret")}
	en := NewEnroller(auth, "op-secret", time.Hour, false)
	body, _ := json.Marshal(enrollRequest{})
	req := httptest.NewRequest(http.MethodPost, "/host/enroll", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:44444"
	rec := httptest.NewRecorder()
	en.handleEnroll(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("loopback without explicit opt-in should require the secret, got %d", rec.Code)
	}
}

// TestEnroll_NonLoopbackNoSecretRejected confirms a remote caller that omits the
// secret is still rejected — the loopback opening does NOT weaken the public
// path. Wrong/absent secret with a secret configured → 401.
func TestEnroll_NonLoopbackNoSecretRejected(t *testing.T) {
	auth := JWTAuth{Secret: []byte("jwt-secret")}
	en := NewEnroller(auth, "op-secret", time.Hour, false)

	body, _ := json.Marshal(enrollRequest{}) // no secret
	req := httptest.NewRequest(http.MethodPost, "/host/enroll", bytes.NewReader(body))
	req.RemoteAddr = "203.0.113.7:9999" // non-loopback (TEST-NET-3)
	rec := httptest.NewRecorder()
	en.handleEnroll(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-loopback without secret should be 401, got %d", rec.Code)
	}

	// And with no secret configured at all, a remote caller gets 503 (disabled).
	enOff := NewEnroller(auth, "", time.Hour, false)
	req2 := httptest.NewRequest(http.MethodPost, "/host/enroll", bytes.NewReader(body))
	req2.RemoteAddr = "203.0.113.7:9999"
	rec2 := httptest.NewRecorder()
	enOff.handleEnroll(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-loopback with no secret configured should be 503, got %d", rec2.Code)
	}
}

// TestEnroll_Disabled confirms that with no configured secret the endpoint is
// unavailable (503) — the operator did not opt in to enrollment.
func TestEnroll_Disabled(t *testing.T) {
	auth := JWTAuth{Secret: []byte("jwt-secret")}
	en := NewEnroller(auth, "", time.Hour, false) // enrollment disabled

	body, _ := json.Marshal(enrollRequest{Secret: ""})
	rec := httptest.NewRecorder()
	en.handleEnroll(rec, httptest.NewRequest(http.MethodPost, "/host/enroll", bytes.NewReader(body)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled enroll should be 503, got %d", rec.Code)
	}
}

// TestEnroll_GeneratesRoomAndName confirms that an omitted room gets an
// "r-"+rand id and an omitted name defaults to a "host" sub.
func TestEnroll_GeneratesRoomAndName(t *testing.T) {
	auth := JWTAuth{Secret: []byte("jwt-secret")}
	en := NewEnroller(auth, "op-secret", time.Hour, false)

	body, _ := json.Marshal(enrollRequest{Secret: "op-secret"}) // no room, no name
	rec := httptest.NewRecorder()
	en.handleEnroll(rec, httptest.NewRequest(http.MethodPost, "/host/enroll", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: got %d, body=%s", rec.Code, rec.Body)
	}
	var resp enrollResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.HasPrefix(resp.Room, hostRoomPrefix) || len(resp.Room) != len(hostRoomPrefix)+hostRoomLen {
		t.Fatalf("generated room %q, want %s + %d chars", resp.Room, hostRoomPrefix, hostRoomLen)
	}
	id, err := auth.Verify(resp.Token)
	if err != nil {
		t.Fatalf("token should verify: %v", err)
	}
	if id.UserID != "host" {
		t.Fatalf("default sub = %q, want 'host'", id.UserID)
	}
	if id.Room != resp.Room {
		t.Fatalf("token room %q != response room %q", id.Room, resp.Room)
	}
}

// TestEnroll_DefaultTTL confirms a non-positive ttl falls back to the 30-day
// default rather than minting an already-expired token.
func TestEnroll_DefaultTTL(t *testing.T) {
	auth := JWTAuth{Secret: []byte("jwt-secret")}
	en := NewEnroller(auth, "op-secret", 0, false) // 0 → defaultHostEnrollTTL
	if en.ttl != defaultHostEnrollTTL {
		t.Fatalf("ttl = %v, want default %v", en.ttl, defaultHostEnrollTTL)
	}
}

func TestEnrollRejectsOversizedAndUnknownBodies(t *testing.T) {
	auth := JWTAuth{Secret: []byte("jwt-secret")}
	enroller := NewEnroller(auth, "op-secret", time.Hour, false)
	for name, body := range map[string]string{
		"oversized":     `{"secret":"op-secret","room":"` + strings.Repeat("x", 9<<10) + `"}`,
		"unknown field": `{"secret":"op-secret","unexpected":true}`,
		"trailing JSON": `{"secret":"op-secret"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/host/enroll", strings.NewReader(body))
			req.RemoteAddr = "192.0.2.10:1234"
			rec := httptest.NewRecorder()
			enroller.handleEnroll(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestEnroll_CORSPreflight confirms the webview preflight (OPTIONS) on
// /host/enroll answers 204 with permissive CORS headers so the browser-engine
// fetch survives.
func TestEnroll_CORSPreflight(t *testing.T) {
	auth := JWTAuth{Secret: []byte("s")}
	srv := NewServerOpts(ServerOptions{
		AgentAuth: auth, ParticipantAuth: auth,
		Enroller: NewEnroller(auth, "op-secret", time.Hour, false),
	})

	req := httptest.NewRequest(http.MethodOptions, "/host/enroll", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Fatal("Allow-Headers missing")
	}
}
