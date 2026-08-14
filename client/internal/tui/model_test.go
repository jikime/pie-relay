package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// update drives one message through the model and returns the concrete Model.
func update(m Model, msg tea.Msg) Model {
	next, _ := m.Update(msg)
	return next.(Model)
}

// sized returns a model that has received a window size, so refresh() renders
// into a real viewport (exercising the layout path too).
func sized(myName string, send SendFunc) Model {
	m := New(myName, send)
	return update(m, tea.WindowSizeMsg{Width: 80, Height: 24})
}

func TestParseServerEvent(t *testing.T) {
	cases := []struct {
		raw  string
		want tea.Msg
	}{
		{`{"type":"session_id","sessionId":"s-1"}`, SessionIDMsg{ID: "s-1"}},
		{`{"type":"text","text":"hi"}`, TextDeltaMsg{Text: "hi"}},
		{`{"type":"thinking","text":"hmm"}`, ThinkingDeltaMsg{Text: "hmm"}},
		{`{"type":"done","sessionId":"s-2"}`, DoneMsg{SessionID: "s-2"}},
		{`{"type":"error","message":"boom"}`, ErrorMsg{Message: "boom"}},
		{`{"type":"aborted"}`, AbortedMsg{}},
		{`{"type":"peer_chat","from":"guest:bob-x7k2","text":"q?"}`, PeerChatMsg{From: "guest:bob-x7k2", Text: "q?"}},
		{`{"type":"host:status","connected":true}`, HostStatusMsg{Connected: true}},
		{`{"type":"agent:status","connected":false}`, HostStatusMsg{Connected: false}},
		{`{"type":"agent:unavailable","reason":"no-agent-connected"}`, UnavailableMsg{Reason: "no-agent-connected"}},
	}
	for _, c := range cases {
		if got := parseServerEvent([]byte(c.raw)); got != c.want {
			t.Errorf("parseServerEvent(%s) = %#v, want %#v", c.raw, got, c.want)
		}
	}
	// Ignored / unparseable events return nil (no-op in Bubble Tea).
	for _, raw := range []string{`{"type":"tool_call","name":"Read"}`, `{"type":"ready"}`, `not json`} {
		if got := parseServerEvent([]byte(raw)); got != nil {
			t.Errorf("parseServerEvent(%s) = %#v, want nil", raw, got)
		}
	}
}

func TestStreamingAccumulatesAndFinalizesOnDone(t *testing.T) {
	m := sized("나", nil)
	m = update(m, TextDeltaMsg{Text: "안녕"})
	m = update(m, TextDeltaMsg{Text: "하세요"})
	if !m.responding {
		t.Fatal("expected responding=true mid-stream")
	}
	if m.streaming != "안녕하세요" {
		t.Fatalf("streaming = %q, want 안녕하세요", m.streaming)
	}
	if len(m.msgs) != 0 {
		t.Fatalf("expected no finalized msgs mid-stream, got %d", len(m.msgs))
	}

	m = update(m, DoneMsg{SessionID: "s-9"})
	if m.responding {
		t.Fatal("expected responding=false after done")
	}
	if m.streaming != "" {
		t.Fatalf("streaming should be cleared, got %q", m.streaming)
	}
	if m.sessionID != "s-9" {
		t.Fatalf("sessionID = %q, want s-9", m.sessionID)
	}
	if len(m.msgs) != 1 || m.msgs[0].kind != spkClaude || m.msgs[0].text != "안녕하세요" {
		t.Fatalf("expected one finalized Claude msg, got %+v", m.msgs)
	}
}

func TestSessionIDCaptured(t *testing.T) {
	m := sized("나", nil)
	m = update(m, SessionIDMsg{ID: "sess-abc"})
	if m.sessionID != "sess-abc" {
		t.Fatalf("sessionID = %q, want sess-abc", m.sessionID)
	}
	// An empty id must not clobber a captured one.
	m = update(m, SessionIDMsg{ID: ""})
	if m.sessionID != "sess-abc" {
		t.Fatalf("empty SessionIDMsg clobbered sessionID: %q", m.sessionID)
	}
}

func TestHostStatusTransitions(t *testing.T) {
	m := sized("나", nil)
	if m.hostUp {
		t.Fatal("host should start disconnected")
	}
	m = update(m, HostStatusMsg{Connected: true})
	if !m.hostUp {
		t.Fatal("expected hostUp=true")
	}
	if !strings.Contains(m.header(), "호스트 연결") {
		t.Errorf("header should show connected: %q", m.header())
	}
	m = update(m, HostStatusMsg{Connected: false})
	if m.hostUp {
		t.Fatal("expected hostUp=false")
	}
	if !strings.Contains(m.header(), "호스트 끊김") {
		t.Errorf("header should show disconnected: %q", m.header())
	}
}

func TestPeerChatShowsFriendlyName(t *testing.T) {
	m := sized("나", nil)
	m = update(m, PeerChatMsg{From: "guest:bob-x7k2", Text: "이 함수 설명해줘"})
	if len(m.msgs) != 1 || m.msgs[0].kind != spkPeer {
		t.Fatalf("expected one peer msg, got %+v", m.msgs)
	}
	if m.msgs[0].name != "bob" {
		t.Fatalf("peer name = %q, want bob", m.msgs[0].name)
	}
}

func TestErrorFinalizesStreamAndAppends(t *testing.T) {
	m := sized("나", nil)
	m = update(m, TextDeltaMsg{Text: "부분 응답"})
	m = update(m, ErrorMsg{Message: "rate limit"})
	if m.responding {
		t.Fatal("responding should be false after error")
	}
	// Partial stream flushed, then the error line appended.
	if len(m.msgs) != 2 {
		t.Fatalf("expected partial + error msgs, got %+v", m.msgs)
	}
	if m.msgs[0].kind != spkClaude || m.msgs[1].kind != spkError {
		t.Fatalf("unexpected kinds: %+v", m.msgs)
	}
}

func TestAbortedAppendsSystemNote(t *testing.T) {
	m := sized("나", nil)
	m = update(m, TextDeltaMsg{Text: "진행 중"})
	m = update(m, AbortedMsg{})
	if m.responding {
		t.Fatal("responding should be false after abort")
	}
	last := m.msgs[len(m.msgs)-1]
	if last.kind != spkSystem {
		t.Fatalf("expected system note last, got %+v", last)
	}
}

func TestEnterSendsChatAndEchoesLocally(t *testing.T) {
	var sent []byte
	m := sized("alice", func(payload []byte) error {
		sent = append([]byte(nil), payload...)
		return nil
	})
	m = update(m, SessionIDMsg{ID: "s-42"})

	// Type into the input, then press Enter.
	m = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)

	// My message echoed locally under my name; a turn is now live.
	if len(m.msgs) != 1 || m.msgs[0].kind != spkMe || m.msgs[0].name != "alice" || m.msgs[0].text != "hello" {
		t.Fatalf("expected local echo of my message, got %+v", m.msgs)
	}
	if !m.responding {
		t.Fatal("expected responding=true after sending")
	}
	if m.input.Value() != "" {
		t.Fatalf("input should be reset, got %q", m.input.Value())
	}

	// Running the returned command performs the ws write.
	if cmd == nil {
		t.Fatal("expected a send command")
	}
	cmd()
	var obj map[string]string
	if err := json.Unmarshal(sent, &obj); err != nil {
		t.Fatalf("sent payload not JSON: %v (%s)", err, sent)
	}
	if obj["type"] != "chat" || obj["prompt"] != "hello" || obj["sessionId"] != "s-42" {
		t.Fatalf("unexpected chat payload: %v", obj)
	}
	// Design policy: the client MUST NOT set `from` — the relay injects it.
	if _, ok := obj["from"]; ok {
		t.Fatalf("client must not set from; payload had it: %v", obj)
	}
}

func TestEnterOnEmptyInputDoesNothing(t *testing.T) {
	called := false
	m := sized("나", func([]byte) error { called = true; return nil })
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		if msg := cmd(); msg != nil {
			t.Fatalf("empty-input Enter should not send, got msg %#v", msg)
		}
	}
	if called {
		t.Fatal("send must not be called for empty input")
	}
}

func TestBuildChatOmitsSessionWhenEmpty(t *testing.T) {
	var obj map[string]string
	if err := json.Unmarshal(buildChat("hi", ""), &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["sessionId"]; ok {
		t.Fatalf("sessionId should be omitted when empty: %v", obj)
	}
}
