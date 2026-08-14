package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPPresenceReporterRetriesAndAuthenticates(t *testing.T) {
	var calls atomic.Int32
	received := make(chan PresenceEvent, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/control/relay/presence" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer control-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if calls.Add(1) == 1 {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		var event PresenceEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			http.Error(w, "bad", 400)
			return
		}
		received <- event
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	reporter, err := NewHTTPPresenceReporter(server.URL, "control-token", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !reporter.Report(PresenceEvent{EventID: "event-a", DeviceID: "device-a", SessionID: "session-a", ConnectionID: "connection-a", Kind: "host", Connected: true, At: time.Now()}) {
		t.Fatal("event rejected")
	}
	reporter.Close()
	select {
	case event := <-received:
		if event.DeviceID != "device-a" || event.SessionID != "session-a" {
			t.Fatalf("event=%+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("presence not delivered")
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

type channelPresence struct{ events chan PresenceEvent }

func (c channelPresence) Report(event PresenceEvent) bool {
	c.events <- event
	return true
}

func TestPresenceHeartbeatReplaysLiveConnection(t *testing.T) {
	sink := channelPresence{events: make(chan PresenceEvent, 1)}
	server := NewServerOpts(ServerOptions{NodeID: "cell-a", PublicURL: "https://relay.cookai.dev", Presence: sink})
	id := Identity{UserID: "owner", Room: "room", DeviceID: "device-a", SessionID: "session-a"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.presenceHeartbeat(ctx, id, "connection-a", "host", 5*time.Millisecond, func() (bool, bool) { return true, true })
	select {
	case event := <-sink.events:
		if !event.Connected || !event.HostOnline || event.ConnectionID != "connection-a" {
			t.Fatalf("event=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat missing")
	}
}

type capturePresence struct{ events []PresenceEvent }

func (c *capturePresence) Report(event PresenceEvent) bool {
	c.events = append(c.events, event)
	return true
}

func TestServerPresenceUsesVerifiedScope(t *testing.T) {
	sink := &capturePresence{}
	server := NewServerOpts(ServerOptions{NodeID: "cell-a", PublicURL: "https://relay.cookai.dev", Presence: sink})
	id := Identity{UserID: "owner", Room: "room", DeviceID: "device-a", SessionID: "session-a", RelayGeneration: 3, Role: RoleHost, Access: AccessControl}
	server.reportPresence(id, "connection-a", "host", true, true)
	if len(sink.events) != 1 {
		t.Fatal("presence missing")
	}
	event := sink.events[0]
	if event.NodeID != "cell-a" || event.PublicURL != "https://relay.cookai.dev" || event.DeviceID != "device-a" || event.SessionID != "session-a" || event.RelayGeneration != 3 || event.UserID != "owner" {
		t.Fatalf("event=%+v", event)
	}
}
