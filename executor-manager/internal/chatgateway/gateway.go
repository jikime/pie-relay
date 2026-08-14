package chatgateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/capability"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
)

var (
	ErrBackpressure = errors.New("chat conversation queue is full")
	ErrTurnActive   = errors.New("a chat turn is already active")
)

const (
	MaxChatImageCount       = 4
	MaxChatImageBytes       = 4 << 20
	MaxChatImagesTotalBytes = 4 << 20
	MaxChatRequestBytes     = 6 << 20
)

type ImageAttachment struct {
	Data     string `json:"data"`
	MIMEType string `json:"mimeType"`
	Name     string `json:"name,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

type Gateway struct {
	ctx        context.Context
	control    *control.Service
	issuer     capability.Issuer
	relayURL   string
	journal    *Journal
	usage      UsageRecorder
	mu         sync.Mutex
	peers      map[string]*peer
	subs       map[string]map[chan struct{}]struct{}
	started    atomic.Uint64
	finished   atomic.Uint64
	failed     atomic.Uint64
	turnMu     sync.Mutex
	turnLimit  int
	turnActive map[string]struct{}
	turnQueue  []string
	requestMu  sync.Mutex
	requests   map[string]map[string]map[chan Event]struct{}
}

// UsageRecorder receives only Manager-attributed measurements. Implementations
// must be idempotent because a relay/client reconnect can replay an event.
type UsageRecorder interface {
	RecordUsage(context.Context, control.Conversation, string, json.RawMessage) error
}

type Stats struct {
	Peers       int
	Subscribers int
	QueueDepth  int
	ActiveTurns int
	QueuedTurns int
	Started     uint64
	Finished    uint64
	Failed      uint64
}

type peer struct {
	gateway        *Gateway
	convID         string
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	outgoing       chan json.RawMessage
	chatWake       chan struct{}
	mu             sync.Mutex
	agentSessionID string
	activeChat     string
	activePayload  json.RawMessage
	turnLease      bool
	taskRequests   map[string]string
}

func New(ctx context.Context, controlService *control.Service, issuer capability.Issuer, relayURL string, journal *Journal) (*Gateway, error) {
	if controlService == nil || journal == nil {
		return nil, errors.New("chat gateway control and journal are required")
	}
	endpoint, err := participantEndpoint(relayURL)
	if err != nil {
		return nil, err
	}
	return &Gateway{ctx: ctx, control: controlService, issuer: issuer, relayURL: endpoint, journal: journal, peers: map[string]*peer{}, subs: map[string]map[chan struct{}]struct{}{}, turnLimit: 4, turnActive: map[string]struct{}{}, requests: map[string]map[string]map[chan Event]struct{}{}}, nil
}

// SetTurnLimit bounds simultaneous Claude subscription turns across all user
// containers. Accepted excess requests remain FIFO queued in their peer and
// are promoted automatically when a terminal event releases a slot.
func (g *Gateway) SetTurnLimit(limit int) {
	if g == nil {
		return
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 128 {
		limit = 128
	}
	g.turnMu.Lock()
	g.turnLimit = limit
	g.turnMu.Unlock()
}

func (g *Gateway) Enabled() bool {
	return g != nil && g.issuer.Enabled() && g.relayURL != ""
}

func (g *Gateway) SetUsageRecorder(recorder UsageRecorder) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.usage = recorder
	g.mu.Unlock()
}

func (g *Gateway) Stats() Stats {
	if g == nil {
		return Stats{}
	}
	g.mu.Lock()
	peers := make([]*peer, 0, len(g.peers))
	stats := Stats{Peers: len(g.peers)}
	stats.Started = g.started.Load()
	stats.Finished = g.finished.Load()
	stats.Failed = g.failed.Load()
	for _, p := range g.peers {
		peers = append(peers, p)
	}
	for _, values := range g.subs {
		stats.Subscribers += len(values)
	}
	g.mu.Unlock()
	for _, p := range peers {
		stats.QueueDepth += len(p.outgoing)
	}
	g.turnMu.Lock()
	stats.QueuedTurns = len(g.turnQueue)
	stats.ActiveTurns = len(g.turnActive)
	g.turnMu.Unlock()
	return stats
}

// HasActiveTurn reports whether Claude is still processing a request. Idle
// cleanup must never stop a session while a turn or permission flow is active.
func (g *Gateway) HasActiveTurn(conversationID string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	p := g.peers[conversationID]
	g.mu.Unlock()
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activeChat != ""
}

// BeginIdle atomically changes an inactive conversation to closing while the
// peer request lock is held. Send rechecks this state under the same lock, so a
// cleanup scan cannot cut across a newly accepted chat turn.
func (g *Gateway) BeginIdle(conversationID string, cutoff time.Time) (control.Conversation, bool, error) {
	if g == nil {
		return control.Conversation{}, false, errors.New("chat gateway is unavailable")
	}
	g.mu.Lock()
	p := g.peers[conversationID]
	g.mu.Unlock()
	if p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.activeChat != "" {
			return control.Conversation{}, false, nil
		}
	}
	conversation, ok := g.control.Conversation(conversationID)
	if !ok {
		return control.Conversation{}, false, control.ErrNotFound
	}
	if conversation.Status == "closing" || conversation.Status == "closed" || conversation.Status == "deleted" {
		return conversation, false, nil
	}
	activity := conversation.LastActivityAt
	if activity.IsZero() {
		activity = conversation.CreatedAt
	}
	if activity.After(cutoff) {
		return conversation, false, nil
	}
	conversation.Status, conversation.LastError = "closing", "idle timeout"
	updated, err := g.control.PutConversation(context.Background(), conversation, conversation.Version, control.MutationMeta{ActorUserID: "chat-idle-reaper", SkipAudit: true})
	if errors.Is(err, control.ErrConflict) {
		return control.Conversation{}, false, nil
	}
	return updated, err == nil, err
}

// RecordLifecycle persists a public lifecycle event before a peer is stopped.
// SSE consumers can therefore explain why a previously ready chat became idle.
func (g *Gateway) RecordLifecycle(conversationID, eventType, message string) error {
	if g == nil {
		return errors.New("chat gateway is unavailable")
	}
	data, _ := json.Marshal(map[string]string{"message": message})
	if _, err := g.journal.Append(context.Background(), conversationID, eventType, "", data); err != nil {
		return err
	}
	g.notify(conversationID)
	return nil
}

func (g *Gateway) Ensure(conversation control.Conversation) error {
	if !g.Enabled() {
		return errors.New("chat gateway Relay capability issuer is unavailable")
	}
	if conversation.Status == "closed" || conversation.Status == "deleted" {
		return errors.New("conversation is closed")
	}
	pending, hasPending, err := g.journal.PendingChatRequest(conversation.ID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(g.ctx)
	p := &peer{gateway: g, convID: conversation.ID, ctx: ctx, cancel: cancel, done: make(chan struct{}), outgoing: make(chan json.RawMessage, 64), chatWake: make(chan struct{}, 1), agentSessionID: conversation.AgentSessionID}
	if hasPending {
		p.activeChat = pending.RequestID
		p.activePayload = append(json.RawMessage(nil), pending.Data...)
	}
	// Lock the peer before publishing it. A concurrent Send or turn promotion
	// can find the peer as soon as g.mu is released; keeping p.mu here prevents
	// a race on turnLease while the durable pending request is restored.
	p.mu.Lock()
	g.mu.Lock()
	if g.peers[conversation.ID] != nil {
		g.mu.Unlock()
		p.mu.Unlock()
		cancel()
		return nil
	}
	g.peers[conversation.ID] = p
	g.mu.Unlock()
	if hasPending {
		p.turnLease = g.claimTurn(conversation.ID)
		if p.turnLease {
			p.chatWake <- struct{}{}
		}
	}
	p.mu.Unlock()
	go func() {
		defer close(p.done)
		p.run()
	}()
	return nil
}

func (g *Gateway) Send(conversation control.Conversation, requestID string, message any) (Event, bool, error) {
	if requestID == "" || len(requestID) > 160 {
		return Event{}, false, errors.New("Idempotency-Key is required")
	}
	if err := g.Ensure(conversation); err != nil {
		return Event{}, false, err
	}
	g.mu.Lock()
	p := g.peers[conversation.ID]
	g.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok := g.control.Conversation(conversation.ID)
	if !ok {
		return Event{}, false, control.ErrNotFound
	}
	if current.Status == "closing" || current.Status == "closed" || current.Status == "deleted" {
		return Event{}, false, errors.New("conversation is closed")
	}
	if duplicate, err := g.journal.HasRequest(conversation.ID, requestID); err != nil {
		return Event{}, false, err
	} else if duplicate {
		return Event{}, true, nil
	}
	if len(p.outgoing) >= cap(p.outgoing) {
		return Event{}, false, ErrBackpressure
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return Event{}, false, err
	}
	var envelope struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(encoded, &envelope)
	if envelope.Type == "chat" && p.activeChat != "" {
		return Event{}, false, ErrTurnActive
	}
	eventType := "control.accepted"
	if envelope.Type == "chat" {
		eventType = "request.accepted"
	}
	durable := durableControlPayload(envelope.Type, encoded)
	event, err := g.journal.Append(context.Background(), conversation.ID, eventType, requestID, durable)
	if err != nil {
		if errors.Is(err, ErrDuplicateRequest) {
			return Event{}, true, nil
		}
		return Event{}, false, err
	}
	if envelope.Type == "chat" {
		p.activeChat = requestID
		p.activePayload = append(json.RawMessage(nil), encoded...)
		p.turnLease = g.claimTurn(conversation.ID)
		if p.turnLease {
			select {
			case p.chatWake <- struct{}{}:
			default:
			}
		}
	} else {
		p.outgoing <- append(json.RawMessage(nil), encoded...)
	}
	g.touchActivity(conversation.ID, true)
	g.notify(conversation.ID)
	return event, false, nil
}

// Request sends a non-chat control message and waits for the matching durable
// executor response. The waiter is installed before Send, so even an immediate
// local response cannot be missed. Results remain recoverable from the journal
// when the HTTP caller retries with the same idempotency key.
func (g *Gateway) Request(ctx context.Context, conversation control.Conversation, requestID string, message any, responseType string) (Event, bool, error) {
	if strings.TrimSpace(responseType) == "" || len(responseType) > 96 {
		return Event{}, false, errors.New("response type is required")
	}
	responses, unregister := g.registerRequest(conversation.ID, requestID)
	defer unregister()
	_, duplicate, err := g.Send(conversation, requestID, message)
	if err != nil {
		return Event{}, false, err
	}
	if duplicate {
		// Restart/retry recovery is deliberately the slow path. Normal requests
		// are delivered directly below and never rescan a potentially large chat
		// journal while holding its append lock.
		event, found, lookupErr := g.journal.EventByRequestType(ctx, conversation.ID, requestID, responseType)
		if lookupErr != nil {
			return Event{}, duplicate, lookupErr
		}
		if found {
			return event, duplicate, nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			return Event{}, duplicate, ctx.Err()
		case event := <-responses:
			if event.Type == responseType {
				return event, duplicate, nil
			}
		}
	}
}

func (g *Gateway) registerRequest(conversationID, requestID string) (<-chan Event, func()) {
	ch := make(chan Event, 1)
	g.requestMu.Lock()
	if g.requests == nil {
		g.requests = map[string]map[string]map[chan Event]struct{}{}
	}
	if g.requests[conversationID] == nil {
		g.requests[conversationID] = map[string]map[chan Event]struct{}{}
	}
	if g.requests[conversationID][requestID] == nil {
		g.requests[conversationID][requestID] = map[chan Event]struct{}{}
	}
	g.requests[conversationID][requestID][ch] = struct{}{}
	g.requestMu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			g.requestMu.Lock()
			delete(g.requests[conversationID][requestID], ch)
			if len(g.requests[conversationID][requestID]) == 0 {
				delete(g.requests[conversationID], requestID)
			}
			if len(g.requests[conversationID]) == 0 {
				delete(g.requests, conversationID)
			}
			g.requestMu.Unlock()
		})
	}
}

func (g *Gateway) deliverRequest(event Event) {
	if event.RequestID == "" {
		return
	}
	g.requestMu.Lock()
	defer g.requestMu.Unlock()
	for ch := range g.requests[event.ConversationID][event.RequestID] {
		select {
		case ch <- event:
		default:
		}
	}
}

func (g *Gateway) SendChat(conversation control.Conversation, requestID, prompt string) (Event, bool, error) {
	return g.SendChatWithImages(conversation, requestID, prompt, nil)
}

func (g *Gateway) SendChatWithImages(conversation control.Conversation, requestID, prompt string, images []ImageAttachment) (Event, bool, error) {
	if strings.TrimSpace(prompt) == "" || len(prompt) > 256<<10 {
		return Event{}, false, errors.New("prompt is required and must be at most 256KiB")
	}
	if err := validateImageAttachments(images); err != nil {
		return Event{}, false, err
	}
	g.mu.Lock()
	p := g.peers[conversation.ID]
	g.mu.Unlock()
	sessionID := conversation.AgentSessionID
	if p != nil {
		p.mu.Lock()
		if p.agentSessionID != "" {
			sessionID = p.agentSessionID
		}
		p.mu.Unlock()
	}
	message := map[string]any{"type": "chat", "prompt": prompt, "requestId": requestID}
	if len(images) > 0 {
		message["images"] = images
	}
	if sessionID != "" {
		message["sessionId"] = sessionID
	}
	event, duplicate, err := g.Send(conversation, requestID, message)
	if err == nil && !duplicate {
		g.started.Add(1)
		log.Printf("chat_request_started request_id=%s conversation_id=%s", requestID, conversation.ID)
	}
	return event, duplicate, err
}

func validateImageAttachments(images []ImageAttachment) error {
	if len(images) > MaxChatImageCount {
		return fmt.Errorf("at most %d image attachments are allowed", MaxChatImageCount)
	}
	total := 0
	for index, image := range images {
		if image.Name != "" && (len(image.Name) > 255 || strings.ContainsAny(image.Name, "/\\\x00\r\n")) {
			return errors.New("image attachment filename is invalid")
		}
		if len(image.Data) == 0 || len(image.Data) > base64.StdEncoding.EncodedLen(MaxChatImageBytes) {
			return errors.New("image attachment is empty or too large")
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(image.Data)
		if err != nil || len(decoded) == 0 || len(decoded) > MaxChatImageBytes {
			return errors.New("image attachment data is invalid")
		}
		if image.Size != 0 && image.Size != int64(len(decoded)) {
			return errors.New("image attachment size does not match its data")
		}
		images[index].Size = int64(len(decoded))
		if !validImageSignature(image.MIMEType, decoded) {
			return errors.New("image attachment MIME type or signature is unsupported")
		}
		total += len(decoded)
		if total > MaxChatImagesTotalBytes {
			return fmt.Errorf("image attachments exceed the %d byte total limit", MaxChatImagesTotalBytes)
		}
	}
	return nil
}

func validImageSignature(mimeType string, data []byte) bool {
	switch mimeType {
	case "image/jpeg":
		return len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff
	case "image/png":
		return len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n"
	case "image/gif":
		return len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a")
	case "image/webp":
		return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
	default:
		return false
	}
}

func (g *Gateway) Events(ctx context.Context, conversationID string, after uint64, limit int) ([]Event, error) {
	return g.journal.Events(ctx, conversationID, after, limit)
}

func (g *Gateway) Subscribe(conversationID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	g.mu.Lock()
	if g.subs[conversationID] == nil {
		g.subs[conversationID] = map[chan struct{}]struct{}{}
	}
	g.subs[conversationID][ch] = struct{}{}
	g.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.subs[conversationID], ch)
			if len(g.subs[conversationID]) == 0 {
				delete(g.subs, conversationID)
			}
			close(ch)
			g.mu.Unlock()
		})
	}
}

func (g *Gateway) Close(conversationID string) {
	_ = g.stop(conversationID)
}

func (g *Gateway) stop(conversationID string) error {
	g.mu.Lock()
	p := g.peers[conversationID]
	delete(g.peers, conversationID)
	g.mu.Unlock()
	if p != nil {
		defer p.abandonTurn()
		p.cancel()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			return errors.New("timed out stopping chat peer")
		}
	}
	return nil
}

func (g *Gateway) claimTurn(conversationID string) bool {
	g.turnMu.Lock()
	defer g.turnMu.Unlock()
	if _, ok := g.turnActive[conversationID]; ok {
		return true
	}
	for _, queued := range g.turnQueue {
		if queued == conversationID {
			return false
		}
	}
	if len(g.turnActive) < g.turnLimit {
		g.turnActive[conversationID] = struct{}{}
		return true
	}
	g.turnQueue = append(g.turnQueue, conversationID)
	return false
}

func (g *Gateway) releaseTurn(conversationID string) {
	for {
		g.turnMu.Lock()
		delete(g.turnActive, conversationID)
		// A stopped peer can be waiting rather than active. Remove its stale
		// queue entry immediately so it cannot consume a later promotion slot.
		for index, queued := range g.turnQueue {
			if queued == conversationID {
				g.turnQueue = append(g.turnQueue[:index], g.turnQueue[index+1:]...)
				break
			}
		}
		if len(g.turnQueue) == 0 || len(g.turnActive) >= g.turnLimit {
			g.turnMu.Unlock()
			return
		}
		next := g.turnQueue[0]
		g.turnQueue = g.turnQueue[1:]
		g.turnActive[next] = struct{}{}
		g.turnMu.Unlock()

		g.mu.Lock()
		p := g.peers[next]
		g.mu.Unlock()
		if p != nil {
			p.mu.Lock()
			valid := p.activeChat != ""
			if valid {
				p.turnLease = true
				select {
				case p.chatWake <- struct{}{}:
				default:
				}
			}
			p.mu.Unlock()
			if valid {
				return
			}
		}
		conversationID = next
	}
}

// Remove stops the live peer and permanently removes its persisted events.
// The control-plane conversation tombstone remains available for audit.
func (g *Gateway) Remove(conversationID string) error {
	if err := g.stop(conversationID); err != nil {
		return err
	}
	return g.journal.Remove(conversationID)
}

func (g *Gateway) notify(conversationID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for ch := range g.subs[conversationID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (p *peer) run() {
	backoff := time.Second
	for p.ctx.Err() == nil {
		conversation, ok := p.gateway.control.Conversation(p.convID)
		if !ok || conversation.Status == "closing" || conversation.Status == "closed" || conversation.Status == "deleted" {
			return
		}
		session, ok := p.gateway.control.Session(conversation.SessionID)
		if !ok {
			p.recordState("transport.error", "control session is unavailable")
			return
		}
		minted, err := p.gateway.issuer.MintSession(capability.SessionCredential{
			Subject: conversation.OwnerUserID, Room: conversation.OwnerUserID, DeviceID: conversation.DeviceID,
			SessionID: conversation.SessionID, ApplicationID: session.ApplicationID, PoolID: session.PoolID,
			TenantID: session.TenantID, ResourceType: session.ResourceType, ResourceID: session.ResourceID,
			AgentID: session.AgentID, Protocol: session.Protocol, ExecutionTarget: "docker", Role: "host",
			Access: "control", RelayNode: session.RelayNodeID, RelayGeneration: session.RelayGeneration, TTL: time.Hour,
		})
		if err != nil {
			p.recordState("transport.error", err.Error())
			return
		}
		relayURL := p.gateway.relayURL
		if session.RelayNodeID != "" {
			if node, exists := p.gateway.control.Node(session.RelayNodeID); exists {
				nodeAddress := node.ControlAddress
				if nodeAddress == "" {
					nodeAddress = node.Address
				}
				if endpoint, endpointErr := participantEndpoint(nodeAddress); endpointErr == nil {
					relayURL = endpoint
				} else {
					p.recordState("transport.error", endpointErr.Error())
					return
				}
			}
		}
		connection, _, err := websocket.Dial(p.ctx, relayURL, &websocket.DialOptions{Subprotocols: []string{"pie-relay.ticket." + minted.Token}})
		if err != nil {
			p.recordState("transport.reconnecting", err.Error())
			if !sleepContext(p.ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		connection.SetReadLimit(16 << 20)
		join, _ := json.Marshal(map[string]any{"type": "relay_join", "protocolVersion": "2.0", "streamId": "chat", "clientId": "pie-chat-gateway"})
		if err := connection.Write(p.ctx, websocket.MessageText, join); err != nil {
			_ = connection.CloseNow()
			continue
		}
		p.recordState("transport.connected", "")
		backoff = time.Second
		err = p.connected(connection, session.RelayNodeID, session.RelayGeneration)
		_ = connection.CloseNow()
		if p.ctx.Err() != nil {
			return
		}
		p.recordState("transport.reconnecting", errorMessage(err))
		if !sleepContext(p.ctx, backoff) {
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (p *peer) connected(connection *websocket.Conn, relayNodeID string, relayGeneration int64) error {
	type incoming struct {
		data []byte
		err  error
	}
	read := make(chan incoming)
	go func() {
		defer close(read)
		for {
			_, data, err := connection.Read(p.ctx)
			select {
			case read <- incoming{data: data, err: err}:
			case <-p.ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	hostOnline := false
	sentChatID := ""
	sendActiveChat := func() error {
		p.mu.Lock()
		requestID := p.activeChat
		payload := append(json.RawMessage(nil), p.activePayload...)
		leased := p.turnLease
		p.mu.Unlock()
		if requestID == "" || !leased || requestID == sentChatID || len(payload) == 0 {
			return nil
		}
		if err := connection.Write(p.ctx, websocket.MessageText, payload); err != nil {
			return err
		}
		sentChatID = requestID
		return nil
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	assignment := time.NewTicker(2 * time.Second)
	defer assignment.Stop()
	for {
		var outgoing <-chan json.RawMessage
		if hostOnline {
			outgoing = p.outgoing
		}
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-ping.C:
			pingCtx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
			err := connection.Ping(pingCtx)
			cancel()
			if err != nil {
				return err
			}
		case <-assignment.C:
			conversation, ok := p.gateway.control.Conversation(p.convID)
			if !ok {
				return errors.New("control conversation is unavailable")
			}
			session, ok := p.gateway.control.Session(conversation.SessionID)
			if !ok {
				return errors.New("control session is unavailable")
			}
			if session.RelayNodeID != relayNodeID || session.RelayGeneration != relayGeneration {
				return errors.New("Relay assignment changed")
			}
		case value, ok := <-read:
			if !ok {
				return errors.New("relay connection closed")
			}
			if value.err != nil {
				return value.err
			}
			var envelope struct {
				Type            string `json:"type"`
				Connected       bool   `json:"connected"`
				SessionID       string `json:"sessionId"`
				RequestID       string `json:"requestId"`
				TaskID          string `json:"taskId"`
				ParentToolUseID string `json:"parentToolUseId"`
			}
			_ = json.Unmarshal(value.data, &envelope)
			if envelope.Type == "host:status" || envelope.Type == "agent_status" {
				hostOnline = envelope.Connected
				if hostOnline {
					p.setConversationStatus("ready", "")
					if err := sendActiveChat(); err != nil {
						return err
					}
				} else {
					// A Docker runtime replacement can keep the participant
					// WebSocket alive while changing the host behind it. The new
					// host has not seen the pending request, so allow the durable
					// request to be sent again when host:status becomes connected.
					// clientd deduplicates the same requestId and replays buffered
					// frames when the original host merely reconnected.
					sentChatID = ""
					p.setConversationStatus("reconnecting", "clientd is offline")
				}
			}
			if envelope.Type == "session_id" && envelope.SessionID != "" {
				p.setAgentSessionID(envelope.SessionID)
			}
			requestID := ""
			// Long-running background subagents can emit progress/completion after
			// the main turn's done event has released activeChat. The executor keeps
			// the originating requestId on those events. Trust it only when it names
			// an already accepted request in this conversation; arbitrary client
			// supplied IDs must never alter journal attribution.
			if envelope.RequestID != "" && len(envelope.RequestID) <= 160 {
				if accepted, _ := p.gateway.journal.HasRequest(p.convID, envelope.RequestID); accepted {
					requestID = envelope.RequestID
				}
			}
			// Prefer the task's previously established origin over the currently
			// active turn. An old background task may continue while a newer user
			// message is active, and attributing it to that newer request would mix
			// two independent transcripts. This also protects rolling upgrades from
			// older executors that accidentally expose the SDK's internal req_ ID.
			requestID = p.correlateTaskRequest(
				envelope.Type,
				envelope.TaskID,
				envelope.ParentToolUseID,
				requestID,
			)
			if requestID == "" {
				requestID = p.activeRequestID()
				requestID = p.correlateTaskRequest(
					envelope.Type,
					envelope.TaskID,
					envelope.ParentToolUseID,
					requestID,
				)
			}
			persisted := p.recordRaw(envelope.Type, requestID, value.data)
			if persisted && envelope.Type == "usage" {
				p.recordUsage(requestID, value.data)
			}
			if envelope.Type == "done" || envelope.Type == "error" || envelope.Type == "aborted" {
				if p.completeActive(envelope.Type) {
					sentChatID = ""
				}
			}
		case data := <-outgoing:
			if err := connection.Write(p.ctx, websocket.MessageText, data); err != nil {
				return err
			}
		case <-p.chatWake:
			if hostOnline {
				if err := sendActiveChat(); err != nil {
					return err
				}
			}
		}
	}
}

func (p *peer) activeRequestID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activeChat
}

const maxRememberedTaskRequestKeys = 2048

func (p *peer) correlateTaskRequest(eventType, taskID, parentToolUseID, candidate string) string {
	if !strings.HasPrefix(eventType, "task_") && !strings.HasPrefix(eventType, "subagent_") {
		return candidate
	}
	keys := make([]string, 0, 2)
	if taskID != "" && len(taskID) <= 256 {
		keys = append(keys, "task:"+taskID)
	}
	if parentToolUseID != "" && len(parentToolUseID) <= 256 {
		keys = append(keys, "parent:"+parentToolUseID)
	}
	if len(keys) == 0 {
		return candidate
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, key := range keys {
		if requestID := p.taskRequests[key]; requestID != "" {
			return requestID
		}
	}
	if candidate == "" {
		return ""
	}
	if p.taskRequests == nil {
		p.taskRequests = make(map[string]string)
	}
	for _, key := range keys {
		if len(p.taskRequests) >= maxRememberedTaskRequestKeys {
			break
		}
		p.taskRequests[key] = candidate
	}
	return candidate
}

func (p *peer) recordUsage(requestID string, data json.RawMessage) {
	if requestID == "" {
		log.Printf("chat_usage_ignored conversation_id=%s reason=no_active_request", p.convID)
		return
	}
	p.gateway.mu.Lock()
	recorder := p.gateway.usage
	p.gateway.mu.Unlock()
	if recorder == nil {
		return
	}
	conversation, ok := p.gateway.control.Conversation(p.convID)
	if !ok {
		log.Printf("chat_usage_record_failed conversation_id=%s request_id=%s error=conversation_not_found", p.convID, requestID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := recorder.RecordUsage(ctx, conversation, requestID, append(json.RawMessage(nil), data...)); err != nil {
		// The event is already fsync'd in the chat journal. A Manager restart or
		// reconciliation can replay it safely because the usage ledger deduplicates
		// by conversation/result/model.
		log.Printf("chat_usage_record_failed conversation_id=%s request_id=%s error=%v", p.convID, requestID, err)
	}
}

func (p *peer) completeActive(terminalType string) bool {
	p.mu.Lock()
	if p.activeChat == "" {
		p.mu.Unlock()
		return false
	}
	requestID := p.activeChat
	data, _ := json.Marshal(map[string]string{"terminalType": terminalType})
	if _, err := p.gateway.journal.Append(context.Background(), p.convID, "request.completed", requestID, data); err != nil {
		p.mu.Unlock()
		return false
	}
	p.activeChat = ""
	p.activePayload = nil
	hadLease := p.turnLease
	p.turnLease = false
	p.mu.Unlock()
	if hadLease {
		p.gateway.releaseTurn(p.convID)
	}
	p.gateway.finished.Add(1)
	if terminalType == "error" || terminalType == "aborted" {
		p.gateway.failed.Add(1)
	}
	log.Printf("chat_request_finished request_id=%s conversation_id=%s terminal_type=%s", requestID, p.convID, terminalType)
	p.gateway.touchActivity(p.convID, true)
	p.gateway.notify(p.convID)
	return true
}

func (p *peer) abandonTurn() {
	p.mu.Lock()
	hadTurn := p.activeChat != ""
	p.activeChat = ""
	p.activePayload = nil
	p.turnLease = false
	p.mu.Unlock()
	if hadTurn {
		p.gateway.releaseTurn(p.convID)
	}
}

func (p *peer) setAgentSessionID(value string) {
	p.mu.Lock()
	p.agentSessionID = value
	p.mu.Unlock()
	for attempt := 0; attempt < 3; attempt++ {
		conversation, ok := p.gateway.control.Conversation(p.convID)
		if !ok || conversation.AgentSessionID == value {
			return
		}
		conversation.AgentSessionID = value
		if _, err := p.gateway.control.PutConversation(context.Background(), conversation, conversation.Version, control.MutationMeta{ActorUserID: "chat-gateway", SkipAudit: true}); err == nil {
			return
		} else if !errors.Is(err, control.ErrConflict) {
			return
		}
	}
}

func (p *peer) setConversationStatus(status, lastError string) {
	for attempt := 0; attempt < 3; attempt++ {
		conversation, ok := p.gateway.control.Conversation(p.convID)
		if !ok || conversation.Status == "closing" || conversation.Status == "closed" || conversation.Status == "deleted" {
			return
		}
		if conversation.Status == status && conversation.LastError == lastError {
			return
		}
		conversation.Status, conversation.LastError = status, lastError
		if _, err := p.gateway.control.PutConversation(context.Background(), conversation, conversation.Version, control.MutationMeta{ActorUserID: "chat-gateway", SkipAudit: true}); err == nil {
			return
		} else if !errors.Is(err, control.ErrConflict) {
			return
		}
	}
}

func (p *peer) recordRaw(eventType, requestID string, data []byte) bool {
	if eventType == "" {
		eventType = "relay.event"
	}
	durable := durableExecutorPayload(eventType, data)
	if event, err := p.gateway.journal.Append(context.Background(), p.convID, eventType, requestID, durable); err == nil {
		// The synchronous workspace HTTP caller needs the full in-memory result,
		// while the durable journal intentionally retains metadata only. Source
		// text must not consume the bounded chat journal or broaden its exposure.
		delivery := event
		delivery.Data = append(json.RawMessage(nil), data...)
		p.gateway.deliverRequest(delivery)
		p.gateway.touchActivity(p.convID, false)
		p.gateway.notify(p.convID)
		return true
	}
	return false
}

func durableControlPayload(messageType string, data json.RawMessage) json.RawMessage {
	if messageType != "workspace" {
		return data
	}
	var request struct {
		Type      string `json:"type"`
		RequestID string `json:"requestId"`
		Operation string `json:"operation"`
		Path      string `json:"path"`
	}
	if json.Unmarshal(data, &request) != nil {
		return json.RawMessage(`{"type":"workspace"}`)
	}
	encoded, _ := json.Marshal(request)
	return encoded
}

func durableExecutorPayload(eventType string, data json.RawMessage) json.RawMessage {
	if eventType != "workspace_result" {
		return data
	}
	var result map[string]any
	if json.Unmarshal(data, &result) != nil {
		return json.RawMessage(`{"type":"workspace_result","ok":false}`)
	}
	if value, ok := result["data"].(map[string]any); ok {
		delete(value, "content")
		result["data"] = value
	}
	encoded, _ := json.Marshal(result)
	return encoded
}
func (p *peer) recordState(eventType, message string) {
	data, _ := json.Marshal(map[string]string{"message": message})
	p.recordRaw(eventType, "", data)
}

func (g *Gateway) touchActivity(conversationID string, force bool) {
	now := time.Now().UTC()
	for attempt := 0; attempt < 3; attempt++ {
		conversation, ok := g.control.Conversation(conversationID)
		if !ok || conversation.Status == "closing" || conversation.Status == "closed" || conversation.Status == "deleted" {
			return
		}
		if !force && !conversation.LastActivityAt.IsZero() && now.Sub(conversation.LastActivityAt) < 30*time.Second {
			return
		}
		conversation.LastActivityAt = now
		if _, err := g.control.PutConversation(context.Background(), conversation, conversation.Version, control.MutationMeta{ActorUserID: "chat-gateway", SkipAudit: true}); err == nil {
			return
		} else if !errors.Is(err, control.ErrConflict) {
			return
		}
	}
}

func participantEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", errors.New("chat gateway Relay URL is invalid")
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", errors.New("chat gateway Relay URL must use HTTP(S) or WS(S)")
	}
	parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = "/ws/participant", "", "", ""
	return parsed.String(), nil
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func errorMessage(err error) string {
	if err == nil {
		return "relay connection closed"
	}
	return fmt.Sprintf("%v", err)
}
