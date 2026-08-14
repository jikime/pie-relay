package main

import "testing"

func TestCurrentClientVersionNormalizesBuildValues(t *testing.T) {
	previousVersion, previousCommit, previousDate := clientVersion, clientCommit, clientBuildDate
	t.Cleanup(func() {
		clientVersion, clientCommit, clientBuildDate = previousVersion, previousCommit, previousDate
	})
	clientVersion, clientCommit, clientBuildDate = " v1.2.3 ", " abc123 ", " 2026-08-14T00:00:00Z "

	value := currentClientVersion()
	if value.Name != "pie-client" || value.Version != "v1.2.3" || value.Commit != "abc123" || value.BuildDate != "2026-08-14T00:00:00Z" {
		t.Fatalf("unexpected version info: %#v", value)
	}
	if value.Platform == "" || value.GoVersion == "" {
		t.Fatalf("runtime metadata is missing: %#v", value)
	}
}

func TestCurrentClientVersionUsesSafeDevelopmentFallbacks(t *testing.T) {
	previousVersion, previousCommit, previousDate := clientVersion, clientCommit, clientBuildDate
	t.Cleanup(func() {
		clientVersion, clientCommit, clientBuildDate = previousVersion, previousCommit, previousDate
	})
	clientVersion, clientCommit, clientBuildDate = "", "", ""

	value := currentClientVersion()
	if value.Version != "dev" || value.Commit != "unknown" || value.BuildDate != "unknown" {
		t.Fatalf("unexpected fallback version info: %#v", value)
	}
}
