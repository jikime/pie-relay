// Package executor supervises the Node chat executor (node-executor/executor.mjs)
// over stdio and exposes its NDJSON events as typed Go values.
package executor

import (
	"bytes"
	"encoding/json"
)

// Event is one parsed stdout event from the Node executor.
type Event struct {
	Type      string          // ready|session_id|text|thinking|tool_call|tool_result|permission_request|done|error
	Text      string          // text/thinking
	SessionID string          // session_id/done
	ToolName  string          // tool_call (name) / permission_request (toolName)
	Input     json.RawMessage // tool_call/permission_request
	RequestID string          // permission_request
	Message   string          // error
	Raw       json.RawMessage // original NDJSON line (for lossless relay forwarding)
}

// ParseEvent decodes one NDJSON line into an Event. ok=false for blank,
// invalid, or type-less lines.
func ParseEvent(line []byte) (Event, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return Event{}, false
	}
	var raw struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		SessionID string          `json:"sessionId"`
		ToolName  string          `json:"toolName"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		RequestID string          `json:"requestId"`
		Message   string          `json:"message"`
	}
	if err := json.Unmarshal(line, &raw); err != nil || raw.Type == "" {
		return Event{}, false
	}
	tool := raw.ToolName
	if tool == "" {
		tool = raw.Name
	}
	return Event{
		Type: raw.Type, Text: raw.Text, SessionID: raw.SessionID,
		ToolName: tool, Input: raw.Input, RequestID: raw.RequestID, Message: raw.Message,
		Raw: append(json.RawMessage(nil), line...),
	}, true
}
