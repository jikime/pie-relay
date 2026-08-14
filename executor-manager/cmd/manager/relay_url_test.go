package main

import "testing"

func TestResolveRelayAgentURLDefaultsToCookAI(t *testing.T) {
	got, err := resolveRelayAgentURL("")
	if err != nil {
		t.Fatal(err)
	}
	const want = "wss://relay.cookai.dev/ws/agent"
	if got != want {
		t.Fatalf("unexpected default Relay: got %q want %q", got, want)
	}
}

func TestResolveRelayAgentURLUsesLocalValue(t *testing.T) {
	got, err := resolveRelayAgentURL("http://127.0.0.1:13412")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ws://127.0.0.1:13412/ws/agent" {
		t.Fatalf("unexpected local Relay: %q", got)
	}
}

func TestResolveRelayAgentURLRejectsUnsupportedScheme(t *testing.T) {
	if _, err := resolveRelayAgentURL("file:///tmp/relay"); err == nil {
		t.Fatal("expected invalid scheme error")
	}
}
