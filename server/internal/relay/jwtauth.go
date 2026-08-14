package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWTAuth verifies HS256 JWTs signed by Pie Control (또는 릴레이 자신의
// /rooms/join 발급기) with a shared secret. Every leg (host, participant) uses
// the SAME verifier — no network call: verification is entirely offline
// against Secret. The same secret also SIGNS guest tokens minted at
// /rooms/join, so the relay is both verifier and issuer for the room flow.
type JWTAuth struct {
	Secret                            []byte
	RequireScopedCapabilities         bool
	RequirePoolScope                  bool
	AllowDirectTokensWithoutPoolScope bool
	PoolID                            string
	NodeID                            string
	AllowedApplications               map[string]bool
	Random                            io.Reader
}

// Role values carried in the "role" claim. An empty role is legacy (older
// tokens predate rooms) — the connection axis infers it: /ws/agent → host,
// /ws/participant → participant.
const (
	RoleHost        = "host"
	RoleParticipant = "participant"
)

// Access values carried in the "access" claim decide what a participant may do
// once in a room: "control" may send input (chat, terminal keystrokes) and, in
// a terminal room, auto-becomes the driver; "view" is pure spectate — the relay
// drops its input-bearing messages. An absent claim is legacy and reads as
// "control" (unchanged behavior for tokens minted before access grading);
// role=host is ALWAYS control regardless of the claim.
const (
	AccessView    = "view"
	AccessControl = "control"
)

// Identity is the verified caller: who they are (UserID = sub, used as the
// `from` label), which room they belong to, and their role. Room defaults to
// UserID when the token carries no "room" claim. This supports an explicitly
// enabled migration from older single-sub tokens (room == sub).
type Identity struct {
	UserID          string // sub — the person's identity (from 표기에 사용)
	Room            string // pairing key; falls back to UserID when "room" claim absent
	DeviceID        string // optional scoped target device; paired with SessionID
	SessionID       string // optional terminal session; paired with DeviceID
	ProtocolVersion int
	ApplicationID   string
	PoolID          string
	TenantID        string
	ResourceType    string
	ResourceID      string
	AgentID         string
	Protocol        string
	ExecutionTarget string // optional "local" | "docker" session runtime metadata
	Role            string // "host" | "participant" | "" (빈 값이면 접속 축으로 추론)
	Access          string // "control" | "view"; falls back to "control" (legacy/host)
	TokenID         string
	Capabilities    map[string]bool
	RelayNode       string
	RelayGeneration int64
}

// RoutingKey is internal-only and cannot be selected by an unverified frame.
// Legacy tokens preserve room-only routing; scoped tokens isolate every
// device/session pair even when they share the same human-facing room.
func (id Identity) RoutingKey() string {
	if id.DeviceID == "" && id.SessionID == "" {
		return id.Room
	}
	key := id.Room + "\x1f" + id.DeviceID + "\x1f" + id.SessionID
	if id.RelayGeneration > 0 {
		key += "\x1f" + strconv.FormatInt(id.RelayGeneration, 10)
	}
	return key
}

const (
	CapabilityHostConnect        = "relay:host:connect"
	CapabilityParticipantConnect = "relay:participant:connect"
	CapabilityInviteCreate       = "relay:invite:create"
	CapabilityTerminalView       = "terminal:view"
	CapabilityTerminalControl    = "terminal:control"
	relayTokenAudience           = "pie-relay"
	relayTokenIssuer             = "pie-relay"
	relayControlTokenIssuer      = "pie-control"
	relayContextVersion          = 3
)

var errInvalidToken = errors.New("invalid or expired token")

// Verify enforces the algorithm TWICE, deliberately, at two different layers:
//   - the keyfunc's type assertion (*jwt.SigningMethodHMAC) rejects the whole
//     non-HMAC family — "none", RS256, ES256, etc. — before any key material
//     is even considered (this is what stops the classic alg-confusion attack
//     where an RSA public key gets reused as an HMAC secret).
//   - jwt.WithValidMethods([]string{"HS256"}) then narrows further, WITHIN
//     the HMAC family, to exactly HS256 — it alone would NOT stop HS384/HS512
//     from slipping through the type assertion above.
//
// Neither check subsumes the other; do not remove either one as "redundant".
//
// jwt.WithExpirationRequired() closes a separate gap: golang-jwt only
// validates "exp" when present, so a validly-signed token that simply omits
// "exp" would otherwise verify as valid forever — silently defeating the
// short-lived-access-token + refresh design this relay depends on.
func (j JWTAuth) Verify(tokenString string) (Identity, error) {
	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errInvalidToken
		}
		return j.Secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		return Identity{}, errInvalidToken
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return Identity{}, errInvalidToken
	}
	sub, ok := claims["sub"].(string)
	if !ok || sub == "" {
		return Identity{}, errInvalidToken
	}
	// room/role are OPTIONAL only in explicit legacy compatibility mode.
	// room defaults to sub (private room of one); role stays empty so the
	// connection handler can infer it from the axis.
	room, _ := claims["room"].(string)
	if room == "" {
		room = sub
	}
	role, _ := claims["role"].(string)
	if role != "" && role != RoleHost && role != RoleParticipant {
		return Identity{}, errInvalidToken
	}
	// access is OPTIONAL — absence means "control" (legacy tokens predate the
	// view/control grading, so they keep full input rights). A host is always
	// control: the operator can never be demoted to a spectator by a claim.
	access, _ := claims["access"].(string)
	if access != "" && access != AccessView && access != AccessControl {
		return Identity{}, errInvalidToken
	}
	if access == "" {
		access = AccessControl
	}
	if role == RoleHost {
		access = AccessControl
	}
	audience, audiencePresent := claims["aud"]
	if audiencePresent && !claimContainsString(audience, relayTokenAudience) {
		return Identity{}, errInvalidToken
	}
	capabilities := claimStringSet(claims["cap"])
	tokenID, _ := claims["jti"].(string)
	issuer, _ := claims["iss"].(string)
	if j.RequireScopedCapabilities && (role == "" || len(capabilities) == 0 || !audiencePresent || !claimContainsString(audience, relayTokenAudience) || !validRelayIssuer(issuer) || tokenID == "") {
		return Identity{}, errInvalidToken
	}
	relayNode, _ := claims["relay_node"].(string)
	relayGeneration, generationOK := claimInteger(claims["relay_generation"])
	if !generationOK {
		return Identity{}, errInvalidToken
	}
	deviceID, _ := claims["device_id"].(string)
	sessionID, _ := claims["session_id"].(string)
	executionTarget, _ := claims["execution_target"].(string)
	protocolVersion64, protocolVersionOK := claimInteger(claims["relay_version"])
	if !protocolVersionOK {
		return Identity{}, errInvalidToken
	}
	protocolVersion := int(protocolVersion64)
	applicationID, _ := claims["app_id"].(string)
	poolID, _ := claims["pool_id"].(string)
	tenantID, _ := claims["tenant_id"].(string)
	resourceType, _ := claims["resource_type"].(string)
	resourceID, _ := claims["resource_id"].(string)
	agentID, _ := claims["agent_id"].(string)
	protocol, _ := claims["protocol"].(string)
	if (deviceID == "") != (sessionID == "") || deviceID != "" && (!streamIDPattern.MatchString(deviceID) || !streamIDPattern.MatchString(sessionID)) {
		return Identity{}, errInvalidToken
	}
	if executionTarget != "" && executionTarget != "local" && executionTarget != "docker" {
		return Identity{}, errInvalidToken
	}
	if protocolVersion != 0 && protocolVersion != relayContextVersion {
		return Identity{}, errInvalidToken
	}
	if relayGeneration < 0 {
		return Identity{}, errInvalidToken
	}
	for _, scope := range []string{applicationID, poolID, tenantID, resourceType, resourceID, agentID} {
		if scope != "" && !streamIDPattern.MatchString(scope) {
			return Identity{}, errInvalidToken
		}
	}
	if protocol != "" && protocol != "terminal" && protocol != "acp" {
		return Identity{}, errInvalidToken
	}
	contextValues := []string{applicationID, tenantID, resourceType, resourceID}
	contextCount := 0
	for _, value := range contextValues {
		if value != "" {
			contextCount++
		}
	}
	if contextCount != 0 && contextCount != len(contextValues) {
		return Identity{}, errInvalidToken
	}
	if poolID != "" && j.PoolID != "" && poolID != j.PoolID {
		return Identity{}, errInvalidToken
	}
	if applicationID != "" && len(j.AllowedApplications) > 0 && !j.AllowedApplications[applicationID] {
		return Identity{}, errInvalidToken
	}
	if relayNode != "" && j.NodeID != "" && relayNode != j.NodeID {
		return Identity{}, errInvalidToken
	}
	requirePoolScope := j.RequirePoolScope
	if requirePoolScope && issuer == relayTokenIssuer && j.AllowDirectTokensWithoutPoolScope && applicationID == "" && poolID == "" {
		requirePoolScope = false
	}
	if requirePoolScope && (protocolVersion != relayContextVersion || applicationID == "" || poolID == "" ||
		tenantID == "" || resourceType == "" || resourceID == "" || relayNode == "" ||
		relayGeneration < 1 || j.PoolID == "" || j.NodeID == "" || len(j.AllowedApplications) == 0) {
		return Identity{}, errInvalidToken
	}
	return Identity{
		UserID: sub, Room: room, DeviceID: deviceID, SessionID: sessionID,
		ProtocolVersion: protocolVersion, ApplicationID: applicationID, PoolID: poolID,
		TenantID: tenantID, ResourceType: resourceType, ResourceID: resourceID,
		AgentID: agentID, Protocol: protocol, ExecutionTarget: executionTarget, Role: role, Access: access,
		TokenID: tokenID, Capabilities: capabilities, RelayNode: relayNode, RelayGeneration: relayGeneration,
	}, nil
}

func validRelayIssuer(value string) bool {
	return value == relayTokenIssuer || value == relayControlTokenIssuer
}

func claimInteger(raw any) (int64, bool) {
	if raw == nil {
		return 0, true
	}
	switch value := raw.(type) {
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || math.Abs(value) > 1<<53-1 {
			return 0, false
		}
		return int64(value), true
	case int:
		return int64(value), true
	case int64:
		return value, true
	}
	return 0, false
}

func claimContainsString(raw any, want string) bool {
	switch v := raw.(type) {
	case string:
		return v == want
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	case []string:
		for _, s := range v {
			if s == want {
				return true
			}
		}
	}
	return false
}

func claimStringSet(raw any) map[string]bool {
	out := map[string]bool{}
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out[s] = true
			}
		}
	case []string:
		for _, s := range v {
			if s != "" {
				out[s] = true
			}
		}
	case string:
		if v != "" {
			out[v] = true
		}
	}
	return out
}

// Allows treats a missing capability claim as a legacy token only when the
// verifier was explicitly configured to accept that token. Production uses
// RequireScopedCapabilities, so this path is a migration compatibility aid.
func (id Identity) Allows(capability string) bool {
	return len(id.Capabilities) == 0 || id.Capabilities[capability]
}

// Mint signs an HS256 room token. Used by -mint (dev) and by the /rooms/join
// issuer to hand a guest a participant token. room/role/access are omitted from
// the claims when empty so a plain `-mint <user>` still produces a legacy-shaped
// token (room inferred = sub, access inferred = control) that verifies
// identically. Pass access="" for host/legacy tokens; the invite issuer passes
// the code's grade (view/control).
func (j JWTAuth) Mint(sub, room, role, access string, ttl time.Duration) (string, error) {
	return j.MintScoped(sub, room, "", "", role, access, "", ttl)
}

// MintScoped binds a credential to one device/session and, optionally, one
// Relay Cell. Clients cannot widen these values in relay_join or application
// frames because routing is derived only from the verified claims.
func (j JWTAuth) MintScoped(sub, room, deviceID, sessionID, role, access, relayNode string, ttl time.Duration) (string, error) {
	return j.MintScopedWithTarget(sub, room, deviceID, sessionID, "", role, access, relayNode, ttl)
}

// MintScopedWithTarget carries display-only runtime metadata alongside the
// routing scope. Relay authorization never trusts this field; clients use it to
// clearly label whether the verified session terminates on a local machine or
// a Manager-owned Docker executor.
func (j JWTAuth) MintScopedWithTarget(sub, room, deviceID, sessionID, executionTarget, role, access, relayNode string, ttl time.Duration) (string, error) {
	return j.MintScopedWithTargetAndGeneration(sub, room, deviceID, sessionID, executionTarget, role, access, relayNode, 0, ttl)
}

// MintScopedWithTargetAndGeneration is used by Relay-issued invite tokens to
// stay on the same fenced routing epoch as their verified host credential.
func (j JWTAuth) MintScopedWithTargetAndGeneration(sub, room, deviceID, sessionID, executionTarget, role, access, relayNode string, relayGeneration int64, ttl time.Duration) (string, error) {
	return j.mintIdentity(sub, Identity{Room: room, DeviceID: deviceID, SessionID: sessionID, ExecutionTarget: executionTarget, RelayNode: relayNode, RelayGeneration: relayGeneration}, role, access, ttl)
}

// MintInviteParticipant copies the verified host's complete routing scope.
// This keeps invited participants visible to Control Plane presence tracking
// and subjects them to the same pool/node/generation checks as the host.
func (j JWTAuth) MintInviteParticipant(sub string, host Identity, access string, ttl time.Duration) (string, error) {
	return j.mintIdentity(sub, host, RoleParticipant, access, ttl)
}

func (j JWTAuth) mintIdentity(sub string, scope Identity, role, access string, ttl time.Duration) (string, error) {
	if role != "" && role != RoleHost && role != RoleParticipant {
		return "", errInvalidToken
	}
	if access != "" && access != AccessView && access != AccessControl {
		return "", errInvalidToken
	}
	if (scope.DeviceID == "") != (scope.SessionID == "") || scope.DeviceID != "" && (!streamIDPattern.MatchString(scope.DeviceID) || !streamIDPattern.MatchString(scope.SessionID)) {
		return "", errInvalidToken
	}
	if scope.ExecutionTarget != "" && scope.ExecutionTarget != "local" && scope.ExecutionTarget != "docker" {
		return "", errInvalidToken
	}
	if scope.RelayGeneration < 0 {
		return "", errInvalidToken
	}
	contextValues := []string{scope.ApplicationID, scope.TenantID, scope.ResourceType, scope.ResourceID}
	contextCount := 0
	for _, value := range contextValues {
		if value != "" {
			contextCount++
		}
		if value != "" && !streamIDPattern.MatchString(value) {
			return "", errInvalidToken
		}
	}
	if contextCount != 0 && (contextCount != len(contextValues) || scope.PoolID == "" || scope.RelayNode == "" || scope.RelayGeneration < 1) {
		return "", errInvalidToken
	}
	for _, value := range []string{scope.PoolID, scope.AgentID} {
		if value != "" && !streamIDPattern.MatchString(value) {
			return "", errInvalidToken
		}
	}
	if scope.Protocol != "" && scope.Protocol != "terminal" && scope.Protocol != "acp" {
		return "", errInvalidToken
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"sub": sub,
		"iat": now.Unix(),
		"exp": now.Add(ttl).Unix(),
	}
	if role != "" {
		tokenID, err := j.newTokenID()
		if err != nil {
			return "", errInvalidToken
		}
		claims["iss"] = relayTokenIssuer
		claims["aud"] = relayTokenAudience
		claims["jti"] = tokenID
		capabilities := []string{}
		switch role {
		case RoleHost:
			capabilities = []string{CapabilityHostConnect, CapabilityParticipantConnect, CapabilityInviteCreate, CapabilityTerminalView, CapabilityTerminalControl}
		case RoleParticipant:
			capabilities = []string{CapabilityParticipantConnect, CapabilityTerminalView}
			if access != AccessView {
				capabilities = append(capabilities, CapabilityTerminalControl)
			}
		}
		claims["cap"] = capabilities
	}
	if scope.Room != "" {
		claims["room"] = scope.Room
	}
	if role != "" {
		claims["role"] = role
	}
	if access != "" {
		claims["access"] = access
	}
	if scope.DeviceID != "" {
		claims["device_id"] = scope.DeviceID
		claims["session_id"] = scope.SessionID
	}
	if scope.ExecutionTarget != "" {
		claims["execution_target"] = scope.ExecutionTarget
	}
	if scope.RelayNode != "" {
		claims["relay_node"] = scope.RelayNode
	}
	if scope.RelayGeneration > 0 {
		claims["relay_generation"] = scope.RelayGeneration
	}
	if contextCount == len(contextValues) && contextCount > 0 {
		claims["relay_version"] = relayContextVersion
		claims["app_id"] = scope.ApplicationID
		claims["pool_id"] = scope.PoolID
		claims["tenant_id"] = scope.TenantID
		claims["resource_type"] = scope.ResourceType
		claims["resource_id"] = scope.ResourceID
		if scope.AgentID != "" {
			claims["agent_id"] = scope.AgentID
		}
		if scope.Protocol != "" {
			claims["protocol"] = scope.Protocol
		}
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(j.Secret)
}

func (j JWTAuth) newTokenID() (string, error) {
	reader := j.Random
	if reader == nil {
		reader = rand.Reader
	}
	var raw [16]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (j JWTAuth) AgentUser(_ context.Context, token string) (Identity, error) {
	id, err := j.Verify(token)
	if err != nil || !id.Allows(CapabilityHostConnect) {
		return Identity{}, errInvalidToken
	}
	return id, nil
}

func (j JWTAuth) ParticipantUser(_ context.Context, token string) (Identity, error) {
	id, err := j.Verify(token)
	if err != nil || !id.Allows(CapabilityParticipantConnect) || !id.Allows(CapabilityTerminalView) {
		return Identity{}, errInvalidToken
	}
	// The access claim is convenient presentation metadata, never an authority
	// escalation. A scoped token missing terminal:control is forcibly view-only
	// even if a buggy/external issuer stamped access=control.
	if id.Access == AccessControl && !id.Allows(CapabilityTerminalControl) {
		id.Access = AccessView
	}
	return id, nil
}
