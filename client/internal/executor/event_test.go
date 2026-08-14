package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseEvent(t *testing.T) {
	cases := []struct {
		line string
		want Event
	}{
		{`{"type":"ready"}`, Event{Type: "ready"}},
		{`{"type":"session_id","sessionId":"s1"}`, Event{Type: "session_id", SessionID: "s1"}},
		{`{"type":"text","text":"hi"}`, Event{Type: "text", Text: "hi"}},
		{`{"type":"thinking","text":"hmm"}`, Event{Type: "thinking", Text: "hmm"}},
		{`{"type":"subagent_text","taskId":"task-a","parentToolUseId":"tool-a","text":"조사 중"}`, Event{Type: "subagent_text", Text: "조사 중"}},
		{`{"type":"tool_call","name":"Write","input":{"a":1}}`, Event{Type: "tool_call", ToolName: "Write"}},
		{`{"type":"permission_request","requestId":"r1","toolName":"Bash","input":{}}`, Event{Type: "permission_request", RequestID: "r1", ToolName: "Bash"}},
		{`{"type":"done","sessionId":"s1"}`, Event{Type: "done", SessionID: "s1"}},
		{`{"type":"error","message":"boom"}`, Event{Type: "error", Message: "boom"}},
	}
	for _, c := range cases {
		ev, ok := ParseEvent([]byte(c.line))
		if !ok {
			t.Fatalf("ParseEvent(%q) ok=false", c.line)
		}
		if ev.Type != c.want.Type || ev.Text != c.want.Text || ev.SessionID != c.want.SessionID ||
			ev.ToolName != c.want.ToolName || ev.RequestID != c.want.RequestID || ev.Message != c.want.Message {
			t.Errorf("ParseEvent(%q) = %+v, want %+v", c.line, ev, c.want)
		}
	}
	ev, _ := ParseEvent([]byte(`{"type":"tool_call","name":"Write","input":{"a":1}}`))
	var in map[string]any
	if err := json.Unmarshal(ev.Input, &in); err != nil || in["a"] != float64(1) {
		t.Errorf("input not preserved: %s", ev.Input)
	}
	// Raw preserves the original (trimmed) line for lossless forwarding.
	rawLine := `{"type":"tool_call","name":"Write","input":{"a":1}}`
	rev, _ := ParseEvent([]byte("  " + rawLine + "  "))
	if string(rev.Raw) != rawLine {
		t.Errorf("Raw = %q, want %q", rev.Raw, rawLine)
	}
	for _, bad := range []string{``, `   `, `garbage`, `{"no":"type"}`} {
		if _, ok := ParseEvent([]byte(bad)); ok {
			t.Errorf("expected ok=false for %q", bad)
		}
	}
}

func TestStartWithOptionsPassesSessionWorkingDirectory(t *testing.T) {
	script := filepath.Join(t.TempDir(), "working-dir.mjs")
	if err := os.WriteFile(script, []byte(`process.stdout.write(JSON.stringify({type:"text",text:process.env.CLI_RELAY_DEFAULT_CWD||""})+"\n")`), 0600); err != nil {
		t.Fatal(err)
	}
	executor, err := StartWithOptions(context.Background(), "node", script, StartOptions{WorkingDir: "/workspace/projects/project-a"})
	if err != nil {
		t.Fatal(err)
	}
	event, ok := <-executor.Events()
	if !ok || event.Text != "/workspace/projects/project-a" {
		t.Fatalf("event=%+v ok=%t", event, ok)
	}
	if err := executor.Close(); err != nil {
		t.Fatal(err)
	}
}
