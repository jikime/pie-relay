package main

import (
	"context"
	"errors"
	"testing"

	"cli-relay/client/internal/chatagent"
)

// TestRunConnect_PassesTokenThrough verifies the daemon hands the resolved host
// token (ticket or credentials.json accessToken) straight to the agent leg.
func TestRunConnect_PassesTokenThrough(t *testing.T) {
	var seen string
	var calls int
	run := func(ctx context.Context, relayURL, executorPath, token string) error {
		calls++
		seen = token
		return nil
	}

	if err := runConnect(context.Background(), "ws://relay", "/path/to/executor", "host-token", run); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected run to be called exactly once, got %d", calls)
	}
	if seen != "host-token" {
		t.Fatalf("expected token to be passed through, got %q", seen)
	}
}

// TestRunConnect_SurfacesUnauthorized verifies a rejected token surfaces
// verbatim — there is no refresh/re-auth retry anymore.
func TestRunConnect_SurfacesUnauthorized(t *testing.T) {
	var calls int
	run := func(ctx context.Context, relayURL, executorPath, token string) error {
		calls++
		return chatagent.ErrUnauthorized
	}

	err := runConnect(context.Background(), "ws://relay", "/path/to/executor", "host-token", run)
	if !errors.Is(err, chatagent.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized to pass through, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected run to be called exactly once (no retry), got %d", calls)
	}
}
