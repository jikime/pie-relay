package relay

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// peekMessageType extracts the "type" field from a JSON message for routing
// and logging — the ONLY top-level keys the relay interprets are type, from,
// and to (see injectFrom / peerChatMsg); nested payload bodies stay opaque.
// Returns "" if the message isn't parseable JSON, isn't a JSON object, or has
// no string "type" field.
//
// This uses a streaming token decoder rather than json.Unmarshal so it can
// stop as soon as it finds "type" instead of parsing the entire body. Every
// message shape in this protocol puts "type" as the first key, so the common
// case returns after reading just a few tokens — this matters because the
// relay is designed to carry payloads up to maxMessageBytes (16 MiB, see
// below), and a full unmarshal of a multi-MB non-matching message (e.g. a
// large tool_result dump) would otherwise add tens of milliseconds of CPU to
// that connection's hot path on every single message. If "type" were ever
// NOT the first key, this still finds it correctly — it just pays the same
// cost a full unmarshal would have, never returning a wrong answer.
func peekMessageType(data []byte) string {
	return peekStringField(data, "type")
}

// peekStringField extracts a single top-level string field from a JSON object
// message without unmarshalling the whole body (same streaming-decoder rationale
// as the "type" fast path — payloads run up to maxMessageBytes). Returns "" if
// the message isn't a JSON object, the field is absent, or it isn't a string.
func peekStringField(data []byte, field string) string {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return ""
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, ok := keyTok.(string)
		if !ok {
			return ""
		}
		if key == field {
			var val string
			if err := dec.Decode(&val); err != nil {
				return ""
			}
			return val
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return ""
		}
	}
	return ""
}

// injectFrom overwrites the top-level "from" of a participant→host message
// with the VERIFIED sub — a client-supplied from is ignored so a guest cannot
// impersonate another participant. Only the top-level object is parsed; nested
// values ride along as opaque json.RawMessage. Returns ok=false when the body
// isn't a JSON object (policy: unparseable participant messages are dropped).
func injectFrom(data []byte, from string) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return nil, false
	}
	fromJSON, err := json.Marshal(from)
	if err != nil {
		return nil, false
	}
	obj["from"] = fromJSON
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// guestUnsafeFields are the top-level chat fields a role=participant guest must
// not be able to set: they steer the executor's Claude invocation itself —
// bypassing permission prompts (permissionMode), pointing at an arbitrary
// binary (claudePath), overriding the system prompt / disallowed tools, or
// running against an arbitrary path (cwd, projectPath). The relay strips them
// so a guest chat can only ever be a prompt; host senders keep them.
var guestUnsafeFields = []string{
	"permissionMode",
	"claudePath",
	"systemPrompt",
	"disallowedTools",
	"cwd",
	"projectPath",
}

// participantInputTypes are the participant→host message types that constitute
// INPUT — chat prompts, terminal keystrokes/resize, and the host-only control
// verbs. An access=="view" (spectate) participant has ALL of these dropped by
// the relay, so a view guest can never steer the session; non-input messages
// (e.g. request_screen, a scrollback replay ask) still pass so it can watch.
var participantInputTypes = map[string]bool{
	"chat":                true,
	"pty_input":           true,
	"pty_resize":          true,
	"set_driver":          true,
	"driver_request":      true,
	"driver_heartbeat":    true,
	"permission_response": true,
	"abort":               true,
}

var sgrScrollSequence = regexp.MustCompile(`^\x1b\[<([0-9]+);[0-9]+;[0-9]+[Mm]$`)

// validPtyScrollMessage is the relay-side security boundary for the one input
// frame intentionally allowed to access=view spectators. Client-side checks
// are not trusted: a malicious participant can construct JSON directly. Only
// xterm wheel reports and alternate-scroll up/down sequences are accepted;
// arbitrary bytes, clicks, keys, and valid-prefix-plus-suffix payloads drop.
func validPtyScrollMessage(data []byte) bool {
	sequence := peekStringField(data, "data")
	if match := sgrScrollSequence.FindStringSubmatch(sequence); match != nil {
		button, err := strconv.Atoi(match[1])
		return err == nil && button&0x40 != 0
	}
	runes := []rune(sequence)
	if len(runes) == 6 && runes[0] == 0x1b && runes[1] == '[' && runes[2] == 'M' {
		return (runes[3]-32)&0x40 != 0
	}
	return sequence == "\x1bOA" || sequence == "\x1bOB" ||
		sequence == "\x1b[A" || sequence == "\x1b[B"
}

// sanitizeGuest removes the guestUnsafeFields from a participant→host message.
// Like injectFrom it touches only the top-level object; nested payload bodies
// ride along as opaque json.RawMessage and are never inspected. data is
// expected to be an already-parseable JSON object (the caller injected from
// first); if it somehow won't parse the input is returned unchanged. When no
// unsafe field is present the original bytes are returned as-is.
func sanitizeGuest(data []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return data
	}
	stripped := false
	for _, f := range guestUnsafeFields {
		if _, ok := obj[f]; ok {
			delete(obj, f)
			stripped = true
		}
	}
	if !stripped {
		return data
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return data
	}
	return out
}

// peerChatMsg builds the {type:"peer_chat", from, text} echo that lets every
// participant see each other's questions. text is copied verbatim from the
// chat message's top-level "prompt", falling back to top-level "text" when
// "prompt" is absent — the executor's handleChat accepts either (req.prompt ||
// req.text), so a participant may send its message under either field. When
// both are absent the text is left empty. Images are intentionally dropped —
// the echo is a lightweight transcript, not a re-render. data is expected to
// be an already-parseable JSON object (the caller injected from first).
func peerChatMsg(from string, data []byte) []byte {
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(data, &obj) // data already validated by injectFrom
	text := obj["prompt"]
	if len(text) == 0 {
		text = obj["text"]
	}
	echo := struct {
		Type string          `json:"type"`
		From string          `json:"from"`
		Text json.RawMessage `json:"text,omitempty"`
	}{Type: "peer_chat", From: from, Text: text}
	out, _ := json.Marshal(echo)
	return out
}

// AgentAuthenticator validates a host daemon's bearer token → Identity.
type AgentAuthenticator interface {
	AgentUser(ctx context.Context, token string) (Identity, error)
}

// ParticipantAuthenticator validates a participant's token → Identity.
type ParticipantAuthenticator interface {
	ParticipantUser(ctx context.Context, token string) (Identity, error)
}

// ServerOptions configures the host/participant authenticators. BOTH are
// required — there is intentionally no unauthenticated (?user=) mode: the
// relay must never let a caller pick an arbitrary identity. Inviter is
// optional; when set it enables the /rooms/invites and /rooms/join HTTP flow.
// Enroller is optional; when set it enables the /host/enroll HTTP flow (the
// operator-secret path that mints host tokens).
type ServerOptions struct {
	AgentAuth       AgentAuthenticator
	ParticipantAuth ParticipantAuthenticator
	Inviter         *Inviter
	Enroller        *Enroller
	MobileRelay     *MobileRelay
	Limits          Limits
	NodeID          string
	PublicURL       string
	ControlURL      string
	Presence        PresenceSink
	ControlToken    string
	MetricsToken    string
	// AllowLegacyQueryTicket temporarily accepts participant JWTs in
	// ?ticket=. Keep false in production because URLs are commonly logged.
	AllowLegacyQueryTicket bool
	// OriginPatterns contains the exact/patterned browser origins allowed to
	// open cross-origin host and participant WebSockets. Native clients, which
	// do not send Origin, and same-origin browsers remain accepted.
	OriginPatterns []string
}

// withCORS wraps a /rooms/* handler with permissive CORS. Browser-engine
// clients (the Tauri desktop webview, a future web participant) enforce CORS
// on cross-origin fetch — JSON POSTs and authenticated assignment GETs trigger
// an OPTIONS preflight, and
// without these headers the request dies as a network error before ever
// reaching the handler (curl/Go/Node callers never noticed). "*" is
// deliberate: authorization is carried entirely by the Bearer token / invite
// code in the request itself, and native callers bypass CORS anyway, so an
// origin allowlist would add friction without adding security. WebSocket does
// not use CORS, but its Origin header is verified separately by
// websocketAcceptOptions to prevent cross-site WebSocket hijacking.
func withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

// NewServerOpts returns a relay Server with the given auth options.
func NewServerOpts(o ServerOptions) *Server {
	s := &Server{
		reg: NewRegistry(), mux: http.NewServeMux(), auth: o.AgentAuth,
		participantAuth: o.ParticipantAuth, inviter: o.Inviter, enroller: o.Enroller,
		limits: normalizeLimits(o.Limits), metrics: &relayMetrics{},
		nodeID: o.NodeID, publicURL: strings.TrimRight(o.PublicURL, "/"), controlURL: strings.TrimRight(o.ControlURL, "/"), active: map[*wsSender]struct{}{}, connections: map[string]*wsSender{},
		presence:         o.Presence,
		controlToken:     strings.TrimSpace(o.ControlToken),
		metricsToken:     strings.TrimSpace(o.MetricsToken),
		allowQueryTicket: o.AllowLegacyQueryTicket,
		originPatterns:   append([]string(nil), o.OriginPatterns...),
	}
	s.mux.HandleFunc("/ws/agent", s.handleAgent)
	s.mux.HandleFunc("/ws/participant", s.handleParticipant)
	// /ws/browser: deprecated alias of /ws/participant, kept so existing
	// Pre-participant clients keep connecting during the endpoint rename.
	s.mux.HandleFunc("/ws/browser", s.handleParticipant)
	if o.Inviter != nil {
		s.mux.HandleFunc("/rooms/invites", withCORS(o.Inviter.handleCreateInvite))
		s.mux.HandleFunc("/rooms/join", withCORS(o.Inviter.handleJoin))
	}
	if o.Enroller != nil {
		s.mux.HandleFunc("/host/enroll", withCORS(o.Enroller.handleEnroll))
	}
	if o.MobileRelay != nil {
		o.MobileRelay.register(s.mux)
	}
	s.mux.HandleFunc("/healthz", handleHealthz)
	s.mux.HandleFunc("/metrics", s.handleMetrics)
	// /readyz alias: kroot 배포 계약(verify-prod-deploy-contract)이 public 서비스의
	// Docker healthcheck 경로를 /readyz 로 표준화한다. 무상태 릴레이는 live == ready.
	s.mux.HandleFunc("/readyz", s.handleReadyz)
	s.mux.HandleFunc("/v1/relay/assignment", withCORS(s.handleAssignment))
	s.mux.HandleFunc("/v1/control/connections/", s.handleControlConnection)
	s.mux.HandleFunc("/v1/control/driver", s.handleControlDriver)
	return s
}

// handleHealthz is the liveness probe for Traefik/Docker healthchecks. The
// relay is stateless (no DB, no stored tokens beyond in-memory invite codes),
// so "the process answers HTTP" is the entire check — auth backends are probed
// lazily per connect.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if s.draining.Load() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	handleHealthz(w, nil)
}

func (s *Server) registerActive(sender *wsSender) bool {
	if s.draining.Load() {
		return false
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.draining.Load() {
		return false
	}
	s.active[sender] = struct{}{}
	return true
}

func (s *Server) unregisterActive(sender *wsSender) {
	s.activeMu.Lock()
	delete(s.active, sender)
	s.activeMu.Unlock()
}

func (s *Server) trackConnection(connectionID string, sender *wsSender) func() {
	s.activeMu.Lock()
	s.connections[connectionID] = sender
	s.activeMu.Unlock()
	return func() {
		s.activeMu.Lock()
		if s.connections[connectionID] == sender {
			delete(s.connections, connectionID)
		}
		s.activeMu.Unlock()
	}
}

func (s *Server) authorizeControl(r *http.Request) bool {
	if s.controlToken == "" {
		return false
	}
	token := bearerToken(r)
	return len(token) == len(s.controlToken) && subtle.ConstantTimeCompare([]byte(token), []byte(s.controlToken)) == 1
}

func (s *Server) handleControlConnection(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeControl(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	connectionID := strings.TrimPrefix(r.URL.Path, "/v1/control/connections/")
	if connectionID == "" || strings.Contains(connectionID, "/") {
		http.Error(w, "invalid connection id", http.StatusBadRequest)
		return
	}
	s.activeMu.Lock()
	sender, ok := s.connections[connectionID]
	s.activeMu.Unlock()
	if !ok {
		// DELETE is idempotent. Presence delivery is asynchronous, so a
		// connection can disappear between Manager's snapshot and this call.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sender.shutdown(websocket.StatusPolicyViolation, "disconnected by operator")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleControlDriver(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeControl(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	defer r.Body.Close()
	var request struct {
		Room            string `json:"room"`
		DeviceID        string `json:"deviceId"`
		SessionID       string `json:"sessionId"`
		RelayGeneration int64  `json:"relayGeneration,omitempty"`
		UserID          string `json:"userId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Room == "" || request.DeviceID == "" || request.SessionID == "" || request.RelayGeneration < 0 {
		http.Error(w, "invalid driver request", http.StatusBadRequest)
		return
	}
	routingKey := (Identity{Room: request.Room, DeviceID: request.DeviceID, SessionID: request.SessionID, RelayGeneration: request.RelayGeneration}).RoutingKey()
	lease, ok := s.reg.SetDriver(routingKey, request.UserID, time.Now(), driverLeaseTTL)
	if !ok {
		// Keep this diagnostic free of credentials and participant identities.
		// It is still enough to distinguish a stale Control Plane projection
		// from a routing-scope mismatch during operations troubleshooting.
		log.Printf("relay: driver handoff rejected room=%s device=%s session=%s relay_generation=%d connected_participants=%d", request.Room, request.DeviceID, request.SessionID, request.RelayGeneration, len(s.reg.ParticipantSnapshots(routingKey)))
		http.Error(w, "driver target is not connected with control access", http.StatusConflict)
		return
	}
	if host, connected := s.reg.HostFor(routingKey); connected {
		_ = host.Send(setDriverMsg(lease.UserID))
	}
	s.notifyDriverState(routingKey, lease)
	writeJSON(w, struct {
		UserID     string    `json:"userId,omitempty"`
		Generation uint64    `json:"generation"`
		ExpiresAt  time.Time `json:"expiresAt,omitempty"`
	}{lease.UserID, lease.Generation, lease.ExpiresAt})
}

func (s *Server) ActiveConnections() int {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	return len(s.active)
}

// BeginDrain makes readiness/assignment/new websocket handshakes fail while
// existing rooms continue until the process grace period ends.
func (s *Server) BeginDrain() { s.draining.Store(true) }

func (s *Server) CloseActiveConnections() {
	s.activeMu.Lock()
	active := make([]*wsSender, 0, len(s.active))
	for sender := range s.active {
		active = append(active, sender)
	}
	s.activeMu.Unlock()
	for _, sender := range active {
		sender.shutdown(websocket.StatusGoingAway, "relay node draining")
	}
}

func (s *Server) handleAssignment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.draining.Load() {
		http.Error(w, "relay node draining", http.StatusServiceUnavailable)
		return
	}
	token := bearerToken(r)
	if token == "" {
		token = r.URL.Query().Get("ticket")
	}
	err := errInvalidToken
	var id Identity
	if token != "" && s.participantAuth != nil {
		id, err = s.participantAuth.ParticipantUser(r.Context(), token)
	}
	if token != "" && err != nil && s.auth != nil {
		id, err = s.auth.AgentUser(r.Context(), token)
	}
	if token == "" || err != nil || id.Room == "" {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	if requested := r.URL.Query().Get("room"); requested != "" && requested != id.Room {
		http.Error(w, "room does not match token", http.StatusForbidden)
		return
	}
	origin := s.publicURL
	if origin == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		origin = scheme + "://" + r.Host
	}
	wsOrigin := origin
	if strings.HasPrefix(wsOrigin, "https://") {
		wsOrigin = "wss://" + strings.TrimPrefix(wsOrigin, "https://")
	} else if strings.HasPrefix(wsOrigin, "http://") {
		wsOrigin = "ws://" + strings.TrimPrefix(wsOrigin, "http://")
	}
	writeJSON(w, struct {
		NodeID           string `json:"nodeId"`
		Room             string `json:"room"`
		DeviceID         string `json:"deviceId,omitempty"`
		SessionID        string `json:"sessionId,omitempty"`
		ExecutionTarget  string `json:"executionTarget,omitempty"`
		PublicURL        string `json:"publicUrl"`
		AgentWSURL       string `json:"agentWsUrl"`
		ParticipantWSURL string `json:"participantWsUrl"`
	}{s.nodeID, id.Room, id.DeviceID, id.SessionID, id.ExecutionTarget, origin, wsOrigin + "/ws/agent", wsOrigin + "/ws/participant"})
}

// Server is the ws relay HTTP handler.
type Server struct {
	reg              *Registry
	mux              *http.ServeMux
	auth             AgentAuthenticator
	participantAuth  ParticipantAuthenticator
	inviter          *Inviter
	enroller         *Enroller
	limits           Limits
	metrics          *relayMetrics
	nodeID           string
	publicURL        string
	controlURL       string
	presence         PresenceSink
	controlToken     string
	metricsToken     string
	allowQueryTicket bool
	originPatterns   []string
	draining         atomic.Bool
	activeMu         sync.Mutex
	active           map[*wsSender]struct{}
	connections      map[string]*wsSender
}

func (s *Server) websocketAcceptOptions(subprotocol string) *websocket.AcceptOptions {
	options := &websocket.AcceptOptions{OriginPatterns: s.originPatterns}
	if subprotocol != "" {
		options.Subprotocols = []string{subprotocol}
	}
	return options
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metricsToken == "" {
		http.NotFound(w, r)
		return
	}
	token := bearerToken(r)
	if len(token) != len(s.metricsToken) || subtle.ConstantTimeCompare([]byte(token), []byte(s.metricsToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.metrics.handle(s.reg)(w, r)
}

func (s *Server) reportPresence(identity Identity, connectionID, kind string, connected, hostOnline bool) {
	s.emitPresence(identity, connectionID, kind, connected, hostOnline, false)
}

func (s *Server) emitPresence(identity Identity, connectionID, kind string, connected, hostOnline, heartbeat bool) {
	if s.presence == nil {
		return
	}
	event := PresenceEvent{EventID: newConnectionID(), NodeID: s.nodeID, ApplicationID: identity.ApplicationID, PoolID: identity.PoolID, PublicURL: s.publicURL, ControlURL: s.controlURL, Room: identity.Room, DeviceID: identity.DeviceID, SessionID: identity.SessionID, RelayGeneration: identity.RelayGeneration, UserID: identity.UserID, Role: identity.Role, Access: identity.Access, ConnectionID: connectionID, Kind: kind, Connected: connected, HostOnline: hostOnline, Heartbeat: heartbeat, At: time.Now().UTC()}
	if kind == "host" {
		event.Role = RoleHost
		event.Access = AccessControl
	}
	if !s.presence.Report(event) {
		s.metrics.presenceDropped.Add(1)
	}
}

func (s *Server) presenceHeartbeat(ctx context.Context, identity Identity, connectionID, kind string, interval time.Duration, state func() (bool, bool)) {
	if s.presence == nil || identity.DeviceID == "" || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			report, hostOnline := state()
			if report {
				s.emitPresence(identity, connectionID, kind, true, hostOnline, true)
			}
		}
	}
}

// maxMessageBytes lifts coder/websocket's 32KiB default read limit. Chat
// payloads routinely exceed it (skills_list catalogs, large tool_result
// dumps) and hitting the limit KILLS the connection — the daemon then
// reconnects and the next request kills it again (1s connect/disconnect
// storm, UI flapping green/red).
const maxMessageBytes = 16 << 20 // 16 MiB

const driverLeaseTTL = 20 * time.Second

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// writeTimeout bounds a SINGLE websocket frame write. Without it, s.c.Write
// inherits the connection-lifetime ctx and can block for as long as the OS
// TCP stack takes to notice a stuck peer (potentially minutes) — see
// sendQueueCap below for why that must never happen on the caller's goroutine.
const writeTimeout = 10 * time.Second

// Per-connection queue bounds. The previous count-only channel allowed a slow
// peer to retain 256 * 16MiB frames (roughly 4GiB). Both lanes are now bounded
// by message count AND bytes. Control traffic is drained before bulk output so
// presence, driver handoff, errors, and keepalive state do not sit behind a
// terminal scrollback burst.
const (
	sendQueueCap          = 256 // per lane; retained for compatibility/tests
	controlQueueByteLimit = 16 << 20
	outputQueueByteLimit  = 16 << 20
)

type trafficClass uint8

const (
	trafficControl trafficClass = iota
	trafficOutput
)

type queuedMessage struct {
	payload []byte
	class   trafficClass
}

// classifyTraffic keeps only high-volume, replayable application data in the
// output lane. Unknown types stay control by default (fail safe for future
// coordination frames); the byte cap still prevents unbounded retention.
func classifyTraffic(msg []byte) trafficClass {
	switch peekMessageType(msg) {
	case "pty_output", "pty_snapshot", "pty_size", "pty_exit", "text", "thinking", "tool_result", "skills_list", "history", "done", "error", "aborted":
		return trafficOutput
	default:
		return trafficControl
	}
}

// errSenderClosed is returned by wsSender.Send once the connection is
// shutting down (peer evicted for being too slow, a write failed, or the
// connection's read loop ended). Callers already treat Send errors as
// fire-and-forget (routeFromHost/routeFromParticipant ignore the error), so
// this exists mainly for tests and clarity.
var errSenderClosed = errors.New("wsSender: connection closed")

// wsConn is the slice of *websocket.Conn that wsSender needs. It exists so
// the async send-queue logic in wsSender can be unit-tested against a fake
// (e.g. one whose Write blocks forever to simulate a stuck peer) without
// opening a real socket — *websocket.Conn satisfies this interface as-is, so
// production code paths are unaffected.
type wsConn interface {
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Close(code websocket.StatusCode, reason string) error
}

// wsSender adapts a websocket.Conn to Sender via an async, bounded send
// queue. This is the fix for the N:N fanout stall: routeFromHost/
// routeFromParticipant call Send() from the HOST's single read-loop
// goroutine while holding no registry lock, once per participant in the
// room. The old implementation wrote to the socket synchronously with the
// connection-lifetime ctx (no per-write deadline) — one slow or dead
// participant's TCP buffer filling up would block that Write for minutes,
// which in turn blocked delivery to every OTHER participant in the room AND
// blocked the host's read loop (so the whole room's live traffic stalled).
//
// Now Send NEVER blocks the caller: it either enqueues onto sendCh (fast) or,
// if the queue is full (a genuinely slow/stuck consumer), evicts the peer by
// closing its connection. A single dedicated writer goroutine per connection
// drains sendCh and performs the actual (bounded-deadline) socket write, so
// per-connection message ORDER is preserved for healthy peers — the same
// external behavior as the old synchronous Send.
type wsSender struct {
	ctx  context.Context // connection lifetime ctx (unchanged: used for reads too)
	c    wsConn
	done chan struct{} // closed exactly once to signal shutdown
	wake chan struct{} // coalesced notification that either lane has data
	once sync.Once     // guards done/close so Send and the writer can't double-close

	mu           sync.Mutex
	closed       bool
	control      []queuedMessage
	output       []queuedMessage
	controlBytes int
	outputBytes  int
	metrics      *relayMetrics
}

// newWsSender constructs a wsSender and starts its dedicated writer
// goroutine. Callers must not use the zero value — always go through this
// constructor so the writer goroutine exists before the first Send.
func newWsSender(ctx context.Context, c wsConn) *wsSender {
	return newWsSenderWithMetrics(ctx, c, nil)
}

func newWsSenderWithMetrics(ctx context.Context, c wsConn, metrics *relayMetrics) *wsSender {
	s := &wsSender{
		ctx: ctx, c: c, done: make(chan struct{}), wake: make(chan struct{}, 1),
		metrics: metrics,
	}
	go s.writeLoop()
	return s
}

// Send enqueues msg for the writer goroutine and returns immediately — it
// never blocks on the underlying socket. If the queue is already full the
// peer is treated as too slow to keep up: the connection is closed (which
// unblocks its read loop for cleanup) and the message is dropped, rather
// than stalling the caller (which, for a host→room fanout, would stall
// delivery to every other participant and the host's own read loop).
func (s *wsSender) Send(msg []byte) error {
	return s.sendClass(msg, classifyTraffic(msg))
}

func (s *wsSender) sendClass(msg []byte, class trafficClass) error {
	// Read buffers belong to the websocket implementation. Retaining a copy is
	// required now that delivery is asynchronous and also makes queue byte
	// accounting exact.
	copyOfMsg := append([]byte(nil), msg...)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errSenderClosed
	}
	overflow := false
	switch class {
	case trafficOutput:
		overflow = len(s.output) >= sendQueueCap || s.outputBytes+len(copyOfMsg) > outputQueueByteLimit
		if !overflow {
			s.output = append(s.output, queuedMessage{payload: copyOfMsg, class: class})
			s.outputBytes += len(copyOfMsg)
		}
	default:
		overflow = len(s.control) >= sendQueueCap || s.controlBytes+len(copyOfMsg) > controlQueueByteLimit
		if !overflow {
			s.control = append(s.control, queuedMessage{payload: copyOfMsg, class: class})
			s.controlBytes += len(copyOfMsg)
		}
	}
	s.mu.Unlock()

	if overflow {
		if s.metrics != nil {
			s.metrics.slowPeerEvicted.Add(1)
		}
		s.shutdown(websocket.StatusPolicyViolation, "send buffer overflow")
		return errSenderClosed
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func (s *wsSender) popNext() (queuedMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.control) > 0 {
		msg := s.control[0]
		s.control[0] = queuedMessage{}
		s.control = s.control[1:]
		s.controlBytes -= len(msg.payload)
		return msg, true
	}
	if len(s.output) > 0 {
		msg := s.output[0]
		s.output[0] = queuedMessage{}
		s.output = s.output[1:]
		s.outputBytes -= len(msg.payload)
		return msg, true
	}
	return queuedMessage{}, false
}

// writeLoop is the single writer goroutine for this connection. It drains
// sendCh in order (preserving per-connection message ordering) and performs
// each socket write under its own bounded deadline, so a write that the peer
// never acks cannot hang past writeTimeout. Any write error/timeout means the
// peer is dead or unresponsive: the connection is closed and the loop exits.
// It also exits when done is closed by someone else (Send's overflow path,
// or the connection's read loop tearing down) or when ctx is done.
func (s *wsSender) writeLoop() {
	for {
		if msg, ok := s.popNext(); ok {
			wctx, cancel := context.WithTimeout(s.ctx, writeTimeout)
			err := s.c.Write(wctx, websocket.MessageText, msg.payload)
			cancel()
			if err != nil {
				s.shutdown(websocket.StatusPolicyViolation, "write failed or timed out")
				return
			}
			if s.metrics != nil {
				s.metrics.framesOut.Add(1)
				s.metrics.bytesOut.Add(uint64(len(msg.payload)))
			}
			continue
		}
		select {
		case <-s.wake:
		case <-s.done:
			return
		case <-s.ctx.Done():
			s.shutdown(websocket.StatusNormalClosure, "connection context closed")
			return
		}
	}
}

// shutdown closes done (idempotently) and closes the underlying connection.
// Safe to call from Send (overflow) and writeLoop (write error) concurrently
// — only the first caller's close(done) and c.Close run; websocket.Conn.Close
// is itself idempotent (returns net.ErrClosed on a repeat call), but gating
// via sync.Once keeps this a single well-defined shutdown path and avoids a
// double-close panic on the done channel.
func (s *wsSender) shutdown(code websocket.StatusCode, reason string) {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.control = nil
		s.output = nil
		s.controlBytes = 0
		s.outputBytes = 0
		s.mu.Unlock()
		close(s.done)
		_ = s.c.Close(code, reason)
	})
}

// WebSocket keepalive: without a server→client ping, an idle host/participant
// connection is silently reaped by reverse-proxy (Traefik idleTimeout) or NAT,
// which surfaced as a repeating "relay connection lost" reconnect loop. The
// predecessor Node relay pinged every 30s for exactly this reason; keep that.
const (
	pingInterval = 30 * time.Second
	pingTimeout  = 10 * time.Second
)

// pingKeepAlive keeps a hijacked WS warm and detects a dead peer promptly. It
// runs until ctx is done (connection closed) or a ping fails, in which case it
// closes the connection so the read loop unblocks and unregisters. Safe to run
// concurrently with the connection's Read/Write.
func pingKeepAlive(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				c.Close(websocket.StatusPolicyViolation, "ping timeout")
				return
			}
		}
	}
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	// No auth configured = no service. There is deliberately no ?user=
	// fallback: identity comes ONLY from a verified PAT.
	if s.auth == nil {
		http.Error(w, "agent auth not configured", http.StatusServiceUnavailable)
		return
	}
	tok := bearerToken(r)
	if tok == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	id, err := s.auth.AgentUser(r.Context(), tok)
	if err != nil || id.UserID == "" {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	// A participant/guest token must never register as the room's host daemon
	// (보안 원칙: role=participant 로 /ws/agent 접속 불가). An empty role is a
	// legacy host PAT — the agent axis infers host.
	if id.Role == RoleParticipant {
		http.Error(w, "participant token cannot connect as host", http.StatusForbidden)
		return
	}
	if s.draining.Load() {
		http.Error(w, "relay node draining", http.StatusServiceUnavailable)
		return
	}
	if id.RelayNode != "" && s.nodeID != "" && id.RelayNode != s.nodeID {
		http.Error(w, "token assigned to a different relay node", http.StatusConflict)
		return
	}
	room := id.Room
	routingKey := id.RoutingKey()
	c, err := websocket.Accept(w, r, s.websocketAcceptOptions(""))
	if err != nil {
		return
	}
	c.SetReadLimit(maxMessageBytes)
	defer c.Close(websocket.StatusNormalClosure, "")
	ctx := r.Context()
	go pingKeepAlive(ctx, c)
	sender := newWsSenderWithMetrics(ctx, c, s.metrics)
	if !s.registerActive(sender) {
		sender.shutdown(websocket.StatusGoingAway, "relay node draining")
		return
	}
	defer func() {
		s.unregisterActive(sender)
		sender.shutdown(websocket.StatusNormalClosure, "connection closed")
	}()
	s.metrics.connections.Add(1)
	defer s.metrics.connections.Add(-1)
	limiter := newTokenBucket(s.limits, time.Now())
	epoch, unreg := s.reg.RegisterHostWithEpoch(routingKey, sender)
	connectionID := newConnectionID()
	untrackConnection := s.trackConnection(connectionID, sender)
	defer untrackConnection()
	s.reportPresence(id, connectionID, "host", true, true)
	go s.presenceHeartbeat(ctx, id, connectionID, "host", 15*time.Second, func() (bool, bool) {
		current, online := s.reg.HostFor(routingKey)
		return online && current == sender, online
	})
	log.Printf("relay: host CONNECT room=%s user=%s", room, id.UserID)
	if lease, ok := s.reg.Driver(routingKey, time.Now()); ok {
		_ = sender.Send(setDriverMsg(lease.UserID))
	}
	s.notifyHostStatus(routingKey) // participant 상태 인디케이터: 노트북 연결됨
	defer func() {
		log.Printf("relay: host DISCONNECT room=%s user=%s", room, id.UserID)
		unreg()
		_, hostOnline := s.reg.HostFor(routingKey)
		s.reportPresence(id, connectionID, "host", false, hostOnline)
		s.notifyHostStatus(routingKey) // 노트북 끊김 (다른 host 로 교체됐다면 여전히 연결로 통지)
	}()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			log.Printf("relay: host read end room=%s: %v", room, err)
			return
		}
		s.metrics.framesIn.Add(1)
		s.metrics.bytesIn.Add(uint64(len(data)))
		if !limiter.Allow(len(data), time.Now()) {
			s.metrics.rateLimited.Add(1)
			_ = c.Close(websocket.StatusPolicyViolation, "relay rate limit exceeded")
			return
		}
		// Only the CURRENT registered host feeds the room. RegisterHost keeps
		// one host per room (latest wins), but a superseded daemon's ws stays
		// open and would otherwise keep broadcasting — two daemons on one room
		// (orphan from an app restart, a stale process) then emit conflicting
		// heartbeats (e.g. driver "" vs a real sub), which flickers the UI. We
		// don't close the old ws (its daemon would just reconnect and evict the
		// live one — a reconnect war); we silently drop its frames instead.
		if h, ok := s.reg.HostFor(routingKey); !ok || h != sender {
			continue
		}
		if join, isJoin := parseRelayJoin(data); isJoin {
			if !validRelayJoin(join) {
				_ = sender.Send(relayProtocolError("unsupported protocol version or invalid streamId"))
				continue
			}
			_ = sender.Send(makeRelayJoinAckScoped(join, id, connectionID, id.UserID, RoleHost, AccessControl, epoch))
			continue
		}
		mt := peekMessageType(data)
		s.routeFromHost(routingKey, mt, data)
		// Marker-only log at the response-terminal boundary — never at
		// streamed "text" deltas, which would be one log line per token
		// chunk. No prompt/response content is logged, only the type.
		if mt == "done" || mt == "error" || mt == "aborted" {
			log.Printf("relay: chat 응답 완료 room=%s type=%s", room, mt)
		}
	}
}

// routeFromHost fans a host→room message out to the room's participants.
// permission_request is gated to role=host participant connections only
// (policy 4). A top-level "to" field targets a single participant by user id
// (used for terminal scrollback replay to one late-joining viewer, so it does
// not duplicate onto everyone's screen). Everything else broadcasts to all
// participants (policy 3).
func (s *Server) routeFromHost(room, mt string, data []byte) {
	if mt == "permission_request" {
		for _, p := range s.reg.HostSendersFor(room) {
			_ = p.Send(data)
		}
		return
	}
	if to := peekStringField(data, "to"); to != "" {
		for _, p := range s.reg.ParticipantsByUserID(room, to) {
			_ = p.Send(data)
		}
		return
	}
	for _, p := range s.reg.ParticipantsFor(room) {
		_ = p.Send(data)
	}
}

// notifyHostStatus pushes the room's CURRENT host presence to all of its
// participants. It sends BOTH host:status (new participant clients) and
// agent:status (legacy browser clients) with the same payload so the rename
// doesn't break either — see statusMsg.
func (s *Server) notifyHostStatus(room string) {
	participants := s.reg.ParticipantsFor(room)
	if len(participants) == 0 {
		return
	}
	_, up := s.reg.HostFor(room)
	host := statusMsg("host:status", up)
	agent := statusMsg("agent:status", up)
	for _, p := range participants {
		_ = p.Send(host)
		_ = p.Send(agent)
	}
}

// sendHostStatus pushes current presence to a SINGLE participant (on connect),
// again on both the new and legacy type names.
func sendHostStatus(p Sender, up bool) {
	_ = p.Send(statusMsg("host:status", up))
	_ = p.Send(statusMsg("agent:status", up))
}

func statusMsg(typ string, up bool) []byte {
	if up {
		return []byte(`{"type":"` + typ + `","connected":true}`)
	}
	return []byte(`{"type":"` + typ + `","connected":false}`)
}

func (s *Server) notifyRoomState(room string) {
	participants := s.reg.ParticipantsFor(room)
	if len(participants) == 0 {
		return
	}
	lease, _ := s.reg.Driver(room, time.Now())
	msg, _ := json.Marshal(struct {
		Type         string                `json:"type"`
		Participants []ParticipantSnapshot `json:"participants"`
		Driver       string                `json:"driver"`
		Generation   uint64                `json:"generation"`
		ExpiresAt    int64                 `json:"expiresAt,omitempty"`
	}{
		Type: "participant_roster", Participants: s.reg.ParticipantSnapshots(room),
		Driver: lease.UserID, Generation: lease.Generation, ExpiresAt: leaseExpiryMillis(lease),
	})
	for _, p := range participants {
		_ = p.Send(msg)
	}
}

func (s *Server) notifyDriverState(room string, lease DriverLease) {
	msg, _ := json.Marshal(struct {
		Type       string `json:"type"`
		Driver     string `json:"driver"`
		Generation uint64 `json:"generation"`
		ExpiresAt  int64  `json:"expiresAt,omitempty"`
	}{Type: "driver_state", Driver: lease.UserID, Generation: lease.Generation, ExpiresAt: leaseExpiryMillis(lease)})
	for _, p := range s.reg.ParticipantsFor(room) {
		_ = p.Send(msg)
	}
}

func leaseExpiryMillis(lease DriverLease) int64 {
	if lease.ExpiresAt.IsZero() {
		return 0
	}
	return lease.ExpiresAt.UnixMilli()
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}

const participantTicketProtocolPrefix = "pie-relay.ticket."

func participantToken(r *http.Request, allowLegacyQuery bool) (token, protocol string) {
	if token = bearerToken(r); token != "" {
		return token, ""
	}
	for _, offered := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		offered = strings.TrimSpace(offered)
		if strings.HasPrefix(offered, participantTicketProtocolPrefix) && len(offered) > len(participantTicketProtocolPrefix) {
			return strings.TrimPrefix(offered, participantTicketProtocolPrefix), offered
		}
	}
	if allowLegacyQuery {
		return r.URL.Query().Get("ticket"), ""
	}
	return "", ""
}

func (s *Server) handleParticipant(w http.ResponseWriter, r *http.Request) {
	// No auth configured = no service. There is deliberately no ?user=
	// fallback: identity comes ONLY from a verified token.
	if s.participantAuth == nil {
		http.Error(w, "participant auth not configured", http.StatusServiceUnavailable)
		return
	}
	// Native clients use Bearer and current browsers use a negotiated
	// subprotocol. Query-string credentials are opt-in migration compatibility.
	tok, ticketProtocol := participantToken(r, s.allowQueryTicket)
	if tok == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	id, err := s.participantAuth.ParticipantUser(r.Context(), tok)
	if err != nil || id.UserID == "" {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	// Empty role is a legacy PAT on the participant axis → promote to host.
	// Rationale: (1) an empty-role token can only be an explicitly allowed legacy credential,
	// which IS the room owner's full credential — real guests always carry an
	// explicit role=participant minted by /rooms/join. (2) invites.go's
	// handleCreateInvite already treats an empty role as host ("legacy token
	// ... may invite"), so promoting here keeps the two axes consistent. The
	// effect: the credentials.json (legacy token) "join as host" path now
	// receives permission_request, matching an explicit role=host operator.
	role := id.Role
	if role == "" {
		role = RoleHost
	}
	if s.draining.Load() {
		http.Error(w, "relay node draining", http.StatusServiceUnavailable)
		return
	}
	if id.RelayNode != "" && s.nodeID != "" && id.RelayNode != s.nodeID {
		http.Error(w, "token assigned to a different relay node", http.StatusConflict)
		return
	}
	room := id.Room
	routingKey := id.RoutingKey()
	c, err := websocket.Accept(w, r, s.websocketAcceptOptions(ticketProtocol))
	if err != nil {
		return
	}
	c.SetReadLimit(maxMessageBytes)
	defer c.Close(websocket.StatusNormalClosure, "")
	ctx := r.Context()
	go pingKeepAlive(ctx, c)
	sender := newWsSenderWithMetrics(ctx, c, s.metrics)
	if !s.registerActive(sender) {
		sender.shutdown(websocket.StatusGoingAway, "relay node draining")
		return
	}
	defer func() {
		s.unregisterActive(sender)
		sender.shutdown(websocket.StatusNormalClosure, "connection closed")
	}()
	s.metrics.connections.Add(1)
	defer s.metrics.connections.Add(-1)
	limiter := newTokenBucket(s.limits, time.Now())
	connectionID := newConnectionID()
	untrackConnection := s.trackConnection(connectionID, sender)
	defer untrackConnection()
	// The participant stays connected even with no host — it gets the current
	// presence now and live host:status pushes as the daemon comes and goes.
	hostUp, unreg, registered := s.reg.TryRegisterParticipant(routingKey, sender, Participant{
		UserID: id.UserID, Role: role, Access: id.Access, ConnectionID: connectionID,
	}, s.limits.MaxParticipantsPerRoom)
	if !registered {
		s.metrics.roomRejected.Add(1)
		_ = c.Close(websocket.StatusPolicyViolation, "room participant limit reached")
		return
	}
	sendHostStatus(sender, hostUp)
	s.reportPresence(id, connectionID, "participant", true, hostUp)
	go s.presenceHeartbeat(ctx, id, connectionID, "participant", 15*time.Second, func() (bool, bool) {
		_, online := s.reg.HostFor(routingKey)
		return true, online
	})
	s.notifyRoomState(routingKey)
	log.Printf("relay: participant CONNECT room=%s user=%s role=%s access=%s (host=%t)", room, id.UserID, role, id.Access, hostUp)
	// A control guest may automatically take an EMPTY seat, but a reconnect can
	// never steal a live driver's lease. The host operator can explicitly hand
	// off/revoke via set_driver.
	if id.Access == AccessControl && role != RoleHost {
		if lease, acquired := s.reg.AcquireDriverIfFree(routingKey, id.UserID, time.Now(), driverLeaseTTL); acquired {
			if h, ok := s.reg.HostFor(routingKey); ok {
				_ = h.Send(setDriverMsg(lease.UserID))
			}
			s.notifyDriverState(routingKey, lease)
		}
	}
	defer func() {
		log.Printf("relay: participant DISCONNECT room=%s user=%s", room, id.UserID)
		unreg()
		_, hostOnline := s.reg.HostFor(routingKey)
		s.reportPresence(id, connectionID, "participant", false, hostOnline)
		if released, ok := s.reg.ReleaseDriverIfAbsent(routingKey, id.UserID, time.Now()); ok {
			if h, hostOK := s.reg.HostFor(routingKey); hostOK {
				_ = h.Send(setDriverMsg(""))
			}
			s.notifyDriverState(routingKey, released)
		}
		s.notifyRoomState(routingKey)
	}()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		s.metrics.framesIn.Add(1)
		s.metrics.bytesIn.Add(uint64(len(data)))
		if !limiter.Allow(len(data), time.Now()) {
			s.metrics.rateLimited.Add(1)
			_ = c.Close(websocket.StatusPolicyViolation, "relay rate limit exceeded")
			return
		}
		if join, isJoin := parseRelayJoin(data); isJoin {
			if !validRelayJoin(join) {
				_ = sender.Send(relayProtocolError("unsupported protocol version or invalid streamId"))
				continue
			}
			_ = sender.Send(makeRelayJoinAckScoped(join, id, connectionID, id.UserID, role, id.Access, s.reg.HostEpoch(routingKey)))
			continue
		}
		s.routeFromParticipant(routingKey, id.UserID, role, id.Access, sender, data)
	}
}

// setDriverMsg builds the {type:"set_driver","target":<sub>} the relay sends to
// a room's host to hand the terminal driver to a freshly-joined control
// participant. json.Marshal escapes target so an unusual sub can't break the
// frame (guest subs are sanitized, but keep this robust).
func setDriverMsg(target string) []byte {
	msg, err := json.Marshal(struct {
		Type   string `json:"type"`
		Target string `json:"target"`
		Driver string `json:"driver"`
	}{Type: "set_driver", Target: target, Driver: target})
	if err != nil {
		return nil
	}
	return msg
}

// routeFromParticipant applies the participant→host policies:
//   - policy 1: inject the verified `from`; drop the message if its top-level
//     object won't parse.
//   - policy 4: permission_response / abort / set_driver pass to the host
//     daemon only when the sender's role is host; otherwise they are
//     discarded. (S5-1: set_driver joins the same host-only gate — only the
//     operator may reassign the terminal-room driver.)
//   - default: forward to the host daemon (or tell the sender the laptop is
//     offline).
//   - policy 2: a chat additionally fans out to the OTHER participants as a
//     peer_chat echo so everyone sees each other's questions.
//   - P3-1: a role=participant sender's message is stripped of the guest-unsafe
//     top-level fields (permissionMode/claudePath/…); host senders are left
//     intact so a host-role participant keeps full control of its invocation.
//   - A: an access=="view" sender is pure spectate — every input-bearing message
//     (participantInputTypes) is dropped before it can reach the host, while
//     non-input messages (request_screen, …) still pass. Checked first so a view
//     guest can't reach any of the paths below. role=host is always control, so
//     the operator is never caught by this gate.
func (s *Server) routeFromParticipant(room, from, role, access string, self Sender, data []byte) {
	mt := peekMessageType(data)
	if access == AccessView && participantInputTypes[mt] {
		log.Printf("relay: view 입력 폐기 type=%s room=%s from=%s", mt, room, from)
		return
	}
	if mt == "pty_scroll" && !validPtyScrollMessage(data) {
		log.Printf("relay: 잘못된 pty_scroll 폐기 room=%s from=%s", room, from)
		return
	}
	if mt == "pty_input" || mt == "pty_resize" {
		lease, hasDriver := s.reg.Driver(room, time.Now())
		if !hasDriver || lease.UserID != from {
			log.Printf("relay: 비-driver 입력 폐기 type=%s room=%s from=%s", mt, room, from)
			if !hasDriver {
				s.notifyDriverState(room, lease)
			}
			return
		}
	}
	if mt == "driver_heartbeat" {
		if access == AccessControl || role == RoleHost {
			if _, renewed := s.reg.RenewDriver(room, from, time.Now(), driverLeaseTTL); !renewed {
				lease, _ := s.reg.Driver(room, time.Now())
				s.notifyDriverState(room, lease)
			}
		}
		return
	}
	injected, ok := injectFrom(data, from)
	if !ok {
		log.Printf("relay: participant 메시지 폐기(파싱 불가) room=%s from=%s", room, from)
		return
	}
	if role != RoleHost {
		injected = sanitizeGuest(injected)
	}

	if mt == "room_chat" {
		// room_chat: 사람 대 사람 사이드챗. 절대 host 데몬(쉘/Claude)에는 전달하지
		// 않는다 — ParticipantsFor는 참가자 소켓만 반환하고 hosts 맵(데몬)은 건드리지
		// 않으므로, 자기 자신을 제외한 다른 참가자 전원에게만 팬아웃한다. host 데몬이
		// 붙어 있지 않아도 동작하며, access=view(관전자)도 입력이 아니라
		// participantInputTypes에 없어 위의 view 게이트를 통과한다.
		for _, p := range s.reg.ParticipantsFor(room) {
			if p == self {
				continue
			}
			_ = p.Send(injected)
		}
		return
	}

	if mt == "driver_request" {
		if access != AccessControl && role != RoleHost {
			return
		}
		if lease, acquired := s.reg.AcquireDriverIfFree(room, from, time.Now(), driverLeaseTTL); acquired {
			if h, hostOK := s.reg.HostFor(room); hostOK {
				_ = h.Send(setDriverMsg(lease.UserID))
			}
			s.notifyDriverState(room, lease)
			return
		}
		// Occupied seat: tell operator participant sockets who requested it.
		for _, operator := range s.reg.HostSendersFor(room) {
			_ = operator.Send(injected)
		}
		return
	}

	if mt == "set_driver" {
		if role != RoleHost {
			log.Printf("relay: set_driver 폐기(비-host) room=%s from=%s", room, from)
			return
		}
		target := peekStringField(injected, "target")
		if target == "" {
			target = peekStringField(injected, "driver")
		}
		lease, assigned := s.reg.SetDriver(room, target, time.Now(), driverLeaseTTL)
		if !assigned {
			_ = self.Send([]byte(`{"type":"driver_error","reason":"target-not-present-or-control-disabled"}`))
			return
		}
		if h, hostOK := s.reg.HostFor(room); hostOK {
			cmd := setDriverMsg(target)
			// Preserve the verified operator identity for audit/debug consumers.
			cmd, _ = injectFrom(cmd, from)
			_ = h.Send(cmd)
		}
		s.notifyDriverState(room, lease)
		return
	}

	if mt == "permission_response" || mt == "abort" {
		// policy 4: 승인·중단·드라이버 지정은 host 전용. 게스트가 보낸 것은 폐기.
		if role != RoleHost {
			log.Printf("relay: %s 폐기(비-host) room=%s from=%s", mt, room, from)
			return
		}
		if h, ok := s.reg.HostFor(room); ok {
			_ = h.Send(injected)
		}
		return
	}

	if h, ok := s.reg.HostFor(room); ok {
		_ = h.Send(injected)
		if mt == "chat" {
			log.Printf("relay: chat 요청 전달 room=%s from=%s", room, from)
		}
	} else {
		// Message sent while the laptop daemon is down — tell the sender
		// instead of dropping silently (UI shows a toast).
		_ = self.Send([]byte(`{"type":"agent:unavailable","reason":"no-agent-connected"}`))
		if mt == "chat" {
			log.Printf("relay: chat 요청 거부(기기 미연결) room=%s from=%s", room, from)
		}
	}

	if mt == "chat" {
		// policy 2: 다른 participant 전원에게 peer_chat 팬아웃 (자기 자신 제외).
		echo := peerChatMsg(from, injected)
		for _, p := range s.reg.ParticipantsFor(room) {
			if p == self {
				continue
			}
			_ = p.Send(echo)
		}
	}
}
