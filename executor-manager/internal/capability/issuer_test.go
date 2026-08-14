package capability

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("random source unavailable")
}

func TestMintSessionScopesCapabilities(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	now := time.Unix(1_800_000_000, 0)
	issuer := Issuer{Secret: secret, Now: func() time.Time { return now }}
	minted, err := issuer.MintSession(SessionCredential{Subject: "guest", Room: "owner", DeviceID: "device-a", SessionID: "session-a", ExecutionTarget: "docker", Role: "participant", Access: "view", RelayNode: "cell-a", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.Parse(minted.Token, func(token *jwt.Token) (any, error) { return secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience("pie-relay"), jwt.WithIssuer("pie-control"), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil || !parsed.Valid {
		t.Fatalf("parse=%v valid=%t", err, parsed != nil && parsed.Valid)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["device_id"] != "device-a" || claims["session_id"] != "session-a" || claims["execution_target"] != "docker" || claims["relay_node"] != "cell-a" {
		t.Fatalf("claims=%+v", claims)
	}
	capabilities := claims["cap"].([]any)
	for _, capability := range capabilities {
		if capability == "terminal:control" {
			t.Fatal("view token contains control")
		}
	}
}

func TestMintSessionCarriesVersionedResourceContext(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	issuer := Issuer{Secret: secret, RoutingSecret: []byte("routing-secret-012345678901234567")}
	minted, err := issuer.MintSession(SessionCredential{
		Subject: "member-a", Room: "opaque-room", DeviceID: "executor-a", SessionID: "session-a",
		ApplicationID: "pie-canvas", PoolID: "pie-canvas-seoul", TenantID: "workspace-a",
		ResourceType: "project", ResourceID: "project-a", AgentID: "claude-code", Protocol: "acp",
		Role: "participant", Access: "control", RelayGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if minted.ProtocolVersion != RelayProtocolVersion || minted.ApplicationID != "pie-canvas" || minted.ResourceID != "project-a" {
		t.Fatalf("minted context=%+v", minted)
	}
	parsed, err := jwt.Parse(minted.Token, func(*jwt.Token) (any, error) { return secret, nil })
	if err != nil {
		t.Fatal(err)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["relay_version"] != float64(RelayProtocolVersion) || claims["app_id"] != "pie-canvas" ||
		claims["pool_id"] != "pie-canvas-seoul" || claims["tenant_id"] != "workspace-a" ||
		claims["resource_type"] != "project" || claims["resource_id"] != "project-a" ||
		claims["agent_id"] != "claude-code" || claims["protocol"] != "acp" || claims["relay_generation"] != float64(1) {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestOpaqueRoomIsStableAndResourceBound(t *testing.T) {
	issuer := Issuer{RoutingSecret: []byte("routing-secret-012345678901234567")}
	first, err := issuer.OpaqueRoom("pie-canvas", "workspace-a", "project", "project-a", "session-a")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := issuer.OpaqueRoom("pie-canvas", "workspace-a", "project", "project-a", "session-a")
	other, _ := issuer.OpaqueRoom("pie-canvas", "workspace-a", "project", "project-b", "session-a")
	if first != again || first == other || len(first) < 20 || first[:5] != "r_v2_" {
		t.Fatalf("first=%q again=%q other=%q", first, again, other)
	}
	if strings.Contains(first, "workspace") || strings.Contains(first, "project") {
		t.Fatalf("opaque room leaked scope: %q", first)
	}
}

func TestMintSessionRejectsInvalidResourceContext(t *testing.T) {
	issuer := Issuer{Secret: []byte("01234567890123456789012345678901")}
	base := SessionCredential{Subject: "owner", Room: "room", DeviceID: "device-a", SessionID: "session-a", Role: "host", Access: "control"}
	base.TenantID = "../../other-workspace"
	if _, err := issuer.MintSession(base); err == nil {
		t.Fatal("path-like tenant scope was accepted")
	}
	base.TenantID = "workspace-a"
	base.Protocol = "raw-shell"
	if _, err := issuer.MintSession(base); err == nil {
		t.Fatal("unknown relay protocol was accepted")
	}
}

func TestHostIsAlwaysControl(t *testing.T) {
	issuer := Issuer{Secret: []byte("01234567890123456789012345678901")}
	minted, err := issuer.MintSession(SessionCredential{Subject: "owner", Room: "owner", DeviceID: "device-a", SessionID: "session-a", Role: "host", Access: "view"})
	if err != nil {
		t.Fatal(err)
	}
	if minted.Access != "control" {
		t.Fatalf("access=%s", minted.Access)
	}
}

func TestPrivateHostCannotMintInvites(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	issuer := Issuer{Secret: secret}
	privateToken, err := issuer.MintSession(SessionCredential{Subject: "owner", Room: "owner", DeviceID: "device-a", SessionID: "private", Role: "host", Access: "control"})
	if err != nil {
		t.Fatal(err)
	}
	sharedToken, err := issuer.MintSession(SessionCredential{Subject: "owner", Room: "owner", DeviceID: "device-a", SessionID: "shared", Role: "host", Access: "control", AllowInvite: true})
	if err != nil {
		t.Fatal(err)
	}
	hasInvite := func(raw string) bool {
		parsed, _ := jwt.Parse(raw, func(*jwt.Token) (any, error) { return secret, nil })
		for _, value := range parsed.Claims.(jwt.MapClaims)["cap"].([]any) {
			if value == "relay:invite:create" {
				return true
			}
		}
		return false
	}
	if hasInvite(privateToken.Token) || !hasInvite(sharedToken.Token) {
		t.Fatal("invite capability did not follow session access mode")
	}
}

func TestMintSessionFailsWhenRandomSourceFails(t *testing.T) {
	issuer := Issuer{
		Secret: []byte("01234567890123456789012345678901"),
		Random: failingReader{},
	}
	_, err := issuer.MintSession(SessionCredential{
		Subject: "owner", Room: "owner", DeviceID: "device-a", SessionID: "session-a",
		Role: "host", Access: "control",
	})
	if err == nil {
		t.Fatal("credential was minted without a cryptographic token id")
	}
}
