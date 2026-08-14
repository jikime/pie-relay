package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/auth"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/capability"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
)

func (a api) handleAdmin(w http.ResponseWriter, r *http.Request) {
	principal, ok := a.principal(w, r)
	if !ok {
		return
	}
	if !principal.CanViewAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/admin/"), "/")
	if a.handleAdminClaudeAuth(w, r, principal, path) {
		return
	}
	if a.handleAdminIntegrations(w, r, principal.CanAdminister(), path) {
		return
	}
	switch {
	case path == "overview" && r.Method == http.MethodGet:
		write(w, a.control.Overview(), http.StatusOK)
	case path == "snapshot" && r.Method == http.MethodGet:
		write(w, a.control.Snapshot(), http.StatusOK)
	case path == "events" && r.Method == http.MethodGet:
		a.controlEvents(w, r)
	case path == "users" && r.Method == http.MethodGet:
		write(w, limitSlice(a.control.Users(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
	case path == "users" && r.Method == http.MethodPost:
		if !requireAdminister(w, principal) {
			return
		}
		var value control.User
		if !decodeControlJSON(w, r, &value) {
			return
		}
		created, err := a.control.PutUser(r.Context(), value, 0, requestMeta(r, principal))
		writeControlResult(w, created, http.StatusCreated, err)
	case strings.HasPrefix(path, "users/"):
		if !requireAdminister(w, principal) {
			return
		}
		id := strings.TrimPrefix(path, "users/")
		var value control.User
		if !decodeControlJSON(w, r, &value) {
			return
		}
		if value.ID == "" {
			value.ID = id
		}
		if r.Method != http.MethodPut || value.ID != id {
			http.Error(w, "invalid user request", http.StatusBadRequest)
			return
		}
		updated, err := a.control.PutUser(r.Context(), value, value.Version, requestMeta(r, principal))
		writeControlResult(w, updated, http.StatusOK, err)
	case path == "nodes" && r.Method == http.MethodGet:
		write(w, limitSlice(a.control.Nodes(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
	case path == "nodes" && r.Method == http.MethodPost:
		if !requireAdminister(w, principal) {
			return
		}
		var value control.Node
		if !decodeControlJSON(w, r, &value) {
			return
		}
		created, err := a.control.PutNode(r.Context(), value, 0, requestMeta(r, principal))
		writeControlResult(w, created, http.StatusCreated, err)
	case strings.HasPrefix(path, "nodes/") && r.Method == http.MethodPut:
		if !requireAdminister(w, principal) {
			return
		}
		id := strings.TrimPrefix(path, "nodes/")
		var value control.Node
		if !decodeControlJSON(w, r, &value) {
			return
		}
		if value.ID == "" {
			value.ID = id
		}
		if value.ID != id {
			http.Error(w, "invalid node request", http.StatusBadRequest)
			return
		}
		updated, err := a.control.PutNode(r.Context(), value, value.Version, requestMeta(r, principal))
		writeControlResult(w, updated, http.StatusOK, err)
	case path == "devices" && r.Method == http.MethodGet:
		write(w, limitSlice(a.control.Devices(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
	case path == "devices" && r.Method == http.MethodPost:
		if !requireOperate(w, principal) {
			return
		}
		var value control.Device
		if !decodeControlJSON(w, r, &value) {
			return
		}
		created, err := a.control.RegisterDevice(r.Context(), value, requestMeta(r, principal))
		writeControlResult(w, created, http.StatusCreated, err)
	case strings.HasPrefix(path, "devices/") && strings.HasSuffix(path, "/heartbeat") && r.Method == http.MethodPost:
		if !requireOperate(w, principal) {
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "devices/"), "/heartbeat")
		var heartbeat control.DeviceHeartbeat
		if !decodeControlJSON(w, r, &heartbeat) {
			return
		}
		updated, err := a.control.HeartbeatDevice(r.Context(), id, "", heartbeat, requestMeta(r, principal))
		writeControlResult(w, updated, http.StatusOK, err)
	case path == "runtimes" && r.Method == http.MethodGet:
		write(w, limitSlice(a.control.Runtimes(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
	case path == "runtimes" && r.Method == http.MethodPost:
		if !requireAdminister(w, principal) {
			return
		}
		var value control.RuntimeInstance
		if !decodeControlJSON(w, r, &value) {
			return
		}
		created, err := a.control.PutRuntime(r.Context(), value, 0, requestMeta(r, principal))
		writeControlResult(w, created, http.StatusCreated, err)
	case strings.HasPrefix(path, "runtimes/") && r.Method == http.MethodPut:
		if !requireAdminister(w, principal) {
			return
		}
		id := strings.TrimPrefix(path, "runtimes/")
		var value control.RuntimeInstance
		if !decodeControlJSON(w, r, &value) {
			return
		}
		if value.ID == "" {
			value.ID = id
		}
		if value.ID != id {
			http.Error(w, "invalid runtime request", http.StatusBadRequest)
			return
		}
		updated, err := a.control.PutRuntime(r.Context(), value, value.Version, requestMeta(r, principal))
		writeControlResult(w, updated, http.StatusOK, err)
	case path == "sessions" && r.Method == http.MethodGet:
		write(w, limitSlice(a.control.Sessions(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
	case path == "sessions" && r.Method == http.MethodPost:
		if !requireOperate(w, principal) {
			return
		}
		var value control.Session
		if !decodeControlJSON(w, r, &value) {
			return
		}
		created, err := a.control.PutSession(r.Context(), value, 0, requestMeta(r, principal))
		writeControlResult(w, created, http.StatusCreated, err)
	case strings.HasPrefix(path, "sessions/") && r.Method == http.MethodPut:
		if !requireOperate(w, principal) {
			return
		}
		id := strings.TrimPrefix(path, "sessions/")
		var value control.Session
		if !decodeControlJSON(w, r, &value) {
			return
		}
		if value.ID == "" {
			value.ID = id
		}
		if value.ID != id {
			http.Error(w, "invalid session request", http.StatusBadRequest)
			return
		}
		updated, err := a.control.PutSession(r.Context(), value, value.Version, requestMeta(r, principal))
		writeControlResult(w, updated, http.StatusOK, err)
	case path == "participants" && r.Method == http.MethodGet:
		write(w, limitSlice(a.control.Participants(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
	case path == "participants" && r.Method == http.MethodPost:
		if !requireOperate(w, principal) {
			return
		}
		var value control.Participant
		if !decodeControlJSON(w, r, &value) {
			return
		}
		updated, err := a.control.PutParticipant(r.Context(), value, requestMeta(r, principal))
		writeControlResult(w, updated, http.StatusCreated, err)
	case strings.HasPrefix(path, "participants/") && r.Method == http.MethodDelete:
		if !requireOperate(w, principal) {
			return
		}
		id := strings.TrimPrefix(path, "participants/")
		if err := a.control.DeleteParticipant(r.Context(), id, requestMeta(r, principal)); err != nil {
			writeControlError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case path == "grants" && r.Method == http.MethodGet:
		write(w, limitSlice(a.control.Grants(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
	case path == "grants" && r.Method == http.MethodPost:
		if !requireAdminister(w, principal) {
			return
		}
		var value control.AccessGrant
		if !decodeControlJSON(w, r, &value) {
			return
		}
		created, err := a.control.PutGrant(r.Context(), value, 0, requestMeta(r, principal))
		writeControlResult(w, created, http.StatusCreated, err)
	case strings.HasPrefix(path, "grants/") && strings.HasSuffix(path, "/revoke") && r.Method == http.MethodPost:
		if !requireAdminister(w, principal) {
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "grants/"), "/revoke")
		value, found := a.control.Grant(id)
		if !found {
			http.NotFound(w, r)
			return
		}
		updated, err := a.control.RevokeGrant(r.Context(), id, value.Version, requestMeta(r, principal))
		writeControlResult(w, updated, http.StatusOK, err)
	case path == "operations" && r.Method == http.MethodGet:
		write(w, limitSlice(a.control.Operations(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
	case path == "operations" && r.Method == http.MethodPost:
		if !requireOperate(w, principal) {
			return
		}
		var value control.Operation
		if !decodeControlJSON(w, r, &value) {
			return
		}
		value.ActorUserID = principal.UserID
		if value.IdempotencyKey == "" {
			value.IdempotencyKey = r.Header.Get("Idempotency-Key")
		}
		meta := requestMeta(r, principal)
		value.ActorUserID = meta.ActorUserID
		created, duplicate, err := a.control.BeginOperation(r.Context(), value, meta)
		status := http.StatusAccepted
		if duplicate {
			status = http.StatusOK
		}
		writeControlResult(w, created, status, err)
	case path == "audit" && r.Method == http.MethodGet:
		audit := a.control.AuditEvents()
		if limit := positiveQueryInt(r, "limit", 200, 1000); len(audit) > limit {
			audit = audit[:limit]
		}
		write(w, audit, http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func limitSlice[T any](values []T, limit int) []T {
	if len(values) > limit {
		return values[:limit]
	}
	return values
}

func (a api) handleControl(w http.ResponseWriter, r *http.Request) {
	principal, ok := a.principal(w, r)
	if !ok {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/control/"), "/")
	if path == "relay/presence" && r.Method == http.MethodPost {
		if !principal.CanReportRelayPresence() {
			http.Error(w, "relay presence role required", http.StatusForbidden)
			return
		}
		var presence control.RelayPresence
		if !decodeControlJSON(w, r, &presence) {
			return
		}
		meta := requestMeta(r, principal)
		meta.Trusted = true
		if err := a.control.ApplyRelayPresence(r.Context(), presence, meta); err != nil {
			writeControlError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if principal.UserID == "" && !principal.Admin {
		http.Error(w, "user identity required", http.StatusForbidden)
		return
	}
	switch {
	case path == "me" && r.Method == http.MethodGet:
		write(w, map[string]any{"userId": principal.UserID, "organizationId": principal.OrganizationID, "roles": principal.Roles, "admin": principal.Admin}, http.StatusOK)
	case path == "devices" && r.Method == http.MethodGet:
		devices, grants := a.control.Devices(), a.control.Grants()
		filtered := make([]control.Device, 0, len(devices))
		for _, device := range devices {
			if principal.IsDevice() && device.ID != principal.DeviceID {
				continue
			}
			if principal.CanOperate() || device.OwnerUserID == principal.UserID || principalHasDeviceGrant(grants, principal.UserID, device.ID, time.Now()) {
				filtered = append(filtered, device)
			}
		}
		write(w, limitSlice(filtered, positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
	case path == "devices/register" && r.Method == http.MethodPost:
		var value control.Device
		if !decodeControlJSON(w, r, &value) {
			return
		}
		if !principal.Admin {
			if principal.IsDevice() && value.ID != principal.DeviceID {
				http.Error(w, "device token does not match requested device", http.StatusForbidden)
				return
			}
			if principal.IsDevice() {
				if current, exists := a.control.Device(value.ID); exists && current.DesiredState == "revoked" {
					http.Error(w, "device has been revoked", http.StatusForbidden)
					return
				}
			}
			value.OwnerUserID = principal.UserID
			if _, exists := a.control.User(principal.UserID); !exists {
				_, err := a.control.PutUser(r.Context(), control.User{ID: principal.UserID, ExternalSubject: principal.UserID, OrganizationID: principal.OrganizationID, Status: "active"}, 0, requestMeta(r, principal))
				if err != nil && !errors.Is(err, control.ErrConflict) {
					writeControlError(w, err)
					return
				}
			}
		}
		created, err := a.control.RegisterDevice(r.Context(), value, requestMeta(r, principal))
		writeControlResult(w, created, http.StatusCreated, err)
	case strings.HasPrefix(path, "devices/") && strings.HasSuffix(path, "/heartbeat") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "devices/"), "/heartbeat")
		if !requirePrincipalDevice(w, principal, id) {
			return
		}
		if principal.IsDevice() {
			if device, exists := a.control.Device(id); exists && device.DesiredState == "revoked" {
				http.Error(w, "device has been revoked", http.StatusForbidden)
				return
			}
		}
		var heartbeat control.DeviceHeartbeat
		if !decodeControlJSON(w, r, &heartbeat) {
			return
		}
		owner := principal.UserID
		if principal.Admin {
			owner = ""
		}
		updated, err := a.control.HeartbeatDevice(r.Context(), id, owner, heartbeat, requestMeta(r, principal))
		writeControlResult(w, updated, http.StatusOK, err)
	case strings.HasPrefix(path, "devices/") && strings.HasSuffix(path, "/sessions") && r.Method == http.MethodGet:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "devices/"), "/sessions")
		if !requirePrincipalDevice(w, principal, id) {
			return
		}
		a.listDeviceSessions(w, r, principal, id)
	case strings.HasPrefix(path, "devices/") && strings.Contains(path, "/sessions/") && strings.HasSuffix(path, "/status") && r.Method == http.MethodPost:
		trimmed := strings.TrimPrefix(path, "devices/")
		parts := strings.Split(trimmed, "/")
		if len(parts) != 4 || parts[1] != "sessions" || parts[3] != "status" {
			http.NotFound(w, r)
			return
		}
		a.reportDeviceSessionStatus(w, r, principal, parts[0], parts[2])
	case path == "sessions" && r.Method == http.MethodGet:
		sessions, participants := a.control.Sessions(), a.control.Participants()
		visible := make([]control.Session, 0)
		for _, session := range sessions {
			if principal.IsDevice() && session.DeviceID != principal.DeviceID {
				continue
			}
			_, granted := a.control.CanAccessSession(principal.UserID, session.ID, "view", time.Now())
			if principal.CanOperate() || granted || principalInSession(participants, session.ID, principal.UserID) {
				visible = append(visible, session)
			}
		}
		write(w, limitSlice(visible, positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
	case path == "grants" && r.Method == http.MethodGet:
		if principal.IsDevice() {
			http.Error(w, "device tokens cannot manage grants", http.StatusForbidden)
			return
		}
		all := a.control.Grants()
		visible := make([]control.AccessGrant, 0)
		for _, grant := range all {
			if principal.Admin || grant.OwnerUserID == principal.UserID || grant.SubjectUserID == principal.UserID {
				visible = append(visible, grant)
			}
		}
		write(w, limitSlice(visible, positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
	case path == "grants" && r.Method == http.MethodPost:
		if principal.IsDevice() {
			http.Error(w, "device tokens cannot manage grants", http.StatusForbidden)
			return
		}
		var value control.AccessGrant
		if !decodeControlJSON(w, r, &value) {
			return
		}
		if !principal.Admin {
			value.OwnerUserID = principal.UserID
		}
		created, err := a.control.PutGrant(r.Context(), value, 0, requestMeta(r, principal))
		writeControlResult(w, created, http.StatusCreated, err)
	case strings.HasPrefix(path, "grants/") && strings.HasSuffix(path, "/revoke") && r.Method == http.MethodPost:
		if principal.IsDevice() {
			http.Error(w, "device tokens cannot manage grants", http.StatusForbidden)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "grants/"), "/revoke")
		value, found := a.control.Grant(id)
		if !found {
			http.NotFound(w, r)
			return
		}
		if !principal.Admin && value.OwnerUserID != principal.UserID {
			http.Error(w, "grant ownership required", http.StatusForbidden)
			return
		}
		updated, err := a.control.RevokeGrant(r.Context(), id, value.Version, requestMeta(r, principal))
		writeControlResult(w, updated, http.StatusOK, err)
	case path == "sessions" && r.Method == http.MethodPost:
		if principal.IsDevice() {
			http.Error(w, "device tokens cannot create sessions", http.StatusForbidden)
			return
		}
		var value control.Session
		if !decodeControlJSON(w, r, &value) {
			return
		}
		if !principal.CanOperate() {
			value.OwnerUserID = principal.UserID
		} else if value.OwnerUserID == "" {
			if device, exists := a.control.Device(value.DeviceID); exists {
				value.OwnerUserID = device.OwnerUserID
			}
		}
		created, err := a.control.PutSession(r.Context(), value, 0, requestMeta(r, principal))
		writeControlResult(w, created, http.StatusCreated, err)
	case strings.HasPrefix(path, "sessions/") && strings.HasSuffix(path, "/credential") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "sessions/"), "/credential")
		a.issueSessionCredential(w, r, principal, id)
	default:
		http.NotFound(w, r)
	}
}

// listDeviceSessions is the desired-state feed consumed by a Host OS clientd.
// The daemon polls outbound over HTTPS, so its loopback Session Manager never
// needs an inbound firewall/NAT exception. Only the owning device principal (or
// an operator) may see the feed; ordinary share grants permit terminal access,
// not remote process creation on somebody else's machine.
func (a api) listDeviceSessions(w http.ResponseWriter, r *http.Request, principal auth.Principal, deviceID string) {
	if !requirePrincipalDevice(w, principal, deviceID) {
		return
	}
	device, ok := a.control.Device(deviceID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if principal.IsDevice() && device.DesiredState == "revoked" {
		http.Error(w, "device has been revoked", http.StatusForbidden)
		return
	}
	if device.Kind != "local" {
		http.Error(w, "device session feed is only available to Host OS agents", http.StatusBadRequest)
		return
	}
	if !principal.CanOperate() && device.OwnerUserID != principal.UserID {
		http.Error(w, "device ownership required", http.StatusForbidden)
		return
	}
	values := make([]control.Session, 0)
	for _, session := range a.control.Sessions() {
		if session.DeviceID == device.ID && session.OwnerUserID == device.OwnerUserID && session.ExecutionTarget == "local" {
			values = append(values, session)
		}
	}
	write(w, limitSlice(values, positiveQueryInt(r, "limit", 256, 1024)), http.StatusOK)
}

// reportDeviceSessionStatus projects the local Session Manager result back into
// the Control Plane. Relay presence remains authoritative for "active"; the
// device reports only launch readiness, terminal shutdown, or launch failure.
func (a api) reportDeviceSessionStatus(w http.ResponseWriter, r *http.Request, principal auth.Principal, deviceID, sessionID string) {
	if !requirePrincipalDevice(w, principal, deviceID) {
		return
	}
	device, ok := a.control.Device(deviceID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if principal.IsDevice() && device.DesiredState == "revoked" {
		http.Error(w, "device has been revoked", http.StatusForbidden)
		return
	}
	if device.Kind != "local" {
		http.Error(w, "session status is only accepted from Host OS agents", http.StatusBadRequest)
		return
	}
	if !principal.CanOperate() && device.OwnerUserID != principal.UserID {
		http.Error(w, "device ownership required", http.StatusForbidden)
		return
	}
	var request struct {
		Status            string `json:"status"`
		SelectedTransport string `json:"selectedTransport,omitempty"`
		LastError         string `json:"lastError,omitempty"`
	}
	if !decodeControlJSON(w, r, &request) {
		return
	}
	if request.Status != "ready" && request.Status != "closed" && request.Status != "error" {
		http.Error(w, "invalid device session status", http.StatusBadRequest)
		return
	}
	if len(request.LastError) > 2048 {
		http.Error(w, "session error is too long", http.StatusBadRequest)
		return
	}
	if request.SelectedTransport != "" && request.SelectedTransport != "relay" {
		http.Error(w, "Host OS managed sessions currently require relay transport", http.StatusBadRequest)
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		session, found := a.control.Session(sessionID)
		if !found {
			http.NotFound(w, r)
			return
		}
		if session.DeviceID != device.ID || session.OwnerUserID != device.OwnerUserID || session.ExecutionTarget != "local" {
			http.Error(w, "session does not belong to device", http.StatusForbidden)
			return
		}
		// A delayed ready/error response must never resurrect a session the owner
		// already closed while the device was starting it.
		if (session.Status == "closing" || session.Status == "closed") && request.Status != "closed" {
			http.Error(w, "session is closing", http.StatusConflict)
			return
		}
		if request.Status == "ready" && (session.Status == "active" || session.Status == "idle") {
			write(w, session, http.StatusOK)
			return
		}
		session.Status = request.Status
		session.LastError = request.LastError
		if request.Status == "ready" {
			session.SelectedTransport = "relay"
			session.LastActivityAt = time.Now().UTC()
			session.LastError = ""
		}
		updated, err := a.control.PutSession(r.Context(), session, session.Version, requestMeta(r, principal))
		if errors.Is(err, control.ErrConflict) {
			continue
		}
		writeControlResult(w, updated, http.StatusOK, err)
		return
	}
	http.Error(w, "session status changed concurrently", http.StatusConflict)
}

func (a api) issueSessionCredential(w http.ResponseWriter, r *http.Request, principal auth.Principal, sessionID string) {
	if !a.issuer.Enabled() {
		http.Error(w, "relay capability issuer is not configured", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		SubjectUserID string `json:"subjectUserId"`
		Role          string `json:"role"`
		Access        string `json:"access"`
		TTLSeconds    int64  `json:"ttlSeconds,omitempty"`
	}
	if !decodeControlJSON(w, r, &request) {
		return
	}
	if principal.IsDevice() && request.SubjectUserID != "" && request.SubjectUserID != principal.UserID {
		http.Error(w, "device token cannot change credential subject", http.StatusForbidden)
		return
	}
	subject := principal.UserID
	if principal.Admin || principal.CanOperate() {
		if request.SubjectUserID != "" {
			subject = request.SubjectUserID
		}
	}
	if subject == "" {
		http.Error(w, "subject user is required", http.StatusBadRequest)
		return
	}
	role := request.Role
	if role == "" {
		role = "participant"
	}
	access := request.Access
	if access == "" {
		access = "view"
	}
	session, allowed := a.control.CanAccessSession(subject, sessionID, access, time.Now())
	if !allowed && !principal.CanOperate() {
		http.Error(w, "session access forbidden", http.StatusForbidden)
		return
	}
	if !allowed && principal.CanOperate() {
		var ok bool
		session, ok = a.control.Session(sessionID)
		if !ok {
			http.NotFound(w, r)
			return
		}
	}
	if principal.IsDevice() {
		if session.DeviceID != principal.DeviceID || session.OwnerUserID != principal.UserID || role != "host" {
			http.Error(w, "device credential scope mismatch", http.StatusForbidden)
			return
		}
		if device, exists := a.control.Device(principal.DeviceID); !exists || device.DesiredState == "revoked" {
			http.Error(w, "device has been revoked", http.StatusForbidden)
			return
		}
	}
	if session.ExecutionTarget == "docker" && subject != session.OwnerUserID {
		http.Error(w, "managed Docker sessions are owner-only", http.StatusForbidden)
		return
	}
	if role == "host" && subject != session.OwnerUserID && !principal.CanOperate() {
		http.Error(w, "host credential requires session ownership", http.StatusForbidden)
		return
	}
	if role == "host" {
		access = "control"
	}
	ttl := time.Duration(request.TTLSeconds) * time.Second
	maxTTL := 12 * time.Hour
	if role == "host" {
		maxTTL = 24 * time.Hour
	}
	if ttl <= 0 {
		if role == "host" {
			ttl = 24 * time.Hour
		} else {
			ttl = time.Hour
		}
	}
	if ttl > maxTTL {
		ttl = maxTTL
	}
	relayURL := a.relayPublicURL
	if session.ApplicationID != "" || session.PoolID != "" {
		assigned, node, err := a.control.EnsureSessionRelayNode(r.Context(), session.ID, requestMeta(r, principal))
		if err != nil {
			if errors.Is(err, control.ErrNoRelayCapacity) {
				http.Error(w, "no healthy relay node is available for this pool", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "could not assign relay node", http.StatusBadRequest)
			return
		}
		session = assigned
		relayURL = node.Address
	}
	minted, err := a.issuer.MintSession(capability.SessionCredential{
		Subject: subject, Room: session.OwnerUserID, DeviceID: session.DeviceID, SessionID: session.ID,
		ApplicationID: session.ApplicationID, PoolID: session.PoolID, TenantID: session.TenantID,
		ResourceType: session.ResourceType, ResourceID: session.ResourceID, AgentID: session.AgentID,
		Protocol: session.Protocol, ExecutionTarget: session.ExecutionTarget, Role: role, Access: access,
		RelayNode: session.RelayNodeID, RelayGeneration: session.RelayGeneration,
		TTL: ttl, AllowInvite: role == "host" && session.AccessMode == "shared",
	})
	if err != nil {
		http.Error(w, "could not issue relay capability", http.StatusBadRequest)
		return
	}
	write(w, struct {
		capability.Minted
		RelayURL string `json:"relayUrl,omitempty"`
	}{Minted: minted, RelayURL: relayURL}, http.StatusCreated)
}

func (a api) controlEvents(w http.ResponseWriter, r *http.Request) {
	if !acquire(w, a.eventSlots) {
		return
	}
	defer func() { <-a.eventSlots }()
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("x-accel-buffering", "no")
	w.Header().Set("connection", "keep-alive")
	events, unsubscribe := a.control.Subscribe(64)
	defer unsubscribe()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	deadline := time.NewTimer(a.eventLifetime)
	defer deadline.Stop()
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-heartbeat.C:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, open := <-events:
			if !open {
				return
			}
			data, _ := json.Marshal(event)
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
			if _, err := fmt.Fprintf(w, "id: %d\nevent: change\ndata: %s\n\n", event.Sequence, data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func decodeControlJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	if decoder.Decode(&struct{}{}) == nil {
		http.Error(w, "multiple JSON values are not allowed", http.StatusBadRequest)
		return false
	}
	return true
}

func writeControlResult(w http.ResponseWriter, value any, status int, err error) {
	if err != nil {
		writeControlError(w, err)
		return
	}
	write(w, value, status)
}

func writeControlError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, control.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, control.ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, control.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, control.ErrQuota):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, control.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, "control plane unavailable", http.StatusServiceUnavailable)
	}
}

func requestMeta(r *http.Request, principal auth.Principal) control.MutationMeta {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	actor := principal.UserID
	if actor == "" && principal.Admin {
		actor = "static-admin"
	}
	return control.MutationMeta{ActorUserID: actor, RequestID: requestID, SourceIP: host}
}

func requireOperate(w http.ResponseWriter, principal auth.Principal) bool {
	if principal.CanOperate() {
		return true
	}
	http.Error(w, "operator role required", http.StatusForbidden)
	return false
}

func requireAdminister(w http.ResponseWriter, principal auth.Principal) bool {
	if principal.CanAdminister() {
		return true
	}
	http.Error(w, "administrator role required", http.StatusForbidden)
	return false
}

func positiveQueryInt(r *http.Request, key string, fallback, maximum int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || value < 1 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

func principalInSession(participants []control.Participant, sessionID, userID string) bool {
	for _, participant := range participants {
		if participant.SessionID == sessionID && participant.UserID == userID {
			return true
		}
	}
	return false
}

func principalHasDeviceGrant(grants []control.AccessGrant, userID, deviceID string, now time.Time) bool {
	for _, grant := range grants {
		if grant.SubjectUserID == userID && grant.TargetDeviceID == deviceID && grant.RevokedAt == nil && grant.ExpiresAt.After(now) {
			return true
		}
	}
	return false
}

func requirePrincipalDevice(w http.ResponseWriter, principal auth.Principal, deviceID string) bool {
	if !principal.IsDevice() || principal.DeviceID == deviceID {
		return true
	}
	http.Error(w, "device token scope mismatch", http.StatusForbidden)
	return false
}
