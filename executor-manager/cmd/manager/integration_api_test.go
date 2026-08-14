package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/capability"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/chatgateway"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
)

func TestPublicChatEventRedactsInlineImageData(t *testing.T) {
	secretImage := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"
	raw, err := json.Marshal(map[string]any{
		"type": "chat", "prompt": "inspect", "images": []map[string]any{{
			"data": secretImage, "mimeType": "image/png", "name": "pixel.png", "size": 24,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	public := publicChatEvent(chatgateway.Event{Type: "request.accepted", Data: raw})
	if strings.Contains(string(public.Data), secretImage) || strings.Contains(string(public.Data), `"images"`) {
		t.Fatalf("public event leaked inline image: %s", public.Data)
	}
	if !strings.Contains(string(public.Data), `"attachments"`) || !strings.Contains(string(public.Data), `"pixel.png"`) {
		t.Fatalf("public event lost attachment summary: %s", public.Data)
	}
}

func TestPublicChatEventRedactsWorkspaceContents(t *testing.T) {
	accepted := publicChatEvent(chatgateway.Event{Type: "control.accepted", Data: json.RawMessage(`{"type":"workspace","operation":"write","path":"src/app.ts","content":"private source"}`)})
	if strings.Contains(string(accepted.Data), "private source") || !strings.Contains(string(accepted.Data), `"operation":"write"`) {
		t.Fatalf("workspace request was not redacted: %s", accepted.Data)
	}
	result := publicChatEvent(chatgateway.Event{Type: "workspace_result", Data: json.RawMessage(`{"type":"workspace_result","operation":"read","ok":true,"data":{"content":"private source"}}`)})
	if strings.Contains(string(result.Data), "private source") || !strings.Contains(string(result.Data), `"ok":true`) {
		t.Fatalf("workspace result was not redacted: %s", result.Data)
	}
}

func TestIntegrationEventReplayDrainsEveryJournalPage(t *testing.T) {
	ctx := context.Background()
	service, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	journal, err := chatgateway.NewJournal(t.TempDir(), 8<<20, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := chatgateway.New(ctx, service, capability.Issuer{Secret: []byte("01234567890123456789012345678901")}, "ws://127.0.0.1:1/ws/agent", journal)
	if err != nil {
		t.Fatal(err)
	}
	const eventCount = integrationEventReplayBatch*2 + 37
	for range eventCount {
		if _, err := journal.Append(ctx, "conversation-replay", "text", "", json.RawMessage(`{"text":"delta"}`)); err != nil {
			t.Fatal(err)
		}
	}
	recorder := httptest.NewRecorder()
	after := uint64(0)
	if err := sendIntegrationEventBacklog(ctx, gateway, "conversation-replay", &after, recorder, recorder); err != nil {
		t.Fatal(err)
	}
	if after != eventCount {
		t.Fatalf("after=%d want=%d", after, eventCount)
	}
	if count := strings.Count(recorder.Body.String(), "data: "); count != eventCount {
		t.Fatalf("replayed=%d want=%d", count, eventCount)
	}
	if !recorder.Flushed {
		t.Fatal("replayed backlog was not flushed")
	}
	if err := sendIntegrationReplayComplete(recorder, recorder, after); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), `event: replay_complete`) || !strings.Contains(recorder.Body.String(), `"lastSequence":437`) {
		t.Fatalf("missing replay completion marker: %s", recorder.Body.String()[max(0, recorder.Body.Len()-200):])
	}
}

func TestIntegrationConversationReasonDistinguishesLifecycleFailures(t *testing.T) {
	healthy := integrationConversationConnection{
		RelayAvailable: true, RuntimeRunning: true, RuntimeHealthy: true,
		ClientConnected: true, RelayRegistered: true, SessionStatus: "active",
	}
	tests := []struct {
		name         string
		conversation control.Conversation
		connection   integrationConversationConnection
		want         string
	}{
		{name: "connected", conversation: control.Conversation{Status: "ready"}, connection: healthy, want: "connected"},
		{name: "idle", conversation: control.Conversation{Status: "closed", LastError: "idle timeout"}, connection: healthy, want: "idle_timeout"},
		{name: "runtime stopped", conversation: control.Conversation{Status: "connecting"}, connection: integrationConversationConnection{}, want: "runtime_stopped"},
		{name: "runtime unhealthy", conversation: control.Conversation{Status: "connecting"}, connection: integrationConversationConnection{RuntimeRunning: true}, want: "runtime_unhealthy"},
		{name: "session starting", conversation: control.Conversation{Status: "connecting"}, connection: integrationConversationConnection{RuntimeRunning: true, RuntimeHealthy: true, SessionStatus: "starting"}, want: "session_starting"},
		{name: "client offline", conversation: control.Conversation{Status: "connecting"}, connection: integrationConversationConnection{RuntimeRunning: true, RuntimeHealthy: true, SessionStatus: "active"}, want: "client_offline"},
		{name: "relay registration", conversation: control.Conversation{Status: "ready"}, connection: integrationConversationConnection{RuntimeRunning: true, RuntimeHealthy: true, ClientConnected: true, SessionStatus: "active"}, want: "relay_unregistered"},
		{name: "relay unavailable", conversation: control.Conversation{Status: "ready"}, connection: integrationConversationConnection{RuntimeRunning: true, RuntimeHealthy: true, ClientConnected: true, RelayRegistered: true, SessionStatus: "active"}, want: "relay_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := integrationConversationReason(test.conversation, test.connection); got != test.want {
				t.Fatalf("reason=%q want=%q", got, test.want)
			}
		})
	}
}

func TestIntegrationConversationViewUsesFreshRelayHeartbeat(t *testing.T) {
	ctx := t.Context()
	service, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	conversation := createLifecycleConversation(t, service, time.Now().UTC())
	device, _ := service.Device(conversation.DeviceID)
	if _, err := service.HeartbeatDevice(ctx, device.ID, device.OwnerUserID, control.DeviceHeartbeat{
		ObservedState: "online", RuntimeRunning: true, RuntimeHealthy: true,
		ClientConnected: true, RelayRegistered: true, RelayNodeID: "relay-a", ActiveSessions: 1,
	}, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutNode(ctx, control.Node{ID: "relay-a", Kind: "relay", Status: "ready", Address: "https://relay.example", LastHeartbeat: time.Now().UTC()}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	session, _ := service.Session(conversation.SessionID)
	session.RelayNodeID = "relay-a"
	if _, err := service.PutSession(ctx, session, session.Version, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	view := (api{control: service}).integrationConversationView(conversation)
	if view.Connection.Reason != "connected" || !view.Connection.ClientConnected || !view.Connection.RelayAvailable {
		t.Fatalf("unexpected connection view: %+v", view.Connection)
	}
}

func TestActiveIntegrationUserConversationsScopesOwnerAndState(t *testing.T) {
	values := []control.Conversation{
		{ID: "ready-a", IntegrationUserID: "binding-a", Status: "ready"},
		{ID: "connecting-a", IntegrationUserID: "binding-a", Status: "connecting"},
		{ID: "closed-a", IntegrationUserID: "binding-a", Status: "closed"},
		{ID: "deleted-a", IntegrationUserID: "binding-a", Status: "deleted"},
		{ID: "ready-b", IntegrationUserID: "binding-b", Status: "ready"},
	}
	actual := activeIntegrationUserConversations(values, "binding-a")
	if len(actual) != 2 || actual[0].ID != "ready-a" || actual[1].ID != "connecting-a" {
		t.Fatalf("unexpected active conversations: %+v", actual)
	}
}
