package relay

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type failingRandomReader struct{}

func (failingRandomReader) Read([]byte) (int, error) {
	return 0, errors.New("random source unavailable")
}

func mintTestJWT(t *testing.T, secret []byte, sub, deviceID string, ttl time.Duration) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":      sub,
		"deviceId": deviceID,
		"iat":      now.Unix(),
		"exp":      now.Add(ttl).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(secret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestJWTAuth_AgentUser_Valid(t *testing.T) {
	secret := []byte("test-secret")
	auth := JWTAuth{Secret: secret}
	token := mintTestJWT(t, secret, "user_1", "device_1", time.Hour)
	id, err := auth.AgentUser(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.UserID != "user_1" {
		t.Fatalf("got user %q, want user_1", id.UserID)
	}
	// Backward compat: a token with no "room"/"role" claims resolves room to
	// sub and leaves role empty (the connection axis infers it).
	if id.Room != "user_1" {
		t.Fatalf("got room %q, want room to default to sub user_1", id.Room)
	}
	if id.Role != "" {
		t.Fatalf("got role %q, want empty (legacy token)", id.Role)
	}
}

func TestJWTAuth_AgentUser_WrongSecret(t *testing.T) {
	auth := JWTAuth{Secret: []byte("test-secret")}
	token := mintTestJWT(t, []byte("other-secret"), "user_1", "device_1", time.Hour)
	if _, err := auth.AgentUser(context.Background(), token); err == nil {
		t.Fatal("expected error for wrong secret, got nil")
	}
}

func TestJWTAuth_AgentUser_Expired(t *testing.T) {
	secret := []byte("test-secret")
	auth := JWTAuth{Secret: secret}
	token := mintTestJWT(t, secret, "user_1", "device_1", -time.Hour)
	if _, err := auth.AgentUser(context.Background(), token); err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

func TestJWTAuth_ParticipantUser_SameAsAgent(t *testing.T) {
	secret := []byte("test-secret")
	auth := JWTAuth{Secret: secret}
	token := mintTestJWT(t, secret, "user_1", "vibe-canvas", time.Hour)
	id, err := auth.ParticipantUser(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.UserID != "user_1" {
		t.Fatalf("got user %q, want user_1", id.UserID)
	}
}

// TestJWTAuth_RoomRoleClaims confirms explicit room/role claims survive
// verification (the rooms extension) while Mint round-trips them.
func TestJWTAuth_RoomRoleClaims(t *testing.T) {
	secret := []byte("test-secret")
	auth := JWTAuth{Secret: secret}
	token, err := auth.Mint("guest:bob-x7k2", "room_42", RoleParticipant, "", time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	id, err := auth.Verify(token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.UserID != "guest:bob-x7k2" || id.Room != "room_42" || id.Role != RoleParticipant {
		t.Fatalf("got %+v, want sub=guest:bob-x7k2 room=room_42 role=participant", id)
	}
}

func TestJWTAuthScopedDeviceSessionClaims(t *testing.T) {
	auth := JWTAuth{Secret: []byte("test-secret")}
	token, err := auth.MintScoped("alice", "team-room", "device-a", "session-a", RoleHost, "", "cell-a", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, err := auth.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if id.DeviceID != "device-a" || id.SessionID != "session-a" || id.RelayNode != "cell-a" {
		t.Fatalf("scope did not round-trip: %+v", id)
	}
	if id.RoutingKey() == id.Room {
		t.Fatal("scoped token reused legacy room key")
	}
	other, _ := auth.MintScoped("alice", "team-room", "device-a", "session-b", RoleHost, "", "cell-a", time.Hour)
	otherID, _ := auth.Verify(other)
	if id.RoutingKey() == otherID.RoutingKey() {
		t.Fatal("sessions share a routing key")
	}
	if _, err := auth.MintScoped("alice", "room", "device-only", "", RoleHost, "", "", time.Hour); err == nil {
		t.Fatal("partial scope was accepted")
	}
}

func TestJWTAuthVersionedResourceContext(t *testing.T) {
	secret := []byte("test-secret")
	claims := jwt.MapClaims{
		"iss": relayControlTokenIssuer, "aud": relayTokenAudience, "sub": "member-a",
		"room": "r_v2_opaque", "device_id": "executor-a", "session_id": "session-a",
		"role": RoleParticipant, "access": AccessControl, "jti": "token-a",
		"cap":           []string{CapabilityParticipantConnect, CapabilityTerminalView, CapabilityTerminalControl},
		"relay_version": relayContextVersion, "app_id": "pie-canvas", "pool_id": "pie-canvas-seoul",
		"tenant_id": "workspace-a", "resource_type": "project", "resource_id": "project-a",
		"agent_id": "claude-code", "protocol": "acp", "exp": time.Now().Add(time.Hour).Unix(),
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	id, err := (JWTAuth{Secret: secret, RequireScopedCapabilities: true}).ParticipantUser(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if id.ProtocolVersion != relayContextVersion || id.ApplicationID != "pie-canvas" ||
		id.PoolID != "pie-canvas-seoul" || id.TenantID != "workspace-a" ||
		id.ResourceType != "project" || id.ResourceID != "project-a" ||
		id.AgentID != "claude-code" || id.Protocol != "acp" {
		t.Fatalf("identity=%+v", id)
	}
}

func TestJWTAuthRejectsUnknownProtocolVersion(t *testing.T) {
	secret := []byte("test-secret")
	claims := jwt.MapClaims{
		"sub": "member-a", "room": "room-a", "relay_version": 99,
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (JWTAuth{Secret: secret}).Verify(raw); err == nil {
		t.Fatal("unknown relay protocol version was accepted")
	}
}

func TestJWTAuthPoolIsolation(t *testing.T) {
	secret := []byte("test-secret")
	mint := func(appID, poolID, relayNode string) string {
		t.Helper()
		claims := jwt.MapClaims{
			"iss": relayControlTokenIssuer, "aud": relayTokenAudience, "sub": "member-a",
			"room": "opaque-room", "device_id": "executor-a", "session_id": "session-a",
			"role": RoleParticipant, "access": AccessView, "jti": "token-a",
			"cap":           []string{CapabilityParticipantConnect, CapabilityTerminalView},
			"relay_version": relayContextVersion, "app_id": appID, "pool_id": poolID,
			"tenant_id": "workspace-a", "resource_type": "project", "resource_id": "project-a",
			"relay_node": relayNode, "relay_generation": 1, "protocol": "acp", "exp": time.Now().Add(time.Hour).Unix(),
		}
		raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	auth := JWTAuth{
		Secret: secret, RequireScopedCapabilities: true, RequirePoolScope: true,
		PoolID: "pie-canvas-seoul", NodeID: "relay-a", AllowedApplications: map[string]bool{"pie-canvas": true},
	}
	if _, err := auth.ParticipantUser(context.Background(), mint("pie-canvas", "pie-canvas-seoul", "relay-a")); err != nil {
		t.Fatalf("valid pool token rejected: %v", err)
	}
	for name, raw := range map[string]string{
		"application": mint("other-app", "pie-canvas-seoul", "relay-a"),
		"pool":        mint("pie-canvas", "other-pool", "relay-a"),
		"node":        mint("pie-canvas", "pie-canvas-seoul", "relay-b"),
	} {
		if _, err := auth.ParticipantUser(context.Background(), raw); err == nil {
			t.Fatalf("%s mismatch was accepted", name)
		}
	}
}

func TestJWTAuthPoolScopeDirectIssuerCompatibilityIsExplicit(t *testing.T) {
	secret := []byte("test-secret")
	direct, err := (JWTAuth{Secret: secret}).Mint("mobile-owner", "mobile-room", RoleHost, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	strict := JWTAuth{
		Secret: secret, RequireScopedCapabilities: true, RequirePoolScope: true,
		PoolID: "pool-a", NodeID: "relay-a", AllowedApplications: map[string]bool{"pie-control": true},
	}
	if _, err := strict.Verify(direct); err == nil {
		t.Fatal("direct Relay token bypassed pool scope without the compatibility switch")
	}
	strict.AllowDirectTokensWithoutPoolScope = true
	if _, err := strict.Verify(direct); err != nil {
		t.Fatalf("explicit direct-token compatibility rejected Relay-issued token: %v", err)
	}

	now := time.Now()
	controlClaims := jwt.MapClaims{
		"iss": relayControlTokenIssuer, "aud": relayTokenAudience, "sub": "owner", "room": "room",
		"role": RoleHost, "access": AccessControl, "jti": "token-a",
		"cap": []string{CapabilityHostConnect, CapabilityParticipantConnect, CapabilityTerminalView, CapabilityTerminalControl},
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	controlToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, controlClaims).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Verify(controlToken); err == nil {
		t.Fatal("unscoped Pie Control token used the Relay direct-token compatibility path")
	}
}

func TestJWTAuthRelayGenerationFencesRoutingKey(t *testing.T) {
	base := Identity{Room: "room", DeviceID: "device-a", SessionID: "session-a", RelayGeneration: 1}
	next := base
	next.RelayGeneration = 2
	if base.RoutingKey() == next.RoutingKey() {
		t.Fatal("different Relay generations shared a routing key")
	}
	legacy := base
	legacy.RelayGeneration = 0
	if legacy.RoutingKey() == base.RoutingKey() {
		t.Fatal("legacy and fenced routing keys were not isolated")
	}
}

func TestJWTAuthRejectsFractionalRelayGeneration(t *testing.T) {
	secret := []byte("test-secret")
	claims := jwt.MapClaims{
		"iss": relayControlTokenIssuer, "aud": relayTokenAudience, "sub": "owner", "room": "room",
		"role": RoleHost, "access": AccessControl, "jti": "token-a",
		"cap": []string{CapabilityHostConnect}, "relay_generation": 1.5,
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (JWTAuth{Secret: secret, RequireScopedCapabilities: true}).Verify(raw); err == nil {
		t.Fatal("fractional Relay generation was accepted")
	}
}

func TestJWTAuth_MintFailsWhenRandomSourceFails(t *testing.T) {
	auth := JWTAuth{Secret: []byte("test-secret"), Random: failingRandomReader{}}
	if _, err := auth.Mint("alice", "room", RoleHost, "", time.Hour); err == nil {
		t.Fatal("scoped token was minted without a cryptographic token id")
	}
	// Legacy compatibility tokens do not contain a jti and therefore do not
	// consume the scoped credential random source.
	if _, err := auth.Mint("alice", "", "", "", time.Hour); err != nil {
		t.Fatalf("legacy token unexpectedly required a token id: %v", err)
	}
}

// TestJWTAuth_AccessClaim confirms an explicit access grade round-trips through
// Mint/Verify, and that a token with NO access claim (legacy, minted before the
// grading existed) reads back as "control" — the backward-compatible default.
func TestJWTAuth_AccessClaim(t *testing.T) {
	secret := []byte("test-secret")
	auth := JWTAuth{Secret: secret}

	viewTok, _ := auth.Mint("guest:eve-1", "room_42", RoleParticipant, AccessView, time.Hour)
	id, err := auth.Verify(viewTok)
	if err != nil {
		t.Fatalf("verify view: %v", err)
	}
	if id.Access != AccessView {
		t.Fatalf("access = %q, want view", id.Access)
	}

	// No access claim → control (legacy participant token stays fully capable).
	legacyTok, _ := auth.Mint("guest:bob-1", "room_42", RoleParticipant, "", time.Hour)
	id, err = auth.Verify(legacyTok)
	if err != nil {
		t.Fatalf("verify legacy: %v", err)
	}
	if id.Access != AccessControl {
		t.Fatalf("legacy access = %q, want control default", id.Access)
	}
}

func TestJWTAuth_ScopedTokensCannotCrossConnectionAxes(t *testing.T) {
	auth := JWTAuth{Secret: []byte("test-secret")}
	participant, err := auth.Mint("guest:bob", "room", RoleParticipant, AccessControl, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.ParticipantUser(context.Background(), participant); err != nil {
		t.Fatalf("participant axis rejected scoped token: %v", err)
	}
	if _, err := auth.AgentUser(context.Background(), participant); err == nil {
		t.Fatal("participant capability token crossed into host axis")
	}

	host, _ := auth.Mint("alice", "room", RoleHost, "", time.Hour)
	id, err := auth.Verify(host)
	if err != nil || id.TokenID == "" || !id.Allows(CapabilityInviteCreate) {
		t.Fatalf("host capability claims missing: id=%+v err=%v", id, err)
	}
}

func TestJWTAuth_ScopedParticipantCapabilitiesFailClosed(t *testing.T) {
	secret := []byte("test-secret")
	mint := func(capabilities []string) string {
		t.Helper()
		claims := jwt.MapClaims{
			"sub": "bob", "room": "room", "role": RoleParticipant, "access": AccessControl,
			"aud": relayTokenAudience, "cap": capabilities, "exp": time.Now().Add(time.Hour).Unix(),
		}
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	auth := JWTAuth{Secret: secret}

	viewOnly, err := auth.ParticipantUser(context.Background(), mint([]string{
		CapabilityParticipantConnect, CapabilityTerminalView,
	}))
	if err != nil || viewOnly.Access != AccessView {
		t.Fatalf("missing control capability was not downgraded: id=%+v err=%v", viewOnly, err)
	}
	if _, err := auth.ParticipantUser(context.Background(), mint([]string{
		CapabilityParticipantConnect,
	})); err == nil {
		t.Fatal("participant token without terminal:view was accepted")
	}
}

func TestJWTAuth_RejectsWrongAudienceWhenPresent(t *testing.T) {
	secret := []byte("test-secret")
	claims := jwt.MapClaims{
		"sub": "alice", "room": "room", "role": RoleHost,
		"aud": "some-other-service", "exp": time.Now().Add(time.Hour).Unix(),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (JWTAuth{Secret: secret}).Verify(token); err == nil {
		t.Fatal("wrong audience was accepted")
	}
}

func TestJWTAuth_StrictModeRejectsLegacyAndAcceptsScoped(t *testing.T) {
	secret := []byte("test-secret")
	strict := JWTAuth{Secret: secret, RequireScopedCapabilities: true}

	legacy := mintTestJWT(t, secret, "alice", "device-a", time.Hour)
	if _, err := strict.Verify(legacy); err == nil {
		t.Fatal("strict verifier accepted a legacy unscoped token")
	}

	scoped, err := (JWTAuth{Secret: secret}).Mint(
		"alice", "room-a", RoleHost, "", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	id, err := strict.AgentUser(context.Background(), scoped)
	if err != nil {
		t.Fatalf("strict verifier rejected a scoped host token: %v", err)
	}
	if id.Role != RoleHost || id.TokenID == "" || !id.Allows(CapabilityHostConnect) {
		t.Fatalf("scoped identity is incomplete: %+v", id)
	}
}

func TestJWTAuth_StrictModeRejectsIncompleteScope(t *testing.T) {
	secret := []byte("test-secret")
	strict := JWTAuth{Secret: secret, RequireScopedCapabilities: true}
	now := time.Now()
	complete := jwt.MapClaims{
		"sub": "alice", "room": "room-a", "role": RoleHost,
		"iss": relayTokenIssuer, "aud": relayTokenAudience, "jti": "token-a",
		"cap": []string{CapabilityHostConnect},
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}

	for _, claim := range []string{"role", "iss", "aud", "jti", "cap"} {
		t.Run("missing_"+claim, func(t *testing.T) {
			claims := jwt.MapClaims{}
			for key, value := range complete {
				claims[key] = value
			}
			delete(claims, claim)
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := strict.Verify(token); err == nil {
				t.Fatalf("strict verifier accepted token missing %s", claim)
			}
		})
	}
}

func TestJWTAuth_RejectsInvalidRoleAndAccess(t *testing.T) {
	secret := []byte("test-secret")
	now := time.Now()
	for name, overrides := range map[string]jwt.MapClaims{
		"role":   {"role": "administrator", "access": AccessControl},
		"access": {"role": RoleParticipant, "access": "write"},
	} {
		t.Run(name, func(t *testing.T) {
			claims := jwt.MapClaims{
				"sub": "alice", "room": "room-a", "exp": now.Add(time.Hour).Unix(),
			}
			for key, value := range overrides {
				claims[key] = value
			}
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (JWTAuth{Secret: secret}).Verify(token); err == nil {
				t.Fatalf("verifier accepted invalid %s", name)
			}
		})
	}

	auth := JWTAuth{Secret: secret}
	if _, err := auth.Mint("alice", "room-a", "administrator", "", time.Hour); err == nil {
		t.Fatal("Mint accepted an invalid role")
	}
	if _, err := auth.Mint("alice", "room-a", RoleParticipant, "write", time.Hour); err == nil {
		t.Fatal("Mint accepted an invalid access grade")
	}
}

// TestJWTAuth_HostAlwaysControl confirms the operator can never be demoted: even
// a host token that somehow carries access=view verifies as control.
func TestJWTAuth_HostAlwaysControl(t *testing.T) {
	secret := []byte("test-secret")
	auth := JWTAuth{Secret: secret}
	tok, _ := auth.Mint("alice", "room_42", RoleHost, AccessView, time.Hour)
	id, err := auth.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.Access != AccessControl {
		t.Fatalf("host access = %q, want control (host is never a spectator)", id.Access)
	}
}

// TestJWTAuth_AgentUser_MissingExp guards against the fail-open gap where
// golang-jwt only checks "exp" when it is present — a validly-signed token
// that simply omits "exp" would otherwise verify as valid forever, quietly
// defeating the short-lived-access-token design.
func TestJWTAuth_AgentUser_MissingExp(t *testing.T) {
	secret := []byte("test-secret")
	auth := JWTAuth{Secret: secret}
	claims := jwt.MapClaims{
		"sub":      "user-no-exp",
		"deviceId": "device_1",
		"iat":      time.Now().Unix(),
		// intentionally no "exp"
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(secret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := auth.AgentUser(context.Background(), signed); err == nil {
		t.Fatal("expected error for token missing exp claim, got nil")
	}
}

// TestJWTAuth_AgentUser_AlgNone guards against the classic "alg: none"
// downgrade attack, where an attacker strips the signature and asserts no
// signing was used at all.
func TestJWTAuth_AgentUser_AlgNone(t *testing.T) {
	secret := []byte("test-secret")
	auth := JWTAuth{Secret: secret}
	claims := jwt.MapClaims{
		"sub": "user_1",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signed, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := auth.AgentUser(context.Background(), signed); err == nil {
		t.Fatal("expected error for alg:none token, got nil")
	}
}

// TestJWTAuth_AgentUser_AlgConfusion_RS256 guards against algorithm-confusion
// attacks where a token signed with an asymmetric algorithm (here RS256) is
// presented to a verifier expecting a shared HMAC secret.
func TestJWTAuth_AgentUser_AlgConfusion_RS256(t *testing.T) {
	secret := []byte("test-secret")
	auth := JWTAuth{Secret: secret}

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	claims := jwt.MapClaims{
		"sub": "user_1",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(privKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := auth.AgentUser(context.Background(), signed); err == nil {
		t.Fatal("expected error for RS256 alg-confusion token, got nil")
	}
}
