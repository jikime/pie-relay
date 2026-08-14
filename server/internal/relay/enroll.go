package relay

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"time"
)

// defaultHostEnrollTTL is intentionally long (30일): a host token is issued by
// the operator-held enroll secret (not a short-lived browser session), so it is
// meant to survive daemon restarts and day-to-day use without re-enrolling.
// main.go may override it via HOST_ENROLL_TTL.
const defaultHostEnrollTTL = 720 * time.Hour

// hostRoomPrefix / hostRoomLen size the auto-generated room id when the caller
// doesn't name one: "r-" + 8 chars drawn from the same unambiguous alphabet as
// invite codes (randCode).
const (
	hostRoomPrefix = "r-"
	hostRoomLen    = 8
)

// Enroller issues HOST tokens to whoever proves knowledge of the operator's
// enroll secret. This replaces the former external browser-login path:
// the relay operator holds HOST_ENROLL_SECRET and hands it to trusted hosts,
// who exchange it here for a room + host JWT. secret is compared in constant
// time; an empty secret means the operator did not enable enrollment, so the
// endpoint reports 503 rather than minting anything. auth SIGNS the host token
// with the same shared secret used everywhere else in the relay.
type Enroller struct {
	auth                       JWTAuth
	secret                     []byte        // HOST_ENROLL_SECRET; empty = enrollment disabled
	ttl                        time.Duration // host token lifetime
	allowLoopbackWithoutSecret bool          // explicit local-dev opt-in; unsafe behind a proxy
}

// NewEnroller returns an Enroller. secret == "" leaves enrollment disabled
// unless the operator explicitly enables direct loopback enrollment. That
// bypass must never be inferred from RemoteAddr alone: a public request passed
// through a same-machine reverse proxy also appears to originate at 127.0.0.1.
// ttl <= 0 falls back to defaultHostEnrollTTL.
func NewEnroller(auth JWTAuth, secret string, ttl time.Duration, allowLoopbackWithoutSecret bool) *Enroller {
	if ttl <= 0 {
		ttl = defaultHostEnrollTTL
	}
	return &Enroller{
		auth:                       auth,
		secret:                     []byte(secret),
		ttl:                        ttl,
		allowLoopbackWithoutSecret: allowLoopbackWithoutSecret,
	}
}

type enrollRequest struct {
	Secret    string `json:"secret"`
	Room      string `json:"room"` // optional; generated when empty
	Name      string `json:"name"` // optional; defaults to "host"
	DeviceID  string `json:"deviceId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	RelayNode string `json:"relayNode,omitempty"`
}

type enrollResponse struct {
	Token           string `json:"token"`
	Room            string `json:"room"`
	DeviceID        string `json:"deviceId,omitempty"`
	SessionID       string `json:"sessionId,omitempty"`
	ExecutionTarget string `json:"executionTarget,omitempty"`
	RelayNode       string `json:"relayNode,omitempty"`
	ExpiresAt       int64  `json:"expiresAt"` // unix seconds
}

// handleEnroll: POST /host/enroll with {secret, room?, name?}. Mints a host
// token for the (possibly generated) room. Every caller proves the operator
// secret unless the server was explicitly started with the direct-loopback
// bypass and this request is actually loopback. With no configured secret the
// normal path returns 503; a wrong secret returns 401.
func (e *Enroller) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req enrollRequest
	if err := decodeJSONRequest(w, r, &req, 8<<10, false); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if (req.DeviceID == "") != (req.SessionID == "") || req.DeviceID != "" && (!streamIDPattern.MatchString(req.DeviceID) || !streamIDPattern.MatchString(req.SessionID)) {
		http.Error(w, "deviceId and sessionId must be valid and supplied together", http.StatusBadRequest)
		return
	}
	// Loopback skips the secret only under the explicit local-development opt-in.
	if !(e.allowLoopbackWithoutSecret && isLoopback(r.RemoteAddr)) {
		// Enrollment off unless the operator set HOST_ENROLL_SECRET — refuse to
		// mint rather than silently accept any/empty secret.
		if len(e.secret) == 0 {
			http.Error(w, "host enroll disabled", http.StatusServiceUnavailable)
			return
		}
		// Constant-time compare so a wrong secret can't be recovered byte-by-byte
		// via response timing. ConstantTimeCompare already returns 0 on a length
		// mismatch, so no separate length check is needed.
		if subtle.ConstantTimeCompare([]byte(req.Secret), e.secret) != 1 {
			http.Error(w, "invalid secret", http.StatusUnauthorized)
			return
		}
	}

	room := req.Room
	if room == "" {
		code, err := randCode(hostRoomLen)
		if err != nil {
			http.Error(w, "could not generate room", http.StatusInternalServerError)
			return
		}
		room = hostRoomPrefix + strings.ToLower(code)
	}
	name := req.Name
	if name == "" {
		name = "host"
	}
	sub := sanitizeName(name)

	expiresAt := time.Now().Add(e.ttl)
	token, err := e.auth.MintScopedWithTarget(sub, room, req.DeviceID, req.SessionID, "local", RoleHost, "", req.RelayNode, e.ttl)
	if err != nil {
		http.Error(w, "could not mint token", http.StatusInternalServerError)
		return
	}
	writeJSON(w, enrollResponse{Token: token, Room: room, DeviceID: req.DeviceID, SessionID: req.SessionID, ExecutionTarget: "local", RelayNode: req.RelayNode, ExpiresAt: expiresAt.Unix()})
}

// isLoopback reports whether remoteAddr (http.Request.RemoteAddr, "host:port")
// is a loopback address. This is only consulted after the operator explicitly
// opted into the local bypass; RemoteAddr alone is not a trust signal because a
// reverse proxy on the same machine masks public clients as loopback.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr // no port (unusual, but don't drop the check)
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
