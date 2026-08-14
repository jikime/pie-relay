package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestControlConnectionRequiresTokenAndClosesTrackedSocket(t *testing.T) {
	server := NewServerOpts(ServerOptions{ControlToken: "control-secret"})
	connection := newFakeWSConn(false)
	sender := newWsSender(context.Background(), connection)
	defer sender.shutdown(1000, "test complete")
	untrack := server.trackConnection("connection-a", sender)
	defer untrack()

	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodDelete, "/v1/control/connections/connection-a", nil))
	if unauthorized.Code != http.StatusUnauthorized || connection.isClosed() {
		t.Fatalf("unauthorized status=%d closed=%t", unauthorized.Code, connection.isClosed())
	}

	request := httptest.NewRequest(http.MethodDelete, "/v1/control/connections/connection-a", nil)
	request.Header.Set("Authorization", "Bearer control-secret")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !connection.isClosed() {
		t.Fatalf("status=%d closed=%t body=%s", response.Code, connection.isClosed(), response.Body.String())
	}
}

func TestControlConnectionDeleteIsIdempotent(t *testing.T) {
	server := NewServerOpts(ServerOptions{ControlToken: "control-secret"})
	request := httptest.NewRequest(http.MethodDelete, "/v1/control/connections/already-gone", nil)
	request.Header.Set("Authorization", "Bearer control-secret")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestControlDriverHandsOffAndRevokes(t *testing.T) {
	server := NewServerOpts(ServerOptions{ControlToken: "control-secret"})
	identity := Identity{Room: "owner", DeviceID: "device-a", SessionID: "session-a", RelayGeneration: 4}
	routingKey := identity.RoutingKey()
	host := &fakeSender{}
	participant := &fakeSender{}
	server.reg.RegisterHost(routingKey, host)
	server.reg.RegisterParticipant(routingKey, participant, Participant{UserID: "viewer", Access: AccessControl})

	type driverResponse struct {
		UserID     string    `json:"userId"`
		Generation uint64    `json:"generation"`
		ExpiresAt  time.Time `json:"expiresAt"`
	}
	call := func(userID string) driverResponse {
		body, _ := json.Marshal(map[string]any{"room": identity.Room, "deviceId": identity.DeviceID, "sessionId": identity.SessionID, "userId": userID, "relayGeneration": identity.RelayGeneration})
		request := httptest.NewRequest(http.MethodPost, "/v1/control/driver", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer control-secret")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("driver status=%d body=%s", response.Code, response.Body.String())
		}
		var result driverResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	assigned := call("viewer")
	if assigned.UserID != "viewer" || assigned.ExpiresAt.Before(time.Now()) {
		t.Fatalf("assigned=%+v", assigned)
	}
	revoked := call("")
	if revoked.UserID != "" || !revoked.ExpiresAt.IsZero() || revoked.Generation <= assigned.Generation {
		t.Fatalf("revoked=%+v", revoked)
	}
}
