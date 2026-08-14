package chatagent

import "encoding/json"

// peekMessageType extracts the "type" field from a JSON message for logging
// purposes only — never used to alter forwarding behavior (this remains a
// pure byte pump). Returns "" if the message isn't parseable JSON or has no
// string "type" field.
func peekMessageType(data []byte) string {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ""
	}
	return probe.Type
}
