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

func newTestInviter(t *testing.T) (*Inviter, JWTAuth) {
	t.Helper()
	auth := JWTAuth{Secret: []byte("test-secret")}
	return NewInviter(auth), auth
}

// TestInvite_CreateThenJoin walks the happy path: a host mints an invite, a
// guest joins with the code, and the returned token verifies to a guest
// identity scoped to the host's room with role=participant and ~12h exp.
func TestInvite_CreateThenJoin(t *testing.T) {
	in, auth := newTestInviter(t)
	hostToken, err := auth.Mint("alice", "room_42", RoleHost, "", time.Hour)
	if err != nil {
		t.Fatalf("mint host: %v", err)
	}

	// Create invite.
	req := httptest.NewRequest(http.MethodPost, "/rooms/invites", nil)
	req.Header.Set("Authorization", "Bearer "+hostToken)
	rec := httptest.NewRecorder()
	in.handleCreateInvite(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create invite: got %d, body=%s", rec.Code, rec.Body)
	}
	var created createInviteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode invite: %v", err)
	}
	if len(created.Code) != inviteCodeLen {
		t.Fatalf("code %q should be %d chars", created.Code, inviteCodeLen)
	}

	// Join with the code.
	body, _ := json.Marshal(joinRequest{Code: created.Code, Name: "Bob"})
	jreq := httptest.NewRequest(http.MethodPost, "/rooms/join", bytes.NewReader(body))
	jrec := httptest.NewRecorder()
	in.handleJoin(jrec, jreq)
	if jrec.Code != http.StatusOK {
		t.Fatalf("join: got %d, body=%s", jrec.Code, jrec.Body)
	}
	var joined joinResponse
	if err := json.Unmarshal(jrec.Body.Bytes(), &joined); err != nil {
		t.Fatalf("decode join: %v", err)
	}
	if joined.Room != "room_42" {
		t.Fatalf("guest joined room %q, want room_42", joined.Room)
	}

	// The minted token must verify to a scoped guest identity.
	id, err := auth.Verify(joined.Token)
	if err != nil {
		t.Fatalf("guest token should verify: %v", err)
	}
	if id.Room != "room_42" || id.Role != RoleParticipant {
		t.Fatalf("guest identity %+v, want room_42/participant", id)
	}
	if !strings.HasPrefix(id.UserID, "guest:bob-") {
		t.Fatalf("guest sub %q, want guest:bob-<rand>", id.UserID)
	}
}

// TestInvite_AccessGradeStoredAndMinted confirms a "view" invite stores the
// grade and the guest token minted from it carries access=view.
func TestInvite_AccessGradeStoredAndMinted(t *testing.T) {
	in, auth := newTestInviter(t)
	hostToken, _ := auth.Mint("alice", "room_42", RoleHost, "", time.Hour)

	// Create a VIEW invite.
	cbody, _ := json.Marshal(createInviteRequest{Access: AccessView})
	req := httptest.NewRequest(http.MethodPost, "/rooms/invites", bytes.NewReader(cbody))
	req.Header.Set("Authorization", "Bearer "+hostToken)
	rec := httptest.NewRecorder()
	in.handleCreateInvite(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create invite: got %d, body=%s", rec.Code, rec.Body)
	}
	var created createInviteResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Access != AccessView {
		t.Fatalf("response access = %q, want view", created.Access)
	}

	// Join → the guest token must carry access=view.
	jbody, _ := json.Marshal(joinRequest{Code: created.Code, Name: "Eve"})
	jrec := httptest.NewRecorder()
	in.handleJoin(jrec, httptest.NewRequest(http.MethodPost, "/rooms/join", bytes.NewReader(jbody)))
	var joined joinResponse
	_ = json.Unmarshal(jrec.Body.Bytes(), &joined)
	id, err := auth.Verify(joined.Token)
	if err != nil {
		t.Fatalf("guest token should verify: %v", err)
	}
	if id.Access != AccessView {
		t.Fatalf("guest access = %q, want view", id.Access)
	}
}

// TestInvite_DefaultAccessView confirms an invite created with no body fails
// safe to view-only. Control must always be requested explicitly.
func TestInvite_DefaultAccessView(t *testing.T) {
	in, auth := newTestInviter(t)
	hostToken, _ := auth.Mint("alice", "room_42", RoleHost, "", time.Hour)

	req := httptest.NewRequest(http.MethodPost, "/rooms/invites", nil) // no body
	req.Header.Set("Authorization", "Bearer "+hostToken)
	rec := httptest.NewRecorder()
	in.handleCreateInvite(rec, req)
	var created createInviteResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Access != AccessView {
		t.Fatalf("default access = %q, want view", created.Access)
	}

	jbody, _ := json.Marshal(joinRequest{Code: created.Code, Name: "Bob"})
	jrec := httptest.NewRecorder()
	in.handleJoin(jrec, httptest.NewRequest(http.MethodPost, "/rooms/join", bytes.NewReader(jbody)))
	var joined joinResponse
	_ = json.Unmarshal(jrec.Body.Bytes(), &joined)
	id, _ := auth.Verify(joined.Token)
	if id.Access != AccessView {
		t.Fatalf("guest access = %q, want view", id.Access)
	}
}

func TestInvite_InvalidBodyAndAccessFailClosed(t *testing.T) {
	in, auth := newTestInviter(t)
	hostToken, _ := auth.Mint("alice", "room_42", RoleHost, "", time.Hour)
	for name, body := range map[string]string{
		"malformed":      `{`,
		"unknown access": `{"access":"admin"}`,
		"unknown field":  `{"access":"view","extra":true}`,
		"trailing JSON":  `{"access":"view"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/rooms/invites", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+hostToken)
			rec := httptest.NewRecorder()
			in.handleCreateInvite(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestInviteAndJoinRejectOversizedBodies(t *testing.T) {
	in, auth := newTestInviter(t)
	hostToken, _ := auth.Mint("alice", "room_42", RoleHost, "", time.Hour)
	inviteReq := httptest.NewRequest(http.MethodPost, "/rooms/invites", strings.NewReader(`{"access":"view","padding":"`+strings.Repeat("x", 5<<10)+`"}`))
	inviteReq.Header.Set("Authorization", "Bearer "+hostToken)
	inviteRec := httptest.NewRecorder()
	in.handleCreateInvite(inviteRec, inviteReq)
	if inviteRec.Code != http.StatusBadRequest {
		t.Fatalf("oversized invite status=%d", inviteRec.Code)
	}

	joinReq := httptest.NewRequest(http.MethodPost, "/rooms/join", strings.NewReader(`{"code":"ABCD2345","name":"`+strings.Repeat("x", 5<<10)+`"}`))
	joinRec := httptest.NewRecorder()
	in.handleJoin(joinRec, joinReq)
	if joinRec.Code != http.StatusBadRequest {
		t.Fatalf("oversized join status=%d", joinRec.Code)
	}
}

func TestInvitePreservesDeviceSessionAndRelayNode(t *testing.T) {
	auth := JWTAuth{
		Secret: []byte("invite-secret"), RequireScopedCapabilities: true, RequirePoolScope: true,
		PoolID: "pool-a", NodeID: "cell-a", AllowedApplications: map[string]bool{"pie-control": true},
	}
	hostScope := Identity{
		Room: "room_42", DeviceID: "device-a", SessionID: "session-a", ExecutionTarget: "docker",
		ApplicationID: "pie-control", PoolID: "pool-a", TenantID: "tenant-a", ResourceType: "device",
		ResourceID: "device-a", Protocol: "terminal", RelayNode: "cell-a", RelayGeneration: 7,
	}
	hostToken, err := auth.mintIdentity("alice", hostScope, RoleHost, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	inviter := NewInviter(auth)
	createReq := httptest.NewRequest(http.MethodPost, "/rooms/invites", strings.NewReader(`{"access":"view"}`))
	createReq.Header.Set("Authorization", "Bearer "+hostToken)
	createRec := httptest.NewRecorder()
	inviter.handleCreateInvite(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created createInviteResponse
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	joinReq := httptest.NewRequest(http.MethodPost, "/rooms/join", strings.NewReader(`{"code":"`+created.Code+`","name":"bob"}`))
	joinRec := httptest.NewRecorder()
	inviter.handleJoin(joinRec, joinReq)
	if joinRec.Code != http.StatusOK {
		t.Fatalf("join status=%d body=%s", joinRec.Code, joinRec.Body.String())
	}
	var joined joinResponse
	if err := json.NewDecoder(joinRec.Body).Decode(&joined); err != nil {
		t.Fatal(err)
	}
	if joined.DeviceID != "device-a" || joined.SessionID != "session-a" || joined.ExecutionTarget != "docker" || joined.RelayNode != "cell-a" || joined.RelayGeneration != 7 {
		t.Fatalf("scope lost: %+v", joined)
	}
	id, err := auth.Verify(joined.Token)
	if err != nil || id.DeviceID != joined.DeviceID || id.SessionID != joined.SessionID || id.ExecutionTarget != "docker" || id.Access != AccessView || id.RelayGeneration != 7 || id.ApplicationID != "pie-control" || id.PoolID != "pool-a" || id.TenantID != "tenant-a" {
		t.Fatalf("token scope=%+v err=%v", id, err)
	}
}

// TestInvite_RejectsParticipantCreator confirms a guest token cannot mint
// invites (only a host / legacy token may widen a room).
func TestInvite_RejectsParticipantCreator(t *testing.T) {
	in, auth := newTestInviter(t)
	guestToken, _ := auth.Mint("guest:bob-x7k2", "room_42", RoleParticipant, "", time.Hour)
	req := httptest.NewRequest(http.MethodPost, "/rooms/invites", nil)
	req.Header.Set("Authorization", "Bearer "+guestToken)
	rec := httptest.NewRecorder()
	in.handleCreateInvite(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("guest creating an invite should be 403, got %d", rec.Code)
	}
}

// TestInvite_LegacyTokenCanCreate confirms a legacy (no room/role) host PAT can
// still create an invite, scoped to room=sub.
func TestInvite_LegacyTokenCanCreate(t *testing.T) {
	in, auth := newTestInviter(t)
	legacy, _ := auth.Mint("alice", "", "", "", time.Hour) // no room/role
	req := httptest.NewRequest(http.MethodPost, "/rooms/invites", nil)
	req.Header.Set("Authorization", "Bearer "+legacy)
	rec := httptest.NewRecorder()
	in.handleCreateInvite(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy host token should create invite, got %d", rec.Code)
	}
	var created createInviteResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	body, _ := json.Marshal(joinRequest{Code: created.Code, Name: "carol"})
	jrec := httptest.NewRecorder()
	in.handleJoin(jrec, httptest.NewRequest(http.MethodPost, "/rooms/join", bytes.NewReader(body)))
	var joined joinResponse
	_ = json.Unmarshal(jrec.Body.Bytes(), &joined)
	if joined.Room != "alice" {
		t.Fatalf("legacy invite room should default to sub 'alice', got %q", joined.Room)
	}
}

// TestInvite_JoinExpiredCode confirms an expired code is rejected (and dropped).
func TestInvite_JoinExpiredCode(t *testing.T) {
	in, auth := newTestInviter(t)
	// Freeze time so we can advance past the TTL deterministically.
	base := time.Now()
	in.now = func() time.Time { return base }
	hostToken, _ := auth.Mint("alice", "room_42", RoleHost, "", time.Hour)

	req := httptest.NewRequest(http.MethodPost, "/rooms/invites", nil)
	req.Header.Set("Authorization", "Bearer "+hostToken)
	rec := httptest.NewRecorder()
	in.handleCreateInvite(rec, req)
	var created createInviteResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Advance past the invite TTL.
	in.now = func() time.Time { return base.Add(inviteTTL + time.Minute) }
	body, _ := json.Marshal(joinRequest{Code: created.Code, Name: "bob"})
	jrec := httptest.NewRecorder()
	in.handleJoin(jrec, httptest.NewRequest(http.MethodPost, "/rooms/join", bytes.NewReader(body)))
	if jrec.Code != http.StatusUnauthorized {
		t.Fatalf("expired code join should be 401, got %d", jrec.Code)
	}
}

func TestInviteCapacityIsBoundedAndExpiredCodesArePruned(t *testing.T) {
	in, auth := newTestInviter(t)
	base := time.Now()
	in.now = func() time.Time { return base }
	in.max = 2
	hostToken, _ := auth.Mint("alice", "room_42", RoleHost, "", time.Hour)
	create := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/rooms/invites", nil)
		req.Header.Set("Authorization", "Bearer "+hostToken)
		rec := httptest.NewRecorder()
		in.handleCreateInvite(rec, req)
		return rec
	}
	if first, second := create(), create(); first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("initial creates status=%d,%d", first.Code, second.Code)
	}
	full := create()
	if full.Code != http.StatusTooManyRequests || full.Header().Get("Retry-After") == "" {
		t.Fatalf("full status=%d retry-after=%q", full.Code, full.Header().Get("Retry-After"))
	}

	in.now = func() time.Time { return base.Add(inviteTTL + time.Second) }
	if afterExpiry := create(); afterExpiry.Code != http.StatusOK {
		t.Fatalf("create after expiry status=%d body=%s", afterExpiry.Code, afterExpiry.Body.String())
	}
	in.mu.Lock()
	count := len(in.codes)
	in.mu.Unlock()
	if count != 1 {
		t.Fatalf("expired invites were not pruned: count=%d", count)
	}
}

// TestInvite_JoinUnknownCode confirms a bogus code is rejected.
func TestInvite_JoinUnknownCode(t *testing.T) {
	in, _ := newTestInviter(t)
	body, _ := json.Marshal(joinRequest{Code: "NOPESUCH", Name: "bob"})
	jrec := httptest.NewRecorder()
	in.handleJoin(jrec, httptest.NewRequest(http.MethodPost, "/rooms/join", bytes.NewReader(body)))
	if jrec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown code join should be 401, got %d", jrec.Code)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"Bob":                   "bob",
		"  Alice  ":             "alice",
		"a b c":                 "a-b-c",
		"":                      "guest",
		"!!!":                   "guest",
		"weird/../name":         "weirdname",
		strings.Repeat("x", 50): strings.Repeat("x", 32),
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// 웹뷰(브라우저 엔진) 클라이언트는 cross-origin fetch에 CORS를 강제한다 —
// preflight(OPTIONS)가 204 + 허용 헤더로 응답해야 POST가 살아서 도착한다.
func TestRoomsCORS(t *testing.T) {
	auth := JWTAuth{Secret: []byte("s")}
	srv := NewServerOpts(ServerOptions{AgentAuth: auth, ParticipantAuth: auth, Inviter: NewInviter(auth)})

	req := httptest.NewRequest(http.MethodOptions, "/rooms/join", nil)
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

	// 실제 POST 응답에도 Allow-Origin이 실려야 브라우저가 본문을 읽을 수 있다.
	req2 := httptest.NewRequest(http.MethodPost, "/rooms/invites", nil)
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if got := rec2.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("POST Allow-Origin = %q, want *", got)
	}
}
