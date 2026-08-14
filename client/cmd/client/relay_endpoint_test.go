package main

import "testing"

func TestResolveRelayEndpointDefaultsToCookAI(t *testing.T) {
	got, err := resolveRelayEndpoint("")
	if err != nil {
		t.Fatal(err)
	}
	const want = "wss://relay.cookai.dev/ws/agent"
	if got != want {
		t.Fatalf("unexpected default Relay: got %q want %q", got, want)
	}
}

func TestResolveRelayEndpointUsesConfiguredLocalURL(t *testing.T) {
	got, err := resolveRelayEndpoint("http://127.0.0.1:13412")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ws://127.0.0.1:13412/ws/agent" {
		t.Fatalf("unexpected local Relay: %q", got)
	}
}

func TestRelayEnvironmentURLPrefersPublicSetting(t *testing.T) {
	t.Setenv("PIE_RELAY_URL", "https://pie.example")
	if got := relayEnvironmentURL(); got != "https://pie.example" {
		t.Fatalf("unexpected environment URL: %q", got)
	}
}
