package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sync/atomic"
	"time"
)

const relayProtocolVersion = "2.0"

var streamIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// relayJoin is an optional, backwards-compatible in-band protocol handshake.
// Legacy peers may continue sending application frames immediately. V2 peers
// send this as their first frame and receive relay_join_ack before requesting
// a snapshot. The server consumes the control frame instead of forwarding it
// to the room host.
type relayJoin struct {
	Type            string `json:"type"`
	ProtocolVersion string `json:"protocolVersion"`
	StreamID        string `json:"streamId"`
	ClientID        string `json:"clientId,omitempty"`
	LastEpoch       uint64 `json:"lastEpoch,omitempty"`
	LastSeq         uint64 `json:"lastSeq,omitempty"`
}

type relayJoinAck struct {
	Type            string `json:"type"`
	ProtocolVersion string `json:"protocolVersion"`
	Room            string `json:"room"`
	DeviceID        string `json:"deviceId,omitempty"`
	SessionID       string `json:"sessionId,omitempty"`
	ExecutionTarget string `json:"executionTarget,omitempty"`
	StreamID        string `json:"streamId"`
	ConnectionID    string `json:"connectionId"`
	SenderID        string `json:"senderId"`
	Role            string `json:"role"`
	Access          string `json:"access,omitempty"`
	Epoch           uint64 `json:"epoch"`
	ResumeFromSeq   uint64 `json:"resumeFromSeq,omitempty"`
}

func parseRelayJoin(data []byte) (relayJoin, bool) {
	if peekMessageType(data) != "relay_join" {
		return relayJoin{}, false
	}
	var join relayJoin
	if err := json.Unmarshal(data, &join); err != nil {
		return relayJoin{}, true
	}
	return join, true
}

func validRelayJoin(join relayJoin) bool {
	return join.Type == "relay_join" && join.ProtocolVersion == relayProtocolVersion &&
		streamIDPattern.MatchString(join.StreamID)
}

func makeRelayJoinAck(join relayJoin, room, connectionID, senderID, role, access string, epoch uint64) []byte {
	return makeRelayJoinAckScoped(join, Identity{Room: room}, connectionID, senderID, role, access, epoch)
}

func makeRelayJoinAckScoped(join relayJoin, identity Identity, connectionID, senderID, role, access string, epoch uint64) []byte {
	ack, _ := json.Marshal(relayJoinAck{
		Type:            "relay_join_ack",
		ProtocolVersion: relayProtocolVersion,
		Room:            identity.Room,
		DeviceID:        identity.DeviceID,
		SessionID:       identity.SessionID,
		ExecutionTarget: identity.ExecutionTarget,
		StreamID:        join.StreamID,
		ConnectionID:    connectionID,
		SenderID:        senderID,
		Role:            role,
		Access:          access,
		Epoch:           epoch,
		ResumeFromSeq:   join.LastSeq,
	})
	return ack
}

func relayProtocolError(reason string) []byte {
	msg, _ := json.Marshal(struct {
		Type            string `json:"type"`
		ProtocolVersion string `json:"protocolVersion"`
		Reason          string `json:"reason"`
	}{Type: "relay_protocol_error", ProtocolVersion: relayProtocolVersion, Reason: reason})
	return msg
}

func newConnectionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	// Connection/presence IDs are not credentials, but they still must not
	// collapse to one shared map key if the OS random source is unavailable.
	return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), fallbackIDCounter.Add(1))
}

var fallbackIDCounter atomic.Uint64
