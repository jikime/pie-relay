package relay

import "testing"

func TestPeekMessageType_ValidChat(t *testing.T) {
	got := peekMessageType([]byte(`{"type":"chat","prompt":"hello"}`))
	if got != "chat" {
		t.Fatalf("got %q, want %q", got, "chat")
	}
}

func TestPeekMessageType_ValidDone(t *testing.T) {
	got := peekMessageType([]byte(`{"type":"done"}`))
	if got != "done" {
		t.Fatalf("got %q, want %q", got, "done")
	}
}

func TestPeekMessageType_InvalidJSON(t *testing.T) {
	got := peekMessageType([]byte(`not json`))
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestPeekMessageType_NoTypeField(t *testing.T) {
	got := peekMessageType([]byte(`{"foo":"bar"}`))
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestPeekMessageType_NonStringType(t *testing.T) {
	got := peekMessageType([]byte(`{"type":123}`))
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestPeekMessageType_Nil(t *testing.T) {
	got := peekMessageType(nil)
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestPeekMessageType_Empty(t *testing.T) {
	got := peekMessageType([]byte{})
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

// TestPeekMessageType_TypeNotFirstKey guards the fallback path: the fast
// case assumes "type" is the first key (true for every message shape in this
// protocol today), but correctness must not depend on it — if "type" appears
// later, peekMessageType must still find it.
func TestPeekMessageType_TypeNotFirstKey(t *testing.T) {
	got := peekMessageType([]byte(`{"prompt":"hello","nested":{"a":1},"type":"chat"}`))
	if got != "chat" {
		t.Fatalf("got %q, want %q", got, "chat")
	}
}
