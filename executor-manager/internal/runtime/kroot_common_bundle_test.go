package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncKrootCommonBundleInstallsAndUpgradesManagedRoots(t *testing.T) {
	bundleRoot := t.TempDir()
	writeKrootBundleTestFile(t, bundleRoot, ".claude/skills/kroot-a/SKILL.md", "skill-a-v1", 0644)
	writeKrootBundleTestFile(t, bundleRoot, ".claude/skills/kroot-b/scripts/run.sh", "#!/bin/sh\n", 0755)
	writeKrootBundleTestFile(t, bundleRoot, ".claude/agents/kroot/worker.md", "agent-v1", 0644)
	writeKrootBundleTestFile(t, bundleRoot, ".claude/agents/general.md", "general-agent", 0644)
	writeKrootBundleTestFile(t, bundleRoot, ".claude/agents/review/specialist.md", "specialist-agent", 0644)

	stateRoot := t.TempDir()
	writeKrootBundleTestFile(t, stateRoot, ".claude/skills/custom-user/SKILL.md", "custom", 0644)
	writeKrootBundleTestFile(t, stateRoot, ".claude/agents/local-only.md", "local-agent", 0644)
	writeKrootBundleTestFile(t, stateRoot, ".claude/settings.json", `{"theme":"user"}`, 0600)
	writeKrootBundleTestFile(t, stateRoot, ".claude/.credentials.json", `{"oauth":"secret"}`, 0600)

	if err := syncKrootCommonBundle(stateRoot, bundleRoot, "adk-rev-1"); err != nil {
		t.Fatal(err)
	}
	assertKrootBundleTestFile(t, stateRoot, ".claude/skills/kroot-a/SKILL.md", "skill-a-v1")
	assertKrootBundleTestFile(t, stateRoot, ".claude/agents/kroot/worker.md", "agent-v1")
	assertKrootBundleTestFile(t, stateRoot, ".claude/agents/general.md", "general-agent")
	assertKrootBundleTestFile(t, stateRoot, ".claude/agents/review/specialist.md", "specialist-agent")
	if _, err := os.Stat(filepath.Join(stateRoot, ".claude/agents/local-only.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("locally modified managed agents tree was not replaced: %v", err)
	}
	assertKrootBundleTestFile(t, stateRoot, ".claude/skills/custom-user/SKILL.md", "custom")
	assertKrootBundleTestFile(t, stateRoot, ".claude/settings.json", `{"theme":"user"}`)
	assertKrootBundleTestFile(t, stateRoot, ".claude/.credentials.json", `{"oauth":"secret"}`)
	runInfo, err := os.Stat(filepath.Join(stateRoot, ".claude/skills/kroot-b/scripts/run.sh"))
	if err != nil || runInfo.Mode().Perm() != 0555 {
		t.Fatalf("managed executable mode=%v err=%v", runInfo.Mode().Perm(), err)
	}
	if err := os.RemoveAll(filepath.Join(stateRoot, ".claude/skills/kroot-a")); err != nil {
		t.Fatal(err)
	}
	if err := syncKrootCommonBundle(stateRoot, bundleRoot, "adk-rev-1"); err != nil {
		t.Fatal(err)
	}
	assertKrootBundleTestFile(t, stateRoot, ".claude/skills/kroot-a/SKILL.md", "skill-a-v1")

	if err := os.RemoveAll(filepath.Join(bundleRoot, ".claude/skills/kroot-a")); err != nil {
		t.Fatal(err)
	}
	writeKrootBundleTestFile(t, bundleRoot, ".claude/skills/kroot-b/SKILL.md", "skill-b-v2", 0644)
	writeKrootBundleTestFile(t, bundleRoot, ".claude/agents/kroot/worker.md", "agent-v2", 0644)
	if err := syncKrootCommonBundle(stateRoot, bundleRoot, "adk-rev-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, ".claude/skills/kroot-a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale managed skill was not removed: %v", err)
	}
	assertKrootBundleTestFile(t, stateRoot, ".claude/skills/kroot-b/SKILL.md", "skill-b-v2")
	assertKrootBundleTestFile(t, stateRoot, ".claude/agents/kroot/worker.md", "agent-v2")
	assertKrootBundleTestFile(t, stateRoot, ".claude/skills/custom-user/SKILL.md", "custom")

	markerBytes, err := os.ReadFile(filepath.Join(stateRoot, krootCommonMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	var marker krootCommonMarker
	if err := json.Unmarshal(markerBytes, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.BundleVersion != "adk-rev-2" || marker.FileCount != 5 || strings.Contains(strings.Join(marker.ManagedRoots, ","), "kroot-a") || !strings.Contains(strings.Join(marker.ManagedRoots, ","), ".claude/agents") {
		t.Fatalf("marker=%+v", marker)
	}
}

func TestSyncKrootCommonBundleAcceptsAtomicCurrentSymlink(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "releases", "rev-1")
	writeKrootBundleTestFile(t, release, ".claude/skills/kroot-a/SKILL.md", "skill", 0644)
	writeKrootBundleTestFile(t, release, ".claude/agents/kroot/agent.md", "agent", 0644)
	if err := os.Symlink(filepath.Join("releases", "rev-1"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	stateRoot := t.TempDir()
	if err := syncKrootCommonBundle(stateRoot, filepath.Join(root, "current"), "rev-1"); err != nil {
		t.Fatal(err)
	}
	assertKrootBundleTestFile(t, stateRoot, ".claude/skills/kroot-a/SKILL.md", "skill")
}

func TestSyncKrootCommonBundleRejectsSymlinksWithoutTouchingUserState(t *testing.T) {
	bundleRoot := t.TempDir()
	writeKrootBundleTestFile(t, bundleRoot, ".claude/skills/kroot-a/SKILL.md", "skill", 0644)
	writeKrootBundleTestFile(t, bundleRoot, ".claude/agents/kroot/agent.md", "agent", 0644)
	if err := os.Symlink("/etc/passwd", filepath.Join(bundleRoot, ".claude/skills/kroot-a/escape")); err != nil {
		t.Fatal(err)
	}
	stateRoot := t.TempDir()
	writeKrootBundleTestFile(t, stateRoot, ".claude/settings.json", "safe", 0600)
	err := syncKrootCommonBundle(stateRoot, bundleRoot, "rev-bad")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}
	assertKrootBundleTestFile(t, stateRoot, ".claude/settings.json", "safe")
	if _, err := os.Stat(filepath.Join(stateRoot, krootCommonMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker unexpectedly written: %v", err)
	}
}

func TestSyncKrootCommonBundleRejectsSymlinkedManagedTarget(t *testing.T) {
	bundleRoot := t.TempDir()
	writeKrootBundleTestFile(t, bundleRoot, ".claude/skills/kroot-a/SKILL.md", "skill", 0644)
	writeKrootBundleTestFile(t, bundleRoot, ".claude/agents/kroot/agent.md", "agent", 0644)
	stateRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateRoot, ".claude/skills"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp", filepath.Join(stateRoot, ".claude/skills/kroot-a")); err != nil {
		t.Fatal(err)
	}
	err := syncKrootCommonBundle(stateRoot, bundleRoot, "rev-1")
	if err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("err=%v", err)
	}
}

func TestSyncKrootCommonBundleRejectsEmptyAgentsTree(t *testing.T) {
	bundleRoot := t.TempDir()
	writeKrootBundleTestFile(t, bundleRoot, ".claude/skills/kroot-a/SKILL.md", "skill", 0644)
	if err := os.MkdirAll(filepath.Join(bundleRoot, ".claude/agents"), 0700); err != nil {
		t.Fatal(err)
	}
	err := syncKrootCommonBundle(t.TempDir(), bundleRoot, "rev-empty-agents")
	if err == nil || !strings.Contains(err.Error(), "no agent files") {
		t.Fatalf("err=%v", err)
	}
}

func writeKrootBundleTestFile(t *testing.T, root, relative, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func assertKrootBundleTestFile(t *testing.T, root, relative, expected string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil || string(content) != expected {
		t.Fatalf("%s=%q err=%v", relative, content, err)
	}
}
