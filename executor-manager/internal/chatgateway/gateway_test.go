package chatgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/capability"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
)

func TestGatewayRelaysChatPersistsEventsAndAgentSession(t *testing.T) {
	requestSeen := make(chan map[string]any, 1)
	usageSeen := make(chan recordedUsage, 1)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/participant" {
			http.NotFound(w, r)
			return
		}
		offered := strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",")
		protocol := strings.TrimSpace(offered[0])
		connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}, OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, _, err := connection.Read(ctx); err != nil { // relay_join
			return
		}
		status, _ := json.Marshal(map[string]any{"type": "host:status", "connected": true})
		if err := connection.Write(ctx, websocket.MessageText, status); err != nil {
			return
		}
		_, raw, err := connection.Read(ctx)
		if err != nil {
			return
		}
		var message map[string]any
		if json.Unmarshal(raw, &message) != nil {
			return
		}
		requestSeen <- message
		for _, event := range []map[string]any{
			{"type": "session_id", "sessionId": "claude-session-a"},
			{"type": "text", "text": "hello from executor"},
			{"type": "task_started", "requestId": "message-a", "taskId": "task-a", "parentToolUseId": "tool-a"},
			{"type": "usage", "schemaVersion": 1, "resultId": "result-a", "queryRunId": "run-a", "sessionId": "claude-session-a", "modelUsage": map[string]any{"claude-test": map[string]any{"inputTokens": 10, "outputTokens": 2, "costUSD": 0.001}}},
			{"type": "done"},
			// The main turn is already complete here. A background subagent must
			// still retain the request that originally launched it, even if an old
			// executor sends the Claude SDK's internal request ID by mistake.
			{"type": "subagent_text", "requestId": "req-sdk-internal", "taskId": "task-a", "parentToolUseId": "tool-a", "text": "background update"},
		} {
			encoded, _ := json.Marshal(event)
			if err := connection.Write(ctx, websocket.MessageText, encoded); err != nil {
				return
			}
		}
		<-ctx.Done()
	}))
	defer relay.Close()

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controlService, err := control.NewService(rootCtx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer controlService.Close()
	conversation := seedGatewayConversation(t, controlService)
	journal, err := NewJournal(t.TempDir(), 1<<20, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := New(rootCtx, controlService, capability.Issuer{Secret: []byte("01234567890123456789012345678901")}, relay.URL, journal)
	if err != nil {
		t.Fatal(err)
	}
	gateway.SetUsageRecorder(usageRecorderFunc(func(_ context.Context, conversation control.Conversation, requestID string, raw json.RawMessage) error {
		usageSeen <- recordedUsage{conversation: conversation, requestID: requestID, raw: raw}
		return nil
	}))
	image := ImageAttachment{Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=", MIMEType: "image/png", Name: "pixel.png"}
	accepted, duplicate, err := gateway.SendChatWithImages(conversation, "message-a", "hello", []ImageAttachment{image})
	if err != nil || duplicate || accepted.Type != "request.accepted" {
		t.Fatalf("accepted=%+v duplicate=%t err=%v", accepted, duplicate, err)
	}
	if _, duplicate, err := gateway.SendChat(conversation, "message-a", "hello"); err != nil || !duplicate {
		t.Fatalf("duplicate=%t err=%v", duplicate, err)
	}
	select {
	case request := <-requestSeen:
		if request["type"] != "chat" || request["prompt"] != "hello" {
			t.Fatalf("relay request=%+v", request)
		}
		images, ok := request["images"].([]any)
		if !ok || len(images) != 1 || images[0].(map[string]any)["mimeType"] != "image/png" || images[0].(map[string]any)["data"] != image.Data || images[0].(map[string]any)["size"] != float64(68) {
			t.Fatalf("relay images=%+v", request["images"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chat request did not reach relay")
	}
	select {
	case usage := <-usageSeen:
		if usage.conversation.ID != conversation.ID || usage.requestID != "message-a" || !strings.Contains(string(usage.raw), `"resultId":"result-a"`) {
			t.Fatalf("usage=%+v raw=%s", usage, usage.raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("usage event was not attributed")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, eventErr := gateway.Events(context.Background(), conversation.ID, 0, 100)
		if eventErr != nil {
			t.Fatal(eventErr)
		}
		seenDone := false
		seenSubagent := false
		for _, event := range events {
			seenDone = seenDone || event.Type == "done"
			seenSubagent = seenSubagent || (event.Type == "subagent_text" && event.RequestID == "message-a")
		}
		updated, _ := controlService.Conversation(conversation.ID)
		if seenDone && seenSubagent && updated.AgentSessionID == "claude-session-a" && updated.Status == "ready" {
			stats := gateway.Stats()
			if stats.Started == 1 && stats.Finished == 1 && stats.Failed == 0 && stats.ActiveTurns == 0 {
				gateway.Close(conversation.ID)
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gateway did not persist executor events and session state")
}

func TestGatewayRequestWaitsForMatchingWorkspaceResult(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offered := strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",")
		protocol := strings.TrimSpace(offered[0])
		connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}, OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, _, err := connection.Read(ctx); err != nil {
			return
		}
		status, _ := json.Marshal(map[string]any{"type": "host:status", "connected": true})
		if err := connection.Write(ctx, websocket.MessageText, status); err != nil {
			return
		}
		_, raw, err := connection.Read(ctx)
		if err != nil {
			return
		}
		var request map[string]any
		if json.Unmarshal(raw, &request) != nil || request["type"] != "workspace" || request["operation"] != "read" {
			return
		}
		result, _ := json.Marshal(map[string]any{
			"type": "workspace_result", "requestId": request["requestId"], "operation": "read", "ok": true,
			"data": map[string]any{"path": "README.md", "content": "hello"},
		})
		_ = connection.Write(ctx, websocket.MessageText, result)
		<-ctx.Done()
	}))
	defer relay.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	conversation := seedGatewayConversation(t, service)
	journal, err := NewJournal(t.TempDir(), 1<<20, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := New(ctx, service, capability.Issuer{Secret: []byte("01234567890123456789012345678901")}, relay.URL, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(conversation.ID)
	requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
	defer requestCancel()
	event, duplicate, err := gateway.Request(requestCtx, conversation, "workspace-a", map[string]any{
		"type": "workspace", "requestId": "workspace-a", "operation": "read", "path": "README.md",
	}, "workspace_result")
	if err != nil || duplicate || event.Type != "workspace_result" || event.RequestID != "workspace-a" || !strings.Contains(string(event.Data), `"content":"hello"`) {
		t.Fatalf("event=%+v duplicate=%t err=%v", event, duplicate, err)
	}
	persisted, err := journal.Events(requestCtx, conversation.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	encodedPersisted, _ := json.Marshal(persisted)
	if strings.Contains(string(encodedPersisted), "hello") {
		t.Fatalf("workspace source content leaked into durable journal: %s", encodedPersisted)
	}
	// A retry does not execute the write/control request twice; it recovers the
	// original durable response by idempotency key.
	recovered, duplicate, err := gateway.Request(requestCtx, conversation, "workspace-a", map[string]any{
		"type": "workspace", "requestId": "workspace-a", "operation": "read", "path": "README.md",
	}, "workspace_result")
	if err != nil || !duplicate || recovered.Sequence != event.Sequence || strings.Contains(string(recovered.Data), `"content"`) {
		t.Fatalf("recovered=%+v duplicate=%t err=%v", recovered, duplicate, err)
	}
}

type recordedUsage struct {
	conversation control.Conversation
	requestID    string
	raw          json.RawMessage
}

type usageRecorderFunc func(context.Context, control.Conversation, string, json.RawMessage) error

func (f usageRecorderFunc) RecordUsage(ctx context.Context, conversation control.Conversation, requestID string, raw json.RawMessage) error {
	return f(ctx, conversation, requestID, raw)
}

func TestValidateImageAttachmentsRejectsSpoofingAndBounds(t *testing.T) {
	valid := ImageAttachment{Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=", MIMEType: "image/png", Name: "pixel.png", Size: 68}
	if err := validateImageAttachments([]ImageAttachment{valid}); err != nil {
		t.Fatalf("valid image: %v", err)
	}
	for name, image := range map[string]ImageAttachment{
		"bad base64":   {Data: "%%%", MIMEType: "image/png"},
		"spoofed MIME": {Data: "aGVsbG8=", MIMEType: "image/png"},
		"bad filename": {Data: valid.Data, MIMEType: valid.MIMEType, Name: "../pixel.png"},
		"wrong size":   {Data: valid.Data, MIMEType: valid.MIMEType, Size: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateImageAttachments([]ImageAttachment{image}); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	tooMany := make([]ImageAttachment, MaxChatImageCount+1)
	if err := validateImageAttachments(tooMany); err == nil {
		t.Fatal("expected image count rejection")
	}
}

func TestGatewayAllowsOnlyOneActiveChatTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	conversation := seedGatewayConversation(t, service)
	journal, err := NewJournal(t.TempDir(), 1<<20, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := New(ctx, service, capability.Issuer{Secret: []byte("01234567890123456789012345678901")}, "http://127.0.0.1:1", journal)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(conversation.ID)
	if _, _, err := gateway.SendChat(conversation, "first", "one"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gateway.SendChat(conversation, "second", "two"); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("second turn err=%v", err)
	}
	if _, duplicate, err := gateway.SendChat(conversation, "first", "one"); err != nil || !duplicate {
		t.Fatalf("first retry duplicate=%t err=%v", duplicate, err)
	}
}

func TestSubscriptionTurnLimiterPromotesFIFO(t *testing.T) {
	gateway := &Gateway{
		peers: map[string]*peer{}, subs: map[string]map[chan struct{}]struct{}{},
		turnLimit: 1, turnActive: map[string]struct{}{},
	}
	first := &peer{gateway: gateway, convID: "conversation-a", activeChat: "request-a", chatWake: make(chan struct{}, 1)}
	second := &peer{gateway: gateway, convID: "conversation-b", activeChat: "request-b", chatWake: make(chan struct{}, 1)}
	gateway.peers[first.convID], gateway.peers[second.convID] = first, second
	first.turnLease = gateway.claimTurn(first.convID)
	second.turnLease = gateway.claimTurn(second.convID)
	if !first.turnLease || second.turnLease {
		t.Fatalf("initial leases first=%t second=%t", first.turnLease, second.turnLease)
	}
	stats := gateway.Stats()
	if stats.ActiveTurns != 1 || stats.QueuedTurns != 1 {
		t.Fatalf("initial stats=%+v", stats)
	}
	gateway.releaseTurn(first.convID)
	select {
	case <-second.chatWake:
	case <-time.After(time.Second):
		t.Fatal("queued turn was not promoted")
	}
	second.mu.Lock()
	leased := second.turnLease
	second.mu.Unlock()
	stats = gateway.Stats()
	if !leased || stats.ActiveTurns != 1 || stats.QueuedTurns != 0 {
		t.Fatalf("promoted lease=%t stats=%+v", leased, stats)
	}
}

func TestSubscriptionTurnLimiterRemovesStoppedQueuedPeer(t *testing.T) {
	gateway := &Gateway{
		peers: map[string]*peer{}, subs: map[string]map[chan struct{}]struct{}{},
		turnLimit: 1, turnActive: map[string]struct{}{},
	}
	first := &peer{gateway: gateway, convID: "conversation-a", activeChat: "request-a", chatWake: make(chan struct{}, 1)}
	second := &peer{gateway: gateway, convID: "conversation-b", activeChat: "request-b", chatWake: make(chan struct{}, 1)}
	gateway.peers[first.convID], gateway.peers[second.convID] = first, second
	first.turnLease = gateway.claimTurn(first.convID)
	second.turnLease = gateway.claimTurn(second.convID)

	gateway.releaseTurn(second.convID)
	stats := gateway.Stats()
	if stats.ActiveTurns != 1 || stats.QueuedTurns != 0 {
		t.Fatalf("stopped queued peer leaked a limiter slot: %+v", stats)
	}
	select {
	case <-second.chatWake:
		t.Fatal("stopped queued peer must not be promoted")
	default:
	}
}

func TestGatewayReplaysDurablyAcceptedRequestAfterRestart(t *testing.T) {
	requestSeen := make(chan map[string]any, 1)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offered := strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",")
		protocol := strings.TrimSpace(offered[0])
		connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}, OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, _, err := connection.Read(ctx); err != nil {
			return
		}
		status, _ := json.Marshal(map[string]any{"type": "host:status", "connected": true})
		if connection.Write(ctx, websocket.MessageText, status) != nil {
			return
		}
		_, raw, err := connection.Read(ctx)
		if err != nil {
			return
		}
		var message map[string]any
		if json.Unmarshal(raw, &message) != nil {
			return
		}
		requestSeen <- message
		done, _ := json.Marshal(map[string]string{"type": "done"})
		_ = connection.Write(ctx, websocket.MessageText, done)
		<-ctx.Done()
	}))
	defer relay.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	conversation := seedGatewayConversation(t, service)
	journal, err := NewJournal(t.TempDir(), 1<<20, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"type":"chat","prompt":"recover","requestId":"request-recover"}`)
	if _, err := journal.Append(ctx, conversation.ID, "request.accepted", "request-recover", payload); err != nil {
		t.Fatal(err)
	}
	// Constructing a fresh Gateway models a Manager process restart: its only
	// source of the accepted-but-unsent request is the durable journal.
	gateway, err := New(ctx, service, capability.Issuer{Secret: []byte("01234567890123456789012345678901")}, relay.URL, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(conversation.ID)
	if err := gateway.Ensure(conversation); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requestSeen:
		if request["requestId"] != "request-recover" || request["prompt"] != "recover" {
			t.Fatalf("replayed request=%+v", request)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("durably accepted request was not replayed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, pending, err := journal.PendingChatRequest(conversation.ID); err != nil {
			t.Fatal(err)
		} else if !pending {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("replayed request was not durably completed")
}

func TestGatewayResendsActiveRequestAfterRelayHostReplacement(t *testing.T) {
	requests := make(chan string, 2)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offered := strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",")
		protocol := strings.TrimSpace(offered[0])
		connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol}, OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, _, err := connection.Read(ctx); err != nil { // relay_join
			return
		}
		writeStatus := func(connected bool) bool {
			status, _ := json.Marshal(map[string]any{"type": "host:status", "connected": connected})
			return connection.Write(ctx, websocket.MessageText, status) == nil
		}
		readRequest := func() bool {
			_, raw, err := connection.Read(ctx)
			if err != nil {
				return false
			}
			var message struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(raw, &message) != nil {
				return false
			}
			requests <- message.RequestID
			return true
		}
		if !writeStatus(true) || !readRequest() {
			return
		}
		// The participant connection stays open, but Relay reports that its
		// host was replaced. The pending request must reach the new host too.
		if !writeStatus(false) || !writeStatus(true) || !readRequest() {
			return
		}
		done, _ := json.Marshal(map[string]string{"type": "done"})
		_ = connection.Write(ctx, websocket.MessageText, done)
		<-ctx.Done()
	}))
	defer relay.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	conversation := seedGatewayConversation(t, service)
	journal, err := NewJournal(t.TempDir(), 1<<20, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := New(ctx, service, capability.Issuer{Secret: []byte("01234567890123456789012345678901")}, relay.URL, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(conversation.ID)
	if _, duplicate, err := gateway.SendChat(conversation, "request-replace", "continue safely"); err != nil || duplicate {
		t.Fatalf("duplicate=%t err=%v", duplicate, err)
	}
	for index := 0; index < 2; index++ {
		select {
		case requestID := <-requests:
			if requestID != "request-replace" {
				t.Fatalf("request %d id=%q", index+1, requestID)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("request %d was not delivered", index+1)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, pending, err := journal.PendingChatRequest(conversation.ID); err != nil {
			t.Fatal(err)
		} else if !pending {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("replacement host request was not completed")
}

func seedGatewayConversation(t *testing.T, service *control.Service) control.Conversation {
	t.Helper()
	ctx := context.Background()
	integration, err := service.PutIntegration(ctx, control.Integration{ID: "partner-a", DisplayName: "Partner A", Credential: control.CredentialProfile{TargetPath: ".partner/credential.json"}}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	user, err := service.PutUser(ctx, control.User{ID: "owner-a", Status: "active"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := service.PutIntegrationUser(ctx, control.IntegrationUser{ID: "binding-a", IntegrationID: integration.ID, ExternalUserID: "external-a", OwnerUserID: user.ID, Status: "ready"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	device, err := service.RegisterDevice(ctx, control.Device{ID: "executor-owner-a", OwnerUserID: user.ID, Kind: "docker", Name: "Executor"}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.PutProject(ctx, control.Project{ID: "project-a", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: user.ID, Name: "Project A", Locale: "ko", Status: "ready"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.PutSession(ctx, control.Session{ID: "chat-session-a", OwnerUserID: user.ID, DeviceID: device.ID, ProjectID: project.ID, ExecutionTarget: "docker", AgentMode: "chat", AccessMode: "private", TransportMode: "relay", Status: "ready"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := service.PutConversation(ctx, control.Conversation{ID: "conversation-a", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: user.ID, DeviceID: device.ID, ProjectID: project.ID, SessionID: session.ID, Status: "connecting"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}
