package main

import (
	"path/filepath"
	"testing"
)

func TestResolvePTYHostPath_ExplicitWins(t *testing.T) {
	got := resolvePTYHostPath("/custom/pty-host.mjs")
	if got != "/custom/pty-host.mjs" {
		t.Fatalf("explicit override should win, got %q", got)
	}
}

func TestResolvePTYHostPath_FallsBackToRelative(t *testing.T) {
	// With no override and no binary-adjacent file present, the cwd-relative
	// path is returned (same discipline as resolveExecutorPath).
	got := resolvePTYHostPath("")
	if got != filepath.FromSlash("node-executor/pty-host.mjs") && filepath.Base(got) != "pty-host.mjs" {
		t.Fatalf("expected a pty-host.mjs path, got %q", got)
	}
}
