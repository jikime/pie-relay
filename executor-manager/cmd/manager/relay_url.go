package main

import (
	"errors"
	"net/url"
	"strings"
)

const defaultRelayOrigin = "https://relay.cookai.dev"

func resolveRelayAgentURL(raw string) (string, error) {
	selected := strings.TrimSpace(raw)
	if selected == "" {
		selected = defaultRelayOrigin
	}
	parsed, err := url.Parse(selected)
	if err != nil || parsed.Host == "" {
		return "", errors.New("PIE_RELAY_URL is invalid")
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", errors.New("PIE_RELAY_URL must use http(s) or ws(s)")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if path == "" {
		path = "/ws/agent"
	} else if !strings.HasSuffix(path, "/ws/agent") {
		path += "/ws/agent"
	}
	parsed.Path, parsed.RawPath = path, ""
	parsed.RawQuery, parsed.Fragment = "", ""
	return parsed.String(), nil
}
