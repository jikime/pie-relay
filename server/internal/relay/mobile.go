package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	mobileInviteTTL       = 5 * time.Minute
	mobileControlLease    = 30 * time.Minute
	mobileResumeTTL       = 30 * 24 * time.Hour
	mobileGraceTTL        = 24 * time.Hour
	mobileAttachDeadline  = 10 * time.Second
	mobileMaxMessageBytes = 1 << 20
)

const (
	mobileCloseBadCredential = websocket.StatusCode(4401)
	mobileCloseHostOffline   = websocket.StatusCode(4404)
	mobileClosePeerDropped   = websocket.StatusCode(4408)
	mobileCloseLimitExceeded = websocket.StatusCode(4429)
)

// MobileRelayOptions configures the Pie Relay mobile Director/Cell-compatible
// endpoints. PublicURL must be the HTTP(S) origin advertised to desktop and
// mobile clients. StateFile is optional; when present, only resume-token hashes
// and their metadata are persisted (never raw bearer credentials).
type MobileRelayOptions struct {
	Auth                          AgentAuthenticator
	PublicURL                     string
	StateFile                     string
	AllowUnsupportedStateFileMode bool
	Now                           func() time.Time
	Random                        io.Reader
}

// MobileRelay implements the subset of the Orca mobile Director/Cell contract
// used by the vendored Pie Relay apps. The relay only sees outer credentials
// and opaque E2EE frames; terminal/RPC plaintext remains end-to-end encrypted.
type MobileRelay struct {
	auth                          AgentAuthenticator
	publicURL                     string
	stateFile                     string
	allowUnsupportedStateFileMode bool
	chmodFile                     func(string, os.FileMode) error
	now                           func() time.Time
	random                        io.Reader

	mu          sync.Mutex
	hosts       map[string]*mobileHost
	invites     map[[32]byte]*mobileInvite
	credentials map[string]map[string]*mobileCredential
	installs    map[string]mobileInstallRecord
	connections map[string]*mobileConnection
}

type mobileHost struct {
	identity            Identity
	publicKeyB64        string
	control             *mobilePeer
	generation          int64
	controlResumeSecret string
	leaseExpiresAt      time.Time
}

type mobileInvite struct {
	hostID      string
	deviceID    string
	expiresAt   time.Time
	attempts    int
	maxAttempts int
}

type mobileCredential struct {
	CurrentHash     string    `json:"currentHash"`
	CurrentVersion  int       `json:"currentVersion"`
	ResumeExpiresAt time.Time `json:"resumeExpiresAt"`
	GraceHash       string    `json:"graceHash,omitempty"`
	GraceVersion    int       `json:"graceVersion,omitempty"`
	GraceExpiresAt  time.Time `json:"graceExpiresAt,omitempty"`
}

type mobileInstallRecord struct {
	HostID   string
	DeviceID string
	Result   mobileCredentialInstalled
}

type mobileConnection struct {
	id                string
	ticket            string
	hostID            string
	deviceID          string
	kind              string
	generation        int64
	credentialVersion int
	acceptedAs        string
	leaseExpiresAt    time.Time

	mu           sync.Mutex
	phone        *mobilePeer
	host         *mobilePeer
	hostAttached chan struct{}
	attachOnce   sync.Once
	done         chan struct{}
	closeOnce    sync.Once
}

type mobilePeer struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

type mobileCredentialState struct {
	Hosts map[string]map[string]*mobileCredential `json:"hosts"`
}

type mobileCredentialInstalled struct {
	V                 int    `json:"v"`
	ReqID             string `json:"reqId"`
	AuthorizationMode string `json:"authorizationMode"`
	CurrentVersion    int    `json:"currentVersion"`
	ResumeExpiresAt   int64  `json:"resumeExpiresAt"`
	GraceExpiresAt    *int64 `json:"graceExpiresAt,omitempty"`
}

func NewMobileRelay(options MobileRelayOptions) (*MobileRelay, error) {
	if options.Auth == nil {
		return nil, errors.New("mobile relay auth is required")
	}
	publicURL := strings.TrimSuffix(strings.TrimSpace(options.PublicURL), "/")
	if publicURL != "" {
		parsed, err := url.Parse(publicURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" {
			return nil, errors.New("mobile relay public URL must be an HTTP(S) origin")
		}
	}
	m := &MobileRelay{
		auth:                          options.Auth,
		publicURL:                     publicURL,
		stateFile:                     strings.TrimSpace(options.StateFile),
		allowUnsupportedStateFileMode: options.AllowUnsupportedStateFileMode,
		chmodFile:                     os.Chmod,
		now:                           options.Now,
		random:                        options.Random,
		hosts:                         map[string]*mobileHost{},
		invites:                       map[[32]byte]*mobileInvite{},
		credentials:                   map[string]map[string]*mobileCredential{},
		installs:                      map[string]mobileInstallRecord{},
		connections:                   map[string]*mobileConnection{},
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.random == nil {
		m.random = rand.Reader
	}
	if err := m.loadState(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *MobileRelay) register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/assign", m.handleAssign)
	mux.HandleFunc("/v1/identity", m.handleIdentity)
	mux.HandleFunc("/v1/resolve", m.handleResolve)
	mux.HandleFunc("/v1/host/control", m.handleHostControl)
	mux.HandleFunc("/v1/host/data/", m.handleHostData)
	mux.HandleFunc("/v1/connect/", m.handlePhone)
}

func (m *MobileRelay) handleIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, err := m.authenticateHost(r)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	profileID := identity.ApplicationID
	if profileID == "" {
		profileID = "pie-relay"
	}
	organizationID := identity.TenantID
	if organizationID == "" {
		organizationID = identity.UserID
	}
	writeJSON(w, map[string]any{
		"userId": identity.UserID, "profileId": profileID, "organizationId": organizationID,
		"applicationId": identity.ApplicationID, "poolId": identity.PoolID,
	})
}

func (m *MobileRelay) advertisedOrigin(r *http.Request) (string, error) {
	if m.publicURL != "" {
		return m.publicURL, nil
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "https" {
		scheme = "https"
	}
	if r.Host == "" {
		return "", errors.New("request host is empty")
	}
	return scheme + "://" + r.Host, nil
}

func (m *MobileRelay) authenticateHost(r *http.Request) (Identity, error) {
	token := bearerToken(r)
	if token == "" {
		return Identity{}, errInvalidToken
	}
	identity, err := m.auth.AgentUser(r.Context(), token)
	if err != nil || identity.UserID == "" || identity.Role == RoleParticipant {
		return Identity{}, errInvalidToken
	}
	return identity, nil
}

func (m *MobileRelay) handleAssign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := m.authenticateHost(r); err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	var request struct {
		V           int    `json:"v"`
		RelayHostID string `json:"relayHostId"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&request) != nil || request.V != 1 || !validMobileHostID(request.RelayHostID) {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	origin, err := m.advertisedOrigin(r)
	if err != nil {
		http.Error(w, "mobile relay public URL unavailable", http.StatusServiceUnavailable)
		return
	}
	lease, err := m.randomBase64URL(32)
	if err != nil {
		http.Error(w, "could not create assignment", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"v": 1, "cellUrl": origin, "assignmentEpoch": 1, "lease": lease,
	})
}

func (m *MobileRelay) handleResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		V           int    `json:"v"`
		RelayHostID string `json:"relayHostId"`
		ResumeToken string `json:"resumeToken"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&request) != nil || request.V != 1 {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	_, _, credential, ok := m.resolveResumeLocked(request.RelayHostID, request.ResumeToken)
	m.mu.Unlock()
	if !ok {
		http.Error(w, "invalid resume credential", http.StatusUnauthorized)
		return
	}
	origin, err := m.advertisedOrigin(r)
	if err != nil {
		http.Error(w, "mobile relay public URL unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{
		"v": 1, "cellUrl": origin, "assignmentEpoch": 1,
		"leaseExpiresAt": credential.ResumeExpiresAt.UnixMilli(),
	})
}

func (m *MobileRelay) handleHostControl(w http.ResponseWriter, r *http.Request) {
	identity, err := m.authenticateHost(r)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 10)
	peer := &mobilePeer{conn: conn}
	ctx := r.Context()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "host hello required")
		return
	}
	var hello struct {
		Type                string `json:"type"`
		V                   int    `json:"v"`
		RelayHostID         string `json:"relayHostId"`
		AssignmentEpoch     int64  `json:"assignmentEpoch"`
		HostPublicKeyB64    string `json:"hostPublicKeyB64"`
		PreviousGeneration  *int64 `json:"previousGeneration,omitempty"`
		ControlResumeSecret string `json:"controlResumeSecret,omitempty"`
	}
	if json.Unmarshal(raw, &hello) != nil || hello.Type != "host-hello" || hello.V != 1 || hello.AssignmentEpoch != 1 || !mobileHostKeyMatches(hello.RelayHostID, hello.HostPublicKeyB64) {
		_ = conn.Close(mobileCloseBadCredential, "invalid host hello")
		return
	}

	now := m.now()
	resumeSecret, err := m.randomBase64URL(32)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "random source unavailable")
		return
	}
	var oldControl *mobilePeer
	var generation int64
	var pending []map[string]string
	var active []string
	m.mu.Lock()
	previous := m.hosts[hello.RelayHostID]
	if previous != nil && hello.PreviousGeneration != nil && *hello.PreviousGeneration == previous.generation && subtle.ConstantTimeCompare([]byte(hello.ControlResumeSecret), []byte(previous.controlResumeSecret)) == 1 {
		generation = previous.generation
	} else if previous != nil {
		generation = previous.generation + 1
	} else {
		generation = 1
	}
	if previous != nil {
		oldControl = previous.control
	}
	host := &mobileHost{
		identity: identity, publicKeyB64: hello.HostPublicKeyB64, control: peer,
		generation: generation, controlResumeSecret: resumeSecret,
		leaseExpiresAt: now.Add(mobileControlLease),
	}
	m.hosts[hello.RelayHostID] = host
	for _, candidate := range m.connections {
		if candidate.hostID != hello.RelayHostID || candidate.generation != generation {
			continue
		}
		candidate.mu.Lock()
		attached := candidate.host != nil
		candidate.mu.Unlock()
		if attached {
			active = append(active, candidate.id)
		} else {
			pending = append(pending, map[string]string{"connId": candidate.id, "connTicket": candidate.ticket})
		}
	}
	m.mu.Unlock()
	if oldControl != nil && oldControl != peer {
		_ = oldControl.close(mobileClosePeerDropped, "control replaced")
	}
	if len(active) > 8 {
		active = active[:8]
	}
	if len(pending) > 8 {
		pending = pending[:8]
	}
	if err := peer.writeJSON(ctx, map[string]any{
		"type": "host-hello-ack", "v": 1, "generation": generation,
		"controlResumeSecret": resumeSecret, "leaseExpiresAt": host.leaseExpiresAt.UnixMilli(),
		"activeConnIds": active, "pendingConns": pending,
	}); err != nil {
		m.clearControl(hello.RelayHostID, peer)
		return
	}

	defer func() {
		m.clearControl(hello.RelayHostID, peer)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()
	for {
		messageType, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText || !m.handleControlMessage(ctx, hello.RelayHostID, host, data) {
			_ = conn.Close(websocket.StatusPolicyViolation, "invalid control message")
			return
		}
	}
}

func (m *MobileRelay) clearControl(hostID string, peer *mobilePeer) {
	m.mu.Lock()
	if host := m.hosts[hostID]; host != nil && host.control == peer {
		host.control = nil
	}
	m.mu.Unlock()
}

func (m *MobileRelay) handleControlMessage(ctx context.Context, hostID string, host *mobileHost, raw []byte) bool {
	var base struct {
		Type  string `json:"type"`
		ReqID string `json:"reqId"`
	}
	if json.Unmarshal(raw, &base) != nil || base.Type == "" {
		return false
	}
	switch base.Type {
	case "pong":
		return true
	case "auth-refresh":
		var request struct {
			Type     string `json:"type"`
			RelayJWT string `json:"relayJwt"`
		}
		if json.Unmarshal(raw, &request) != nil {
			return false
		}
		identity, err := m.auth.AgentUser(ctx, request.RelayJWT)
		return err == nil && identity.UserID == host.identity.UserID && identity.Role != RoleParticipant
	case "invite-create":
		var request struct {
			Type          string `json:"type"`
			ReqID         string `json:"reqId"`
			RelayDeviceID string `json:"relayDeviceId"`
		}
		if json.Unmarshal(raw, &request) != nil || request.ReqID == "" || request.RelayDeviceID == "" {
			return false
		}
		token, err := m.randomBase64URL(32)
		if err != nil {
			return false
		}
		expires := m.now().Add(mobileInviteTTL)
		m.mu.Lock()
		m.invites[sha256.Sum256([]byte(token))] = &mobileInvite{hostID: hostID, deviceID: request.RelayDeviceID, expiresAt: expires, maxAttempts: 8}
		m.mu.Unlock()
		return host.control.writeJSON(ctx, map[string]any{
			"type": "invite-created", "reqId": request.ReqID, "inviteToken": token,
			"expiresAt": expires.UnixMilli(), "maxAttempts": 8,
		}) == nil
	case "device-credential-install":
		return m.handleCredentialInstall(ctx, hostID, host, raw)
	case "device-credential-install-status":
		var request struct {
			Type          string `json:"type"`
			V             int    `json:"v"`
			ReqID         string `json:"reqId"`
			RelayDeviceID string `json:"relayDeviceId"`
		}
		if json.Unmarshal(raw, &request) != nil || request.V != 1 || request.ReqID == "" {
			return false
		}
		m.mu.Lock()
		record, found := m.installs[installKey(hostID, request.RelayDeviceID, request.ReqID)]
		m.mu.Unlock()
		if !found {
			return host.control.writeJSON(ctx, map[string]any{"type": "device-credential-install-status-result", "v": 1, "reqId": request.ReqID, "state": "not-found"}) == nil
		}
		return host.control.writeJSON(ctx, map[string]any{"type": "device-credential-install-status-result", "v": 1, "reqId": request.ReqID, "state": "committed", "result": record.Result}) == nil
	case "device-resume-confirm":
		var request struct {
			Type        string `json:"type"`
			V           int    `json:"v"`
			ReqID       string `json:"reqId"`
			BasisConnID string `json:"basisConnId"`
		}
		if json.Unmarshal(raw, &request) != nil || request.V != 1 || request.ReqID == "" {
			return false
		}
		m.mu.Lock()
		connection := m.connections[request.BasisConnID]
		m.mu.Unlock()
		if connection == nil || connection.hostID != hostID || connection.kind != "resume" {
			return host.control.writeJSON(ctx, map[string]any{"type": "control-error", "reqId": request.ReqID, "code": "resume_basis_not_found"}) == nil
		}
		return host.control.writeJSON(ctx, map[string]any{
			"type": "device-resume-confirmed", "v": 1, "reqId": request.ReqID,
			"currentVersion": connection.credentialVersion, "acceptedAs": connection.acceptedAs,
			"renewed": false, "resumeExpiresAt": connection.leaseExpiresAt.UnixMilli(),
		}) == nil
	case "device-revoke":
		var request struct {
			Type          string `json:"type"`
			ReqID         string `json:"reqId"`
			RelayDeviceID string `json:"relayDeviceId"`
		}
		if json.Unmarshal(raw, &request) != nil || request.ReqID == "" || request.RelayDeviceID == "" {
			return false
		}
		m.revokeDevice(hostID, request.RelayDeviceID)
		return host.control.writeJSON(ctx, map[string]any{"type": "device-revoked", "reqId": request.ReqID}) == nil
	default:
		return false
	}
}

func (m *MobileRelay) handleCredentialInstall(ctx context.Context, hostID string, host *mobileHost, raw []byte) bool {
	var request struct {
		Type                string `json:"type"`
		V                   int    `json:"v"`
		ReqID               string `json:"reqId"`
		RelayDeviceID       string `json:"relayDeviceId"`
		NewResumeTokenHash  string `json:"newResumeTokenHash"`
		ExpectedCurrentHash string `json:"expectedCurrentHash,omitempty"`
		Authorization       struct {
			Mode         string `json:"mode"`
			BasisConnID  string `json:"basisConnId,omitempty"`
			DirectAuthID string `json:"directAuthId,omitempty"`
		} `json:"authorization"`
	}
	if json.Unmarshal(raw, &request) != nil || request.V != 1 || request.ReqID == "" || request.RelayDeviceID == "" || !validBase64URL32(request.NewResumeTokenHash) {
		return false
	}
	if request.Authorization.Mode != "authenticated-direct" && request.Authorization.Mode != "relay-basis" {
		return false
	}
	if request.Authorization.Mode == "relay-basis" {
		m.mu.Lock()
		basis := m.connections[request.Authorization.BasisConnID]
		m.mu.Unlock()
		if basis == nil || basis.hostID != hostID || basis.deviceID != request.RelayDeviceID || basis.kind != "invite" {
			return host.control.writeJSON(ctx, map[string]any{"type": "control-error", "reqId": request.ReqID, "code": "invalid_relay_basis"}) == nil
		}
	} else if request.Authorization.DirectAuthID == "" {
		return false
	}

	now := m.now()
	var result mobileCredentialInstalled
	m.mu.Lock()
	if existing, ok := m.installs[installKey(hostID, request.RelayDeviceID, request.ReqID)]; ok {
		result = existing.Result
		m.mu.Unlock()
		return writeCredentialInstalled(ctx, host.control, result)
	}
	byDevice := m.credentials[hostID]
	if byDevice == nil {
		byDevice = map[string]*mobileCredential{}
		m.credentials[hostID] = byDevice
	}
	previous := byDevice[request.RelayDeviceID]
	if request.ExpectedCurrentHash != "" && (previous == nil || subtle.ConstantTimeCompare([]byte(previous.CurrentHash), []byte(request.ExpectedCurrentHash)) != 1) {
		m.mu.Unlock()
		return host.control.writeJSON(ctx, map[string]any{"type": "control-error", "reqId": request.ReqID, "code": "credential_conflict"}) == nil
	}
	version := 1
	credential := &mobileCredential{CurrentHash: request.NewResumeTokenHash, CurrentVersion: version, ResumeExpiresAt: now.Add(mobileResumeTTL)}
	if previous != nil {
		version = previous.CurrentVersion + 1
		credential.CurrentVersion = version
		credential.GraceHash = previous.CurrentHash
		credential.GraceVersion = previous.CurrentVersion
		credential.GraceExpiresAt = now.Add(mobileGraceTTL)
	}
	byDevice[request.RelayDeviceID] = credential
	result = mobileCredentialInstalled{V: 1, ReqID: request.ReqID, AuthorizationMode: request.Authorization.Mode, CurrentVersion: version, ResumeExpiresAt: credential.ResumeExpiresAt.UnixMilli()}
	if !credential.GraceExpiresAt.IsZero() {
		value := credential.GraceExpiresAt.UnixMilli()
		result.GraceExpiresAt = &value
	}
	m.installs[installKey(hostID, request.RelayDeviceID, request.ReqID)] = mobileInstallRecord{HostID: hostID, DeviceID: request.RelayDeviceID, Result: result}
	err := m.saveStateLocked()
	if err != nil {
		// Keep memory and disk consistent. Otherwise a retry with the same reqId
		// would report a successful install even though the credential was never
		// persisted and would be lost at the next Relay restart.
		if previous == nil {
			delete(byDevice, request.RelayDeviceID)
			if len(byDevice) == 0 {
				delete(m.credentials, hostID)
			}
		} else {
			byDevice[request.RelayDeviceID] = previous
		}
		delete(m.installs, installKey(hostID, request.RelayDeviceID, request.ReqID))
	}
	m.mu.Unlock()
	if err != nil {
		log.Printf("relay: persist mobile credential hash state: %v", err)
		return host.control.writeJSON(ctx, map[string]any{"type": "control-error", "reqId": request.ReqID, "code": "credential_store_failed"}) == nil
	}
	return writeCredentialInstalled(ctx, host.control, result)
}

func writeCredentialInstalled(ctx context.Context, control *mobilePeer, result mobileCredentialInstalled) bool {
	response := map[string]any{
		"type":              "device-credential-installed",
		"v":                 result.V,
		"reqId":             result.ReqID,
		"authorizationMode": result.AuthorizationMode,
		"currentVersion":    result.CurrentVersion,
		"resumeExpiresAt":   result.ResumeExpiresAt,
	}
	if result.GraceExpiresAt != nil {
		response["graceExpiresAt"] = *result.GraceExpiresAt
	}
	return control.writeJSON(ctx, response) == nil
}

func (m *MobileRelay) handlePhone(w http.ResponseWriter, r *http.Request) {
	hostID := strings.TrimPrefix(r.URL.Path, "/v1/connect/")
	if !validMobileHostID(hostID) || strings.Contains(hostID, "/") {
		http.NotFound(w, r)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(mobileMaxMessageBytes)
	phone := &mobilePeer{conn: conn}
	ctx := r.Context()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		_ = conn.Close(mobileCloseBadCredential, "relay auth required")
		return
	}
	var auth struct {
		Type       string `json:"type"`
		V          int    `json:"v"`
		Mode       string `json:"mode"`
		Credential string `json:"credential"`
	}
	if json.Unmarshal(raw, &auth) != nil || auth.Type != "relay-auth" || auth.V != 1 || auth.Mode != "connect" || !validBase64URL32(auth.Credential) {
		_ = phone.writeJSON(ctx, map[string]any{"type": "relay-hello", "ok": false, "code": int(mobileCloseBadCredential)})
		_ = conn.Close(mobileCloseBadCredential, "invalid relay credential")
		return
	}

	now := m.now()
	connectionID, err := m.randomBase64URL(18)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "random source unavailable")
		return
	}
	ticket, err := m.randomBase64URL(32)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "random source unavailable")
		return
	}
	connection := &mobileConnection{id: connectionID, ticket: ticket, hostID: hostID, phone: phone, hostAttached: make(chan struct{}), done: make(chan struct{})}
	var control *mobilePeer
	m.mu.Lock()
	host := m.hosts[hostID]
	if host != nil {
		control = host.control
		connection.generation = host.generation
	}
	if control == nil {
		m.mu.Unlock()
		_ = phone.writeJSON(ctx, map[string]any{"type": "relay-hello", "ok": false, "code": int(mobileCloseHostOffline)})
		_ = conn.Close(mobileCloseHostOffline, "host offline")
		return
	}
	if invite, ok := m.resolveInviteLocked(hostID, auth.Credential, now); ok {
		connection.deviceID = invite.deviceID
		connection.kind = "invite"
		connection.leaseExpiresAt = invite.expiresAt
	} else if deviceID, acceptedAs, credential, ok := m.resolveResumeLocked(hostID, auth.Credential); ok {
		connection.deviceID = deviceID
		connection.kind = "resume"
		connection.acceptedAs = acceptedAs
		if acceptedAs == "current" {
			connection.leaseExpiresAt = credential.ResumeExpiresAt
			connection.credentialVersion = credential.CurrentVersion
		} else {
			connection.leaseExpiresAt = credential.GraceExpiresAt
			connection.credentialVersion = credential.GraceVersion
		}
	} else {
		m.mu.Unlock()
		_ = phone.writeJSON(ctx, map[string]any{"type": "relay-hello", "ok": false, "code": int(mobileCloseBadCredential)})
		_ = conn.Close(mobileCloseBadCredential, "invalid relay credential")
		return
	}
	if len(m.connections) >= 1024 {
		m.mu.Unlock()
		_ = conn.Close(mobileCloseLimitExceeded, "relay capacity reached")
		return
	}
	m.connections[connection.id] = connection
	m.mu.Unlock()

	defer m.closeConnection(connection, mobileClosePeerDropped, "peer dropped")
	if err := control.writeJSON(ctx, map[string]any{
		"type": "conn-open", "connId": connection.id, "connTicket": connection.ticket,
		"kind": connection.kind, "relayDeviceId": connection.deviceID,
		"attachDeadlineMs": mobileAttachDeadline.Milliseconds(),
	}); err != nil {
		_ = conn.Close(mobileCloseHostOffline, "host control unavailable")
		return
	}
	select {
	case <-connection.hostAttached:
	case <-time.After(mobileAttachDeadline):
		_ = conn.Close(mobileCloseHostOffline, "host attach timeout")
		return
	case <-ctx.Done():
		return
	}
	if connection.kind == "invite" {
		if phone.writeJSON(ctx, map[string]any{"type": "relay-hello", "ok": true, "credentialKind": "invite", "leaseExpiresAt": connection.leaseExpiresAt.UnixMilli()}) != nil {
			return
		}
	} else {
		if phone.writeJSON(ctx, map[string]any{
			"type": "relay-hello", "ok": true, "credentialKind": "resume",
			"leaseExpiresAt":            now.Add(mobileControlLease).UnixMilli(),
			"acceptedCredentialVersion": connection.credentialVersion,
			"acceptedAs":                connection.acceptedAs,
			"resumeExpiresAt":           connection.leaseExpiresAt.UnixMilli(),
		}) != nil {
			return
		}
	}
	m.bridge(connection, ctx)
}

func (m *MobileRelay) handleHostData(w http.ResponseWriter, r *http.Request) {
	connectionID := strings.TrimPrefix(r.URL.Path, "/v1/host/data/")
	if connectionID == "" || strings.Contains(connectionID, "/") {
		http.NotFound(w, r)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(mobileMaxMessageBytes)
	peer := &mobilePeer{conn: conn}
	ctx := r.Context()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		_ = conn.Close(mobileCloseBadCredential, "host data auth required")
		return
	}
	var auth struct {
		Type       string `json:"type"`
		V          int    `json:"v"`
		ConnTicket string `json:"connTicket"`
		Generation int64  `json:"generation"`
	}
	if json.Unmarshal(raw, &auth) != nil || auth.Type != "host-data-auth" || auth.V != 1 {
		_ = conn.Close(mobileCloseBadCredential, "invalid host data auth")
		return
	}
	m.mu.Lock()
	connection := m.connections[connectionID]
	m.mu.Unlock()
	if connection == nil || auth.Generation != connection.generation || subtle.ConstantTimeCompare([]byte(auth.ConnTicket), []byte(connection.ticket)) != 1 {
		_ = conn.Close(mobileCloseBadCredential, "invalid connection ticket")
		return
	}
	connection.mu.Lock()
	if connection.host != nil {
		connection.mu.Unlock()
		_ = conn.Close(mobileClosePeerDropped, "host data already attached")
		return
	}
	connection.host = peer
	connection.mu.Unlock()
	connection.attachOnce.Do(func() { close(connection.hostAttached) })
	select {
	case <-connection.done:
	case <-ctx.Done():
		m.closeConnection(connection, mobileClosePeerDropped, "host data closed")
	}
}

func (m *MobileRelay) bridge(connection *mobileConnection, ctx context.Context) {
	connection.mu.Lock()
	phone, host := connection.phone, connection.host
	connection.mu.Unlock()
	if phone == nil || host == nil {
		return
	}
	errors := make(chan error, 2)
	go mobilePipe(ctx, phone, host, errors)
	go mobilePipe(ctx, host, phone, errors)
	select {
	case <-errors:
	case <-ctx.Done():
	}
}

func mobilePipe(ctx context.Context, source, target *mobilePeer, result chan<- error) {
	for {
		messageType, data, err := source.conn.Read(ctx)
		if err != nil {
			result <- err
			return
		}
		if err := target.write(ctx, messageType, data); err != nil {
			result <- err
			return
		}
	}
}

func (m *MobileRelay) closeConnection(connection *mobileConnection, code websocket.StatusCode, reason string) {
	connection.closeOnce.Do(func() {
		m.mu.Lock()
		if m.connections[connection.id] == connection {
			delete(m.connections, connection.id)
		}
		m.mu.Unlock()
		connection.mu.Lock()
		phone, host := connection.phone, connection.host
		connection.mu.Unlock()
		if phone != nil {
			_ = phone.close(code, reason)
		}
		if host != nil {
			_ = host.close(code, reason)
		}
		close(connection.done)
	})
}

func (m *MobileRelay) resolveInviteLocked(hostID, token string, now time.Time) (*mobileInvite, bool) {
	key := sha256.Sum256([]byte(token))
	invite := m.invites[key]
	if invite == nil || invite.hostID != hostID || !now.Before(invite.expiresAt) || invite.attempts >= invite.maxAttempts {
		delete(m.invites, key)
		return nil, false
	}
	invite.attempts++
	return invite, true
}

func (m *MobileRelay) resolveResumeLocked(hostID, token string) (string, string, *mobileCredential, bool) {
	hash := credentialHash(token)
	now := m.now()
	for deviceID, credential := range m.credentials[hostID] {
		if now.Before(credential.ResumeExpiresAt) && subtle.ConstantTimeCompare([]byte(hash), []byte(credential.CurrentHash)) == 1 {
			return deviceID, "current", credential, true
		}
		if !credential.GraceExpiresAt.IsZero() && now.Before(credential.GraceExpiresAt) && subtle.ConstantTimeCompare([]byte(hash), []byte(credential.GraceHash)) == 1 {
			return deviceID, "grace", credential, true
		}
	}
	return "", "", nil, false
}

func (m *MobileRelay) revokeDevice(hostID, deviceID string) {
	var connections []*mobileConnection
	m.mu.Lock()
	delete(m.credentials[hostID], deviceID)
	for key, invite := range m.invites {
		if invite.hostID == hostID && invite.deviceID == deviceID {
			delete(m.invites, key)
		}
	}
	for _, connection := range m.connections {
		if connection.hostID == hostID && connection.deviceID == deviceID {
			connections = append(connections, connection)
		}
	}
	_ = m.saveStateLocked()
	m.mu.Unlock()
	for _, connection := range connections {
		m.closeConnection(connection, mobileCloseBadCredential, "device revoked")
	}
}

func (p *mobilePeer) writeJSON(ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return p.write(ctx, websocket.MessageText, data)
}

func (p *mobilePeer) write(ctx context.Context, messageType websocket.MessageType, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return p.conn.Write(writeCtx, messageType, data)
}

func (p *mobilePeer) close(code websocket.StatusCode, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn.Close(code, reason)
}

func validMobileHostID(value string) bool {
	if len(value) != 16 {
		return false
	}
	for _, char := range value {
		if !((char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func validBase64URL32(value string) bool {
	if len(value) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func mobileHostKeyMatches(hostID, publicKeyB64 string) bool {
	key, err := base64.StdEncoding.DecodeString(publicKeyB64)
	if err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != publicKeyB64 {
		return false
	}
	hash := sha256.Sum256(key)
	return base64.RawURLEncoding.EncodeToString(hash[:])[:16] == hostID
}

func (m *MobileRelay) randomBase64URL(size int) (string, error) {
	value := make([]byte, size)
	if _, err := io.ReadFull(m.random, value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func credentialHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func installKey(hostID, deviceID, requestID string) string {
	return hostID + "\x00" + deviceID + "\x00" + requestID
}

func (m *MobileRelay) loadState() error {
	if m.stateFile == "" {
		return nil
	}
	data, err := os.ReadFile(m.stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read mobile relay state: %w", err)
	}
	var state mobileCredentialState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse mobile relay state: %w", err)
	}
	if state.Hosts != nil {
		m.credentials = state.Hosts
	}
	return nil
}

func (m *MobileRelay) saveStateLocked() error {
	if m.stateFile == "" {
		return nil
	}
	data, err := json.MarshalIndent(mobileCredentialState{Hosts: m.credentials}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.stateFile), 0o700); err != nil {
		return err
	}
	temporary := m.stateFile + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := m.chmodFile(temporary, 0o600); err != nil && !m.allowUnsupportedStateFileMode {
		return err
	}
	return os.Rename(temporary, m.stateFile)
}
