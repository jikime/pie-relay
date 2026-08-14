package claudeauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/credential"
)

const testSetupToken = "sk-ant-oat-test-subscription-token-000000000000000000000001"

func TestPublishResolveRolloutAndRollback(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	store, err := credential.New(stateRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	service, err := Open(filepath.Join(root, "auth"), filepath.Join(root, "login"), store, true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 1, 2, 3, 4, time.UTC)
	service.Now = func() time.Time { return now }

	writeSetupToken(t, service.LoginDirectory(), testSetupToken)
	v1, err := service.PublishFromLogin(context.Background(), "primary")
	if err != nil {
		t.Fatal(err)
	}
	if v1.Mode != authMode || v1.RotateAfter.Sub(v1.CreatedAt) != 330*24*time.Hour || v1.ExpiresAt.Sub(v1.CreatedAt) != 365*24*time.Hour {
		t.Fatalf("unexpected version metadata: %+v", v1)
	}
	if _, err := os.Stat(filepath.Join(service.LoginDirectory(), setupTokenCandidate)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("one-shot candidate remains: %v", err)
	}
	resolved, version, err := service.CurrentOAuthToken(context.Background())
	if err != nil || resolved != testSetupToken || version != v1.ID {
		t.Fatalf("resolved=%q version=%q err=%v", resolved, version, err)
	}
	if _, err := store.Write("user-a", legacyCredentialTarget, []byte(`{"claudeAiOauth":{"refreshToken":"legacy"}}`), 4096); err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureUser(context.Background(), "user-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "user-a", legacyCredentialTarget)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy per-user credential remains after OAuth activation: %v", err)
	}

	rollout, err := service.Rollout(context.Background(), []string{"user-b", "user-a", "user-a"}, 2)
	if err != nil || rollout.Total != 2 || rollout.Succeeded != 2 || rollout.Version != v1.ID {
		t.Fatalf("unexpected rollout: %+v err=%v", rollout, err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "user-a", ".claude", ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subscription token was persisted into executor HOME: %v", err)
	}
	status, err := service.Status([]string{"user-a", "user-b"})
	if err != nil || !status.Available || status.Expired || status.RotationDue || status.Targets.Current != 2 || status.Targets.Missing != 0 {
		t.Fatalf("unexpected status: %+v err=%v", status, err)
	}
	encoded, _ := json.Marshal(status)
	if strings.Contains(string(encoded), testSetupToken) {
		t.Fatalf("status leaked OAuth token: %s", encoded)
	}

	now = now.Add(time.Second)
	writeSetupToken(t, service.LoginDirectory(), testSetupToken+"-rotated")
	v2, err := service.PublishFromLogin(context.Background(), "rotated")
	if err != nil || v2.ID == v1.ID {
		t.Fatalf("rotated version=%+v err=%v", v2, err)
	}
	rolledBack, err := service.Rollback(context.Background())
	if err != nil || rolledBack.ID != v1.ID {
		t.Fatalf("rollback=%+v err=%v", rolledBack, err)
	}
	resolved, _, err = service.CurrentOAuthToken(context.Background())
	if err != nil || resolved != testSetupToken {
		t.Fatalf("rollback token=%q err=%v", resolved, err)
	}
}

func TestExpiredSubscriptionTokenFailsClosedAndRemainsVisibleToAdmin(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, _ := credential.New(filepath.Join(root, "state"), "")
	service, err := Open(filepath.Join(root, "auth"), "", store, false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	writeSetupToken(t, service.LoginDirectory(), testSetupToken)
	if _, err := service.PublishFromLogin(context.Background(), "expiring"); err != nil {
		t.Fatal(err)
	}

	now = now.Add(331 * 24 * time.Hour)
	status, err := service.Status([]string{"user-a"})
	if err != nil || !status.Available || status.Expired || !status.RotationDue {
		t.Fatalf("rotation-due status=%+v err=%v", status, err)
	}

	now = now.Add(35 * 24 * time.Hour)
	status, err = service.Status([]string{"user-a"})
	if err != nil || status.Available || !status.Configured || !status.Expired || status.Targets.Failed != 1 || status.Deployments[0].LastError == "" {
		t.Fatalf("expired status=%+v err=%v", status, err)
	}
	if _, _, err := service.CurrentOAuthToken(context.Background()); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired CurrentOAuthToken error=%v", err)
	}
	if err := service.EnsureUser(context.Background(), "user-a"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired EnsureUser error=%v", err)
	}
	if _, err := service.Rollout(context.Background(), []string{"user-a"}, 1); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired Rollout error=%v", err)
	}
}

func TestEncryptedVersionDoesNotContainPlaintextAndDetectsTampering(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, _ := credential.New(filepath.Join(root, "state"), "")
	service, err := Open(filepath.Join(root, "auth"), "", store, true)
	if err != nil {
		t.Fatal(err)
	}
	writeSetupToken(t, service.LoginDirectory(), testSetupToken)
	version, err := service.PublishFromLogin(context.Background(), "encrypted")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(service.Root, "versions", version.ID, encryptedTokenFile)
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), testSetupToken) {
		t.Fatal("encrypted version contains plaintext token")
	}
	payload[len(payload)/2] ^= 1
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.CurrentOAuthToken(context.Background()); err == nil {
		t.Fatal("tampered token envelope was accepted")
	}
}

func TestPublishDeduplicatesAndValidatesCandidate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, _ := credential.New(filepath.Join(root, "state"), "")
	service, err := Open(filepath.Join(root, "auth"), "", store, false)
	if err != nil {
		t.Fatal(err)
	}
	writeSetupToken(t, service.LoginDirectory(), testSetupToken)
	first, err := service.PublishFromLogin(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	writeSetupToken(t, service.LoginDirectory(), testSetupToken)
	second, err := service.PublishFromLogin(context.Background(), "two")
	if err != nil || second.ID != first.ID {
		t.Fatalf("dedup=%+v err=%v want=%s", second, err, first.ID)
	}
	writeSetupToken(t, service.LoginDirectory(), testSetupToken)
	if err := os.Chmod(filepath.Join(service.LoginDirectory(), setupTokenCandidate), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PublishFromLogin(context.Background(), "unsafe"); err == nil {
		t.Fatal("expected permissive token rejection")
	}
	writeSetupToken(t, service.LoginDirectory(), "short")
	if _, err := service.PublishFromLogin(context.Background(), "short"); err == nil {
		t.Fatal("expected short token rejection")
	}
	writeSetupToken(t, service.LoginDirectory(), testSetupToken+" with-space")
	if _, err := service.PublishFromLogin(context.Background(), "whitespace"); err == nil {
		t.Fatal("expected whitespace rejection")
	}
}

func TestLegacyCredentialMigrationPublishesWithoutUnsafeRollbackTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, _ := credential.New(filepath.Join(root, "state"), "")
	service, err := Open(filepath.Join(root, "auth"), "", store, true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 3, 4, 5, 0, time.UTC)
	service.Now = func() time.Time { return now }
	legacyID := "v-20260810T063531.467761930Z-9b72738e3de6"
	legacyDir := filepath.Join(service.Root, "versions", legacyID)
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	legacy := Version{
		ID: legacyID, Label: "legacy", Fingerprint: strings.Repeat("a", 64),
		Bytes: 509, CreatedAt: now.Add(-48 * time.Hour),
	}
	if err := writeJSONAtomic(filepath.Join(legacyDir, "metadata.json"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, legacyVersionTokenFile), []byte(`{"claudeAiOauth":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(service.Root, "active.json"), State{
		ActiveVersion: legacyID, PreviousVersion: "v-legacy-older", UpdatedAt: now.Add(-time.Hour),
	}, 0600); err != nil {
		t.Fatal(err)
	}

	status, err := service.Status([]string{"user-a"})
	if err != nil || status.Configured || status.Available || !status.MigrationPending || status.Targets.Missing != 1 {
		t.Fatalf("legacy status=%+v err=%v", status, err)
	}
	if _, _, err := service.CurrentOAuthToken(context.Background()); !errors.Is(err, ErrMigrationPending) {
		t.Fatalf("legacy CurrentOAuthToken error=%v", err)
	}

	writeSetupToken(t, service.LoginDirectory(), testSetupToken+"-migrated")
	version, err := service.PublishFromLogin(context.Background(), "central broker")
	if err != nil {
		t.Fatal(err)
	}
	status, err = service.Status([]string{"user-a"})
	if err != nil || !status.Available || status.MigrationPending || status.State.ActiveVersion != version.ID {
		t.Fatalf("migrated status=%+v err=%v", status, err)
	}
	if status.State.PreviousVersion != "" {
		t.Fatalf("legacy version became rollback target: %q", status.State.PreviousVersion)
	}
	if _, err := service.Rollback(context.Background()); err == nil {
		t.Fatal("rollback to legacy credential unexpectedly succeeded")
	}
}

func TestOldVersionListIsTrimmed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, _ := credential.New(filepath.Join(root, "state"), "")
	service, err := Open(filepath.Join(root, "auth"), "", store, true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	for index := 0; index <= 21; index++ {
		writeSetupToken(t, service.LoginDirectory(), fmt.Sprintf("%s-%02d", testSetupToken, index))
		if _, err := service.PublishFromLogin(context.Background(), fmt.Sprintf("version-%d", index)); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	status, err := service.Status([]string{"user-a"})
	if err != nil || len(status.Versions) != 20 || status.Targets.Current != 1 {
		t.Fatalf("unexpected status: versions=%d targets=%+v err=%v", len(status.Versions), status.Targets, err)
	}
}

func TestRequiredAndOptionalUnconfigured(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, _ := credential.New(filepath.Join(root, "state"), "")
	required, _ := Open(filepath.Join(root, "required"), "", store, true)
	if err := required.EnsureUser(context.Background(), "user-a"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("required EnsureUser error=%v", err)
	}
	if _, _, err := required.CurrentOAuthToken(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("required CurrentOAuthToken error=%v", err)
	}
	optional, _ := Open(filepath.Join(root, "optional"), "", store, false)
	if err := optional.EnsureUser(context.Background(), "user-a"); err != nil {
		t.Fatalf("optional EnsureUser=%v", err)
	}
	if token, version, err := optional.CurrentOAuthToken(context.Background()); err != nil || token != "" || version != "" {
		t.Fatalf("optional token=%q version=%q err=%v", token, version, err)
	}
}

func writeSetupToken(t *testing.T, directory, token string) {
	t.Helper()
	path := filepath.Join(directory, setupTokenCandidate)
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
}
