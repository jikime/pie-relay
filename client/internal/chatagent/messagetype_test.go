package chatagent

import "testing"

func TestPeekMessageType_Chat(t *testing.T) {
	got := peekMessageType([]byte(`{"type":"chat","prompt":"hello"}`))
	if got != "chat" {
		t.Fatalf("got %q, want %q", got, "chat")
	}
}

func TestPeekMessageType_Done(t *testing.T) {
	got := peekMessageType([]byte(`{"type":"done"}`))
	if got != "done" {
		t.Fatalf("got %q, want %q", got, "done")
	}
}

func TestPeekMessageType_Error(t *testing.T) {
	got := peekMessageType([]byte(`{"type":"error","message":"boom"}`))
	if got != "error" {
		t.Fatalf("got %q, want %q", got, "error")
	}
}

func TestPeekMessageType_InvalidJSON(t *testing.T) {
	got := peekMessageType([]byte(`not json`))
	if got != "" {
		t.Fatalf("got %q, want empty string for invalid JSON", got)
	}
}

func TestPeekMessageType_NoTypeField(t *testing.T) {
	got := peekMessageType([]byte(`{"prompt":"hello"}`))
	if got != "" {
		t.Fatalf("got %q, want empty string when type field is missing", got)
	}
}

func TestPeekMessageType_NonStringType(t *testing.T) {
	got := peekMessageType([]byte(`{"type":123}`))
	if got != "" {
		t.Fatalf("got %q, want empty string for non-string type field", got)
	}
}

func TestPeekMessageType_Nil(t *testing.T) {
	got := peekMessageType(nil)
	if got != "" {
		t.Fatalf("got %q, want empty string for nil input", got)
	}
}

func TestPeekMessageType_Empty(t *testing.T) {
	got := peekMessageType([]byte{})
	if got != "" {
		t.Fatalf("got %q, want empty string for empty input", got)
	}
}
