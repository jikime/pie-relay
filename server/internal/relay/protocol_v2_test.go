package relay

import (
	"encoding/json"
	"testing"
)

func TestRelayJoinValidationAndAck(t *testing.T) {
	join, ok := parseRelayJoin([]byte(`{"type":"relay_join","protocolVersion":"2.0","streamId":"terminal/main","clientId":"ios"}`))
	if !ok || !validRelayJoin(join) {
		t.Fatalf("valid v2 join rejected: %#v ok=%t", join, ok)
	}

	var ack relayJoinAck
	if err := json.Unmarshal(makeRelayJoinAck(join, "room-a", "conn-a", "user-a", RoleParticipant, AccessControl, 7), &ack); err != nil {
		t.Fatal(err)
	}
	if ack.ProtocolVersion != relayProtocolVersion || ack.Epoch != 7 || ack.SenderID != "user-a" || ack.StreamID != "terminal/main" {
		t.Fatalf("unexpected ack: %#v", ack)
	}
	scoped := makeRelayJoinAckScoped(join, Identity{Room: "room-a", DeviceID: "device-a", SessionID: "session-a", ExecutionTarget: "docker"}, "conn-a", "user-a", RoleParticipant, AccessControl, 8)
	if err := json.Unmarshal(scoped, &ack); err != nil || ack.ExecutionTarget != "docker" {
		t.Fatalf("runtime metadata missing from ack: %#v err=%v", ack, err)
	}
}

func TestRelayJoinRejectsUnknownVersionAndUnsafeStream(t *testing.T) {
	for _, raw := range []string{
		`{"type":"relay_join","protocolVersion":"1.0","streamId":"room"}`,
		`{"type":"relay_join","protocolVersion":"2.0","streamId":"../ bad"}`,
		`{"type":"relay_join","protocolVersion":"2.0","streamId":""}`,
	} {
		join, ok := parseRelayJoin([]byte(raw))
		if !ok || validRelayJoin(join) {
			t.Fatalf("invalid join accepted: %s", raw)
		}
	}
}

func TestRegistryHostEpochNeverReuses(t *testing.T) {
	r := NewRegistry()
	first, offFirst := r.RegisterHostWithEpoch("room", &fakeSender{})
	offFirst()
	second, _ := r.RegisterHostWithEpoch("room", &fakeSender{})
	if first != 1 || second != 2 || r.HostEpoch("room") != 2 {
		t.Fatalf("epochs = %d, %d, current=%d", first, second, r.HostEpoch("room"))
	}
}

func TestSealedFrameRemainsOpaqueWhileSenderIsStamped(t *testing.T) {
	raw := []byte(`{"type":"relay_sealed","protocolVersion":"2.0","streamId":"terminal","epoch":2,"seq":9,"sealed":{"algorithm":"AES-256-GCM","keyId":"k1","nonce":"bm9uY2U=","ciphertext":"c2VjcmV0"}}`)
	stamped, ok := injectFrom(raw, "verified-user")
	if !ok {
		t.Fatal("sealed frame rejected")
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(stamped, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["sealed"]) != `{"algorithm":"AES-256-GCM","keyId":"k1","nonce":"bm9uY2U=","ciphertext":"c2VjcmV0"}` {
		t.Fatalf("sealed payload changed: %s", got["sealed"])
	}
}
