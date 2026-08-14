package capability

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const RelayProtocolVersion = 3

var scopeValuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

type Issuer struct {
	Secret        []byte
	RoutingSecret []byte
	Now           func() time.Time
	Random        io.Reader
}

type SessionCredential struct {
	Subject         string
	Room            string
	DeviceID        string
	SessionID       string
	ApplicationID   string
	PoolID          string
	TenantID        string
	ResourceType    string
	ResourceID      string
	AgentID         string
	Protocol        string
	ExecutionTarget string
	Role            string
	Access          string
	RelayNode       string
	RelayGeneration int64
	TTL             time.Duration
	AllowInvite     bool
}

type Minted struct {
	Token           string    `json:"token"`
	ProtocolVersion int       `json:"protocolVersion"`
	Room            string    `json:"room"`
	DeviceID        string    `json:"deviceId"`
	SessionID       string    `json:"sessionId"`
	ApplicationID   string    `json:"applicationId,omitempty"`
	PoolID          string    `json:"poolId,omitempty"`
	TenantID        string    `json:"tenantId,omitempty"`
	ResourceType    string    `json:"resourceType,omitempty"`
	ResourceID      string    `json:"resourceId,omitempty"`
	AgentID         string    `json:"agentId,omitempty"`
	Protocol        string    `json:"protocol,omitempty"`
	ExecutionTarget string    `json:"executionTarget,omitempty"`
	Role            string    `json:"role"`
	Access          string    `json:"access"`
	RelayNode       string    `json:"relayNode,omitempty"`
	RelayGeneration int64     `json:"relayGeneration,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

func (i Issuer) Enabled() bool { return len(i.Secret) >= 32 }

func (i Issuer) MintSession(value SessionCredential) (Minted, error) {
	if !i.Enabled() {
		return Minted{}, errors.New("relay capability issuer is not configured")
	}
	if value.Subject == "" || value.Room == "" || value.DeviceID == "" || value.SessionID == "" {
		return Minted{}, errors.New("session credential scope is required")
	}
	if !validOptionalScope(value.ApplicationID) || !validOptionalScope(value.PoolID) ||
		!validOptionalScope(value.TenantID) || !validOptionalScope(value.ResourceType) ||
		!validOptionalScope(value.ResourceID) || !validOptionalScope(value.AgentID) {
		return Minted{}, errors.New("invalid relay session context")
	}
	if value.Protocol != "" && value.Protocol != "terminal" && value.Protocol != "acp" {
		return Minted{}, errors.New("invalid relay protocol")
	}
	if hasResourceContext(value) {
		if !completeResourceContext(value) {
			return Minted{}, errors.New("incomplete relay resource context")
		}
		if value.RelayGeneration < 1 {
			return Minted{}, errors.New("relay generation is required for resource-scoped sessions")
		}
		room, err := i.OpaqueRoom(value.ApplicationID, value.TenantID, value.ResourceType, value.ResourceID, value.SessionID)
		if err != nil {
			return Minted{}, err
		}
		value.Room = room
	}
	if value.Role != "host" && value.Role != "participant" {
		return Minted{}, errors.New("invalid session role")
	}
	if value.ExecutionTarget != "" && value.ExecutionTarget != "local" && value.ExecutionTarget != "docker" {
		return Minted{}, errors.New("invalid execution target")
	}
	if value.Access != "view" && value.Access != "control" {
		return Minted{}, errors.New("invalid session access")
	}
	if value.Role == "host" {
		value.Access = "control"
	}
	if value.TTL <= 0 {
		if value.Role == "host" {
			value.TTL = 24 * time.Hour
		} else {
			value.TTL = time.Hour
		}
	}
	now := time.Now()
	if i.Now != nil {
		now = i.Now()
	}
	expires := now.Add(value.TTL)
	tokenID, err := i.randomID()
	if err != nil {
		return Minted{}, errors.New("could not generate relay credential id")
	}
	capabilities := []string{"relay:participant:connect", "terminal:view"}
	if value.Role == "host" {
		capabilities = []string{"relay:host:connect", "relay:participant:connect", "terminal:view", "terminal:control"}
		if value.AllowInvite {
			capabilities = append(capabilities, "relay:invite:create")
		}
	} else if value.Access == "control" {
		capabilities = append(capabilities, "terminal:control")
	}
	claims := jwt.MapClaims{
		"iss": "pie-control", "aud": "pie-relay", "sub": value.Subject, "room": value.Room,
		"device_id": value.DeviceID, "session_id": value.SessionID, "role": value.Role, "access": value.Access,
		"cap": capabilities, "jti": tokenID, "relay_version": RelayProtocolVersion,
		"iat": now.Unix(), "nbf": now.Add(-5 * time.Second).Unix(), "exp": expires.Unix(),
	}
	optionalClaims := map[string]string{
		"app_id": value.ApplicationID, "pool_id": value.PoolID, "tenant_id": value.TenantID,
		"resource_type": value.ResourceType, "resource_id": value.ResourceID,
		"agent_id": value.AgentID, "protocol": value.Protocol,
	}
	for name, claim := range optionalClaims {
		if claim != "" {
			claims[name] = claim
		}
	}
	if value.ExecutionTarget != "" {
		claims["execution_target"] = value.ExecutionTarget
	}
	if value.RelayNode != "" {
		claims["relay_node"] = value.RelayNode
	}
	if value.RelayGeneration > 0 {
		claims["relay_generation"] = value.RelayGeneration
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(i.Secret)
	if err != nil {
		return Minted{}, err
	}
	return Minted{
		Token: signed, ProtocolVersion: RelayProtocolVersion, Room: value.Room,
		DeviceID: value.DeviceID, SessionID: value.SessionID, ApplicationID: value.ApplicationID,
		PoolID: value.PoolID, TenantID: value.TenantID, ResourceType: value.ResourceType,
		ResourceID: value.ResourceID, AgentID: value.AgentID, Protocol: value.Protocol,
		ExecutionTarget: value.ExecutionTarget, Role: value.Role, Access: value.Access,
		RelayNode: value.RelayNode, RelayGeneration: value.RelayGeneration, ExpiresAt: expires,
	}, nil
}

func validOptionalScope(value string) bool {
	return value == "" || scopeValuePattern.MatchString(value)
}

func hasResourceContext(value SessionCredential) bool {
	return value.ApplicationID != "" || value.TenantID != "" || value.ResourceType != "" || value.ResourceID != ""
}

func completeResourceContext(value SessionCredential) bool {
	return value.ApplicationID != "" && value.TenantID != "" && value.ResourceType != "" && value.ResourceID != ""
}

// OpaqueRoom creates a stable routing key without exposing tenant or resource
// identifiers in Relay logs and in-memory room maps. The Relay never needs to
// reverse this value; only the Control Plane owns the HMAC secret.
func (i Issuer) OpaqueRoom(applicationID, tenantID, resourceType, resourceID, sessionID string) (string, error) {
	if len(i.RoutingSecret) < 32 {
		return "", errors.New("relay routing secret is not configured")
	}
	for _, value := range []string{applicationID, tenantID, resourceType, resourceID, sessionID} {
		if !scopeValuePattern.MatchString(value) {
			return "", errors.New("invalid relay routing scope")
		}
	}
	mac := hmac.New(sha256.New, i.RoutingSecret)
	_, _ = mac.Write([]byte(applicationID))
	for _, value := range []string{tenantID, resourceType, resourceID, sessionID} {
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(value))
	}
	return "r_v2_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (i Issuer) randomID() (string, error) {
	reader := i.Random
	if reader == nil {
		reader = rand.Reader
	}
	var raw [16]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
