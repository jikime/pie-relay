package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRuntimeReadinessMetadataContainsNoCommandOutput(t *testing.T) {
	runner := func(_ context.Context, command string, args ...string) ([]byte, error) {
		if len(args) == 1 && args[0] == "--version" {
			return []byte("safe-version\n"), nil
		}
		return []byte("secret-token-must-not-leave-device"), nil
	}
	loader := func(_ context.Context, _ string) ([]aiRuntimeModel, error) {
		return []aiRuntimeModel{{ID: "model-a", DisplayName: "Model A", IsDefault: true}}, nil
	}
	// probeCLIRuntime uses LookPath before the injectable runner. Use the Go
	// executable as a harmless installed command for this pure serialization test.
	value := probeCLIRuntime(context.Background(), "go", []string{"auth", "status"}, runner, loader)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || value.Status != "READY" || !value.Authenticated {
		t.Fatalf("readiness=%+v", value)
	}
	if containsText(string(encoded), "secret-token") {
		t.Fatalf("authentication command output leaked into metadata: %s", encoded)
	}
}

func TestRuntimeReadinessClassifiesMissingAndLoginRequired(t *testing.T) {
	missing := probeCLIRuntime(context.Background(), "pie-command-that-does-not-exist", nil, runRuntimeCommand, nil)
	if missing.Status != "NOT_INSTALLED" || missing.Installed {
		t.Fatalf("missing=%+v", missing)
	}

	runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) == 1 && args[0] == "--version" {
			return []byte("1.2.3"), nil
		}
		return []byte("login required"), errors.New("exit status 1")
	}
	required := probeCLIRuntime(context.Background(), "go", []string{"login", "status"}, runner, nil)
	if required.Status != "LOGIN_REQUIRED" || !required.Installed || required.Authenticated {
		t.Fatalf("login required=%+v", required)
	}

	transientRunner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) == 1 && args[0] == "--version" {
			return []byte("1.2.3"), nil
		}
		return []byte("temporary keychain failure"), errors.New("exit status 1")
	}
	transient := probeCLIRuntime(context.Background(), "go", []string{"auth", "status"}, transientRunner, nil)
	if transient.Status != "ERROR" || !transient.Installed || transient.Authenticated {
		t.Fatalf("transient failure=%+v", transient)
	}
}

func TestRuntimeReadinessRetriesFailedProbeSooner(t *testing.T) {
	ready := aiRuntimeSnapshot{
		ClaudeCode: aiRuntimeReadiness{Status: "READY"},
		Codex:      aiRuntimeReadiness{Status: "NOT_INSTALLED"},
	}
	if got := runtimeReadinessTTL(ready); got != runtimeProbeTTL {
		t.Fatalf("ready ttl=%s", got)
	}
	failed := ready
	failed.ClaudeCode.Status = "LOGIN_REQUIRED"
	if got := runtimeReadinessTTL(failed); got != runtimeRetryProbeTTL {
		t.Fatalf("failed ttl=%s", got)
	}
}

func TestRuntimeReadinessMonitorCachesProbe(t *testing.T) {
	monitor := &runtimeReadinessMonitor{
		snapshot:  aiRuntimeSnapshot{CheckedAt: time.Now().UTC(), ClaudeCode: aiRuntimeReadiness{Status: "READY"}},
		checkedAt: time.Now(),
		run: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			t.Fatal("cached readiness unexpectedly ran a command")
			return nil, nil
		},
	}
	metadata := monitor.metadata(context.Background(), false)
	if !containsText(metadata[aiRuntimeMetadataKey], `"status":"READY"`) {
		t.Fatalf("metadata=%v", metadata)
	}
}

func TestLoadCodexModelsUsesAppServerProtocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-only")
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "codex")
	script := `#!/bin/sh
read initialize
printf '%s\n' '{"id":1,"result":{"userAgent":"fixture","codexHome":"/tmp","platformFamily":"unix","platformOs":"test"}}'
read initialized
read models
printf '%s\n' '{"id":2,"result":{"data":[{"id":"gpt-test","model":"gpt-test","displayName":"GPT Test","hidden":false,"isDefault":true},{"id":"hidden","model":"hidden","displayName":"Hidden","hidden":true,"isDefault":false}],"nextCursor":null}}'
`
	if err := os.WriteFile(fixture, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// Race instrumentation and a loaded CI host can make even this tiny shell
	// fixture take longer than one second to start. Keep the test timeout well
	// below the production probe timeout while avoiding scheduler-dependent
	// failures during the parallel release gate.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	models, err := loadCodexModels(ctx, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "gpt-test" || !models[0].IsDefault {
		t.Fatalf("models=%+v", models)
	}
}

func TestLoadCodexModelsFromInstalledCLI(t *testing.T) {
	if os.Getenv("PIE_TEST_REAL_CODEX") != "1" {
		t.Skip("set PIE_TEST_REAL_CODEX=1 to test the installed Codex account")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	models, err := loadCodexModels(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) == 0 {
		t.Fatal("installed Codex returned no visible models")
	}
}

func containsText(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}
