package main

import (
	"os"
	"strings"
)

const defaultRelayOrigin = "https://relay.cookai.dev"

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// relayEnvironmentURL exposes the single public Relay switch.
func relayEnvironmentURL() string {
	return strings.TrimSpace(os.Getenv("PIE_RELAY_URL"))
}

func resolveRelayEndpoint(configuredURL string) (string, error) {
	return relayAgentURL(firstNonEmpty(configuredURL, defaultRelayOrigin))
}
