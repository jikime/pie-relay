package tui

import (
	"encoding/json"
	"regexp"

	tea "github.com/charmbracelet/bubbletea"
)

// The ws event schema is the contract with the relay + host executor:
// text/thinking/session_id/done/error/aborted (executor.mjs), peer_chat and
// host:status/agent:status (relay server.go), agent:unavailable (relay). Each
// becomes a distinct tea.Msg so the model's Update is a plain switch and the
// event→state transitions are unit-testable without a live socket.
type (
	// SessionIDMsg carries the host session id captured from the first turn; the
	// TUI echoes it back on subsequent chats so everyone joins the same session.
	SessionIDMsg struct{ ID string }
	// TextDeltaMsg is one streamed chunk of the Claude response.
	TextDeltaMsg struct{ Text string }
	// ThinkingDeltaMsg is one streamed chunk of extended-thinking text.
	ThinkingDeltaMsg struct{ Text string }
	// DoneMsg marks the end of a response turn.
	DoneMsg struct{ SessionID string }
	// ErrorMsg is a host/relay-reported error for the current turn.
	ErrorMsg struct{ Message string }
	// AbortedMsg marks a turn interrupted by the host.
	AbortedMsg struct{}
	// PeerChatMsg is another participant's question, fanned out by the relay.
	PeerChatMsg struct {
		From string
		Text string
	}
	// HostStatusMsg reflects host (laptop daemon) presence in the room.
	HostStatusMsg struct{ Connected bool }
	// UnavailableMsg means a chat was sent while no host was connected.
	UnavailableMsg struct{ Reason string }
	// ConnClosedMsg is delivered once when the participant ws closes.
	ConnClosedMsg struct{ Err error }
)

// rawEvent is the flat superset of every top-level field the TUI reads. The
// relay keeps payload bodies opaque, so only these top-level keys matter.
type rawEvent struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	SessionID string `json:"sessionId"`
	From      string `json:"from"`
	Message   string `json:"message"`
	Connected bool   `json:"connected"`
	Reason    string `json:"reason"`
}

// parseServerEvent maps one raw ws JSON line to a tea.Msg, or nil for events
// the chat UI ignores (tool_call/tool_result/ready/session_status/etc). A nil
// return is a no-op in Bubble Tea, so unknown events are silently skipped.
func parseServerEvent(raw []byte) tea.Msg {
	var e rawEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil
	}
	switch e.Type {
	case "session_id":
		return SessionIDMsg{ID: e.SessionID}
	case "text":
		return TextDeltaMsg{Text: e.Text}
	case "thinking":
		return ThinkingDeltaMsg{Text: e.Text}
	case "done":
		return DoneMsg{SessionID: e.SessionID}
	case "error":
		return ErrorMsg{Message: e.Message}
	case "aborted":
		return AbortedMsg{}
	case "peer_chat":
		return PeerChatMsg{From: e.From, Text: e.Text}
	case "host:status", "agent:status":
		return HostStatusMsg{Connected: e.Connected}
	case "agent:unavailable":
		return UnavailableMsg{Reason: e.Reason}
	default:
		return nil
	}
}

// guestSub matches the relay's guest identity shape guest:<name>-<rand4>.
var guestSub = regexp.MustCompile(`^guest:(.+)-[^-]+$`)

// speakerName turns a JWT sub (from) into a friendly display name: a guest sub
// guest:bob-x7k2 shows as "bob"; anything else is shown verbatim.
func speakerName(from string) string {
	if from == "" {
		return "?"
	}
	if m := guestSub.FindStringSubmatch(from); m != nil {
		return m[1]
	}
	return from
}
