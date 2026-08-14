package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

func TestEnsureAppliesIsolationAndResourceLimits(t *testing.T) {
	var calls [][]string
	d := Docker{Image: "executor:test", Prefix: "pie-", PermissionMode: "bypassPermissions", AllowUserNamespaces: true, BlobRoot: t.TempDir(), WorkRoot: t.TempDir(), StateRoot: t.TempDir(), Command: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	}}
	if err := d.Ensure(context.Background(), manager.Executor{UserID: "user-1", ID: "executor-user-1"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls=%d", len(calls))
	}
	joined := strings.Join(calls[0], " ")
	for _, want := range []string{"pie.manager_id=default", "pie.isolation_version=v5", "pie.user_namespaces=true", "pie.kroot_common_home=false", "pie.permission_mode=bypassPermissions", "CLI_RELAY_PERMISSION_MODE=bypassPermissions", "--network bridge", "--user", "--cpus 2", "--memory 2g", "--memory-swap 2g", "--pids-limit 256", "--cap-drop ALL", "no-new-privileges=true", "seccomp=unconfined", "systempaths=unconfined", "--ipc private", "--init", "--ulimit core=0:0", "--read-only", "/tmp:rw,noexec,nosuid,nodev,size=256m,mode=1777", "/home:rw,nosuid,nodev,size=16m,mode=1777", "--log-driver local", "/workspace:rw", "/workspace/input:ro", "/home/executor:rw", "HOME=/home/executor", "--health-cmd", "pie-client sessions request", "pie-client sessions serve", "executor:test"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Join(calls[1], " ") != "start pie-executor-user-1" {
		t.Fatalf("start=%v", calls[1])
	}
}

func TestEnsureEnablesKrootUserCommonModeWhenBundleIsConfigured(t *testing.T) {
	bundleRoot := t.TempDir()
	writeKrootBundleTestFile(t, bundleRoot, ".claude/skills/kroot-a/SKILL.md", "skill", 0644)
	writeKrootBundleTestFile(t, bundleRoot, ".claude/agents/kroot/agent.md", "agent", 0644)
	var calls [][]string
	d := Docker{
		Image: "executor:test", Prefix: "pie-", StateRoot: t.TempDir(),
		KrootCommonBundleRoot: bundleRoot, KrootCommonBundleVersion: "rev-1",
		User: fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()),
		Command: func(_ context.Context, args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			return nil, nil
		},
	}
	if err := d.Ensure(context.Background(), manager.Executor{UserID: "user-common", ID: "executor-user-common"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("docker calls=%v", calls)
	}
	create := strings.Join(calls[0], " ")
	for _, expected := range []string{"pie.kroot_common_home=true", "KROOT_USER_COMMON=true"} {
		if !strings.Contains(create, expected) {
			t.Errorf("missing %q in create command: %s", expected, create)
		}
	}
}

func TestObserveReportsImageAndIsolationPolicyDrift(t *testing.T) {
	response := `{"Id":"container-a","Config":{"Image":"executor:old","Labels":{"pie.isolation_version":"v2","pie.permission_mode":"default","pie.user_namespaces":"false"}},"HostConfig":{"SecurityOpt":["no-new-privileges=true"]},"State":{"Status":"running","Running":true,"Health":{"Status":"healthy"}}}`
	d := Docker{Image: "executor:new", Prefix: "pie-", PermissionMode: "bypassPermissions", AllowUserNamespaces: true, Command: func(context.Context, ...string) ([]byte, error) {
		return []byte(response), nil
	}}
	observation, err := d.Observe(context.Background(), manager.Executor{ID: "executor-user-1"})
	if err != nil || !observation.Running || !observation.Drifted {
		t.Fatalf("drift observation=%+v err=%v", observation, err)
	}

	response = `{"Id":"container-b","Config":{"Image":"executor:new","Labels":{"pie.isolation_version":"v5","pie.permission_mode":"bypassPermissions","pie.user_namespaces":"true","pie.kroot_common_home":"false"}},"HostConfig":{"SecurityOpt":["no-new-privileges=true","seccomp=unconfined"],"MaskedPaths":[],"ReadonlyPaths":[]},"State":{"Status":"running","Running":true,"Health":{"Status":"healthy"}}}`
	observation, err = d.Observe(context.Background(), manager.Executor{ID: "executor-user-1"})
	if err != nil || !observation.Running || observation.Drifted {
		t.Fatalf("converged observation=%+v err=%v", observation, err)
	}

	response = `{"Id":"container-c","Config":{"Image":"executor:new","Labels":{"pie.isolation_version":"v5","pie.permission_mode":"bypassPermissions","pie.user_namespaces":"true","pie.kroot_common_home":"false"}},"HostConfig":{"SecurityOpt":["no-new-privileges=true","seccomp=unconfined"],"MaskedPaths":["/proc/asound"],"ReadonlyPaths":["/proc/sys"]},"State":{"Status":"running","Running":true,"Health":{"Status":"healthy"}}}`
	observation, err = d.Observe(context.Background(), manager.Executor{ID: "executor-user-1"})
	if err != nil || !observation.Drifted {
		t.Fatalf("missing systempaths drift observation=%+v err=%v", observation, err)
	}
}

func TestEnsureAppliesPerUserResourceLimitsAndUpdatesExistingContainer(t *testing.T) {
	var calls [][]string
	d := Docker{Image: "executor:test", Prefix: "pie-", Scope: "manager-a", Command: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "create":
			return nil, fmt.Errorf("already exists")
		case "inspect":
			return []byte("user-1|manager-a|executor:test||v5|false|false\n"), nil
		default:
			return nil, nil
		}
	}}
	executor := manager.Executor{UserID: "user-1", ID: "executor-user-1", CPUs: "1.5", MemoryBytes: 1 << 30, PIDsLimit: 96}
	if err := d.Ensure(context.Background(), executor); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("calls=%v", calls)
	}
	created := strings.Join(calls[0], " ")
	updated := strings.Join(calls[2], " ")
	for _, want := range []string{"--cpus 1.5", "--memory 1073741824", "--memory-swap 1073741824", "--pids-limit 96"} {
		if !strings.Contains(created, want) || !strings.Contains(updated, want) {
			t.Fatalf("resource %q missing: create=%s update=%s", want, created, updated)
		}
	}
	if strings.Join(calls[3], " ") != "start pie-executor-user-1" {
		t.Fatalf("start=%v", calls[3])
	}
}

func TestEnsureRecreatesExistingContainerWhenPermissionPolicyChanges(t *testing.T) {
	var calls [][]string
	createCalls := 0
	d := Docker{Image: "executor:test", Prefix: "pie-", Scope: "manager-a", PermissionMode: "bypassPermissions", Command: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "create":
			createCalls++
			if createCalls == 1 {
				return nil, fmt.Errorf("already exists")
			}
		case "inspect":
			return []byte("user-1|manager-a|executor:test|default|v5|false|false\n"), nil
		}
		return nil, nil
	}}
	if err := d.Ensure(context.Background(), manager.Executor{UserID: "user-1", ID: "executor-user-1"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 5 {
		t.Fatalf("calls=%v", calls)
	}
	if got := strings.Join(calls[2], " "); got != "rm -f pie-executor-user-1" {
		t.Fatalf("remove=%s", got)
	}
	recreated := strings.Join(calls[3], " ")
	for _, want := range []string{"pie.permission_mode=bypassPermissions", "CLI_RELAY_PERMISSION_MODE=bypassPermissions"} {
		if !strings.Contains(recreated, want) {
			t.Fatalf("missing %q in recreate: %s", want, recreated)
		}
	}
	if got := strings.Join(calls[4], " "); got != "start pie-executor-user-1" {
		t.Fatalf("start=%s", got)
	}
}

func TestEnsureRecreatesOwnedContainerWhenImageChanges(t *testing.T) {
	var calls [][]string
	createCalls := 0
	d := Docker{Image: "executor:new", Prefix: "pie-", Scope: "manager-a", Command: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "create":
			createCalls++
			if createCalls == 1 {
				return nil, fmt.Errorf("already exists")
			}
		case "inspect":
			return []byte("user-1|manager-a|executor:old||v5|false|false\n"), nil
		}
		return nil, nil
	}}
	if err := d.Ensure(context.Background(), manager.Executor{UserID: "user-1", ID: "executor-user-1"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 5 {
		t.Fatalf("calls=%v", calls)
	}
	if got := strings.Join(calls[2], " "); got != "rm -f pie-executor-user-1" {
		t.Fatalf("remove=%s", got)
	}
	if recreated := strings.Join(calls[3], " "); !strings.Contains(recreated, "executor:new") {
		t.Fatalf("new image missing from recreate: %s", recreated)
	}
	if got := strings.Join(calls[4], " "); got != "start pie-executor-user-1" {
		t.Fatalf("start=%s", got)
	}
}

func TestValidatePermissionMode(t *testing.T) {
	for _, mode := range []string{"", "default", "acceptEdits", "plan", "bypassPermissions"} {
		if err := ValidatePermissionMode(mode); err != nil {
			t.Fatalf("mode=%q err=%v", mode, err)
		}
	}
	if err := ValidatePermissionMode("yolo"); err == nil {
		t.Fatal("unsupported permission mode was accepted")
	}
}

func TestValidateExecutorNetworkRequiresDedicatedBridgeWithICCDisabled(t *testing.T) {
	d := Docker{Network: "pie-executor", Command: func(_ context.Context, args ...string) ([]byte, error) {
		if got := strings.Join(args, " "); !strings.Contains(got, "network inspect") {
			t.Fatalf("unexpected command=%s", got)
		}
		return []byte("bridge|false|false\n"), nil
	}}
	if err := d.ValidateExecutorNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, network := range []string{"", "bridge", "host", "none"} {
		d.Network = network
		if err := d.ValidateExecutorNetwork(context.Background()); err == nil {
			t.Fatalf("network=%q was accepted", network)
		}
	}
	d.Network = "pie-executor"
	d.Command = func(context.Context, ...string) ([]byte, error) { return []byte("bridge|false|true\n"), nil }
	if err := d.ValidateExecutorNetwork(context.Background()); err == nil {
		t.Fatal("ICC-enabled network was accepted")
	}
}

func TestObserveStorageCountsRegularFilesAndSkipsSymlinkTargets(t *testing.T) {
	workRoot, stateRoot, blobRoot := t.TempDir(), t.TempDir(), t.TempDir()
	userID := "user-a"
	for root, size := range map[string]int{workRoot: 11, stateRoot: 13, blobRoot: 17} {
		path := filepath.Join(root, userID)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "data"), make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(workRoot, userID, "data"), filepath.Join(stateRoot, userID, "link")); err != nil {
		t.Fatal(err)
	}
	d := Docker{WorkRoot: workRoot, StateRoot: stateRoot, BlobRoot: blobRoot, DiskQuotaBytes: 100}
	got, err := d.ObserveStorage(context.Background(), manager.Executor{UserID: userID})
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkBytes != 11 || got.StateBytes != 13 || got.BlobBytes != 17 || got.UsedBytes != 41 || got.LimitBytes != 100 || got.FreeBytes <= 0 {
		t.Fatalf("observation=%+v", got)
	}
}

func TestEnforceStorageRejectsQuotaExceeded(t *testing.T) {
	workRoot := t.TempDir()
	path := filepath.Join(workRoot, "user-a")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "large"), make([]byte, 9), 0600); err != nil {
		t.Fatal(err)
	}
	err := (Docker{WorkRoot: workRoot, DiskQuotaBytes: 8}).enforceStorage(context.Background(), manager.Executor{UserID: "user-a"})
	if !errors.Is(err, manager.ErrExecutorDiskQuota) {
		t.Fatalf("err=%v", err)
	}
}

func TestDockerExecutorIsolationE2E(t *testing.T) {
	if os.Getenv("PIE_DOCKER_E2E") != "1" {
		t.Skip("set PIE_DOCKER_E2E=1 to exercise the local Docker daemon")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("Docker daemon unavailable: %v", err)
	}
	image := os.Getenv("PIE_DOCKER_E2E_IMAGE")
	if image == "" {
		image = "pie-relay-client:latest"
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Skipf("executor image %s unavailable: %v", image, err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	network := "pie-isolation-e2e-" + suffix
	if output, err := exec.Command("docker", "network", "create", "--driver", "bridge", "--opt", "com.docker.network.bridge.enable_icc=false", network).CombinedOutput(); err != nil {
		t.Fatalf("create network: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	workRoot, stateRoot, blobRoot := t.TempDir(), t.TempDir(), t.TempDir()
	d := Docker{
		Image: image, Prefix: "pie-isolation-e2e-", Scope: "isolation-e2e", Network: network,
		User: fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()), WorkRoot: workRoot, StateRoot: stateRoot, BlobRoot: blobRoot,
		CPUs: "0.25", Memory: "256m", MemorySwap: "256m", PIDsLimit: "64", DiskQuotaBytes: 8,
	}
	executor := manager.Executor{UserID: "user-" + suffix, ID: "executor-" + suffix}
	t.Cleanup(func() { _ = d.Stop(context.Background(), executor) })
	if err := d.ValidateExecutorNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.Ensure(context.Background(), executor); err != nil {
		t.Fatal(err)
	}
	inspection, err := d.run(context.Background(), "inspect", "--format", `{{.HostConfig.ReadonlyRootfs}}|{{.HostConfig.Memory}}|{{.HostConfig.MemorySwap}}|{{.HostConfig.PidsLimit}}|{{.HostConfig.IpcMode}}|{{.HostConfig.LogConfig.Type}}|{{index .Config.Labels "pie.isolation_version"}}`, d.Prefix+executor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(inspection)); got != "true|268435456|268435456|64|private|local|v3" {
		t.Fatalf("isolation=%s", got)
	}
	userWork := filepath.Join(workRoot, executor.UserID)
	if err := os.WriteFile(filepath.Join(userWork, "quota.bin"), make([]byte, 9), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.enforceStorage(context.Background(), executor); !errors.Is(err, manager.ErrExecutorDiskQuota) {
		t.Fatalf("quota err=%v", err)
	}
}

func TestRootManagerNeverDefaultsExecutorToRoot(t *testing.T) {
	if got := defaultContainerUser("", 0, 0); got != "10001:10001" {
		t.Fatalf("root default=%q", got)
	}
	if got := defaultContainerUser("2000:3000", 0, 0); got != "2000:3000" {
		t.Fatalf("configured=%q", got)
	}
	if got := defaultContainerUser("", 501, 20); got != "501:20" {
		t.Fatalf("non-root default=%q", got)
	}
}

func TestPrepareBindDirCreatesPrivateWritableDirectory(t *testing.T) {
	path := t.TempDir() + "/workspace/user-a"
	identity := fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid())
	if err := prepareBindDir(path, identity); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestPrepareBindDirRepairsNestedSeedOwnershipWhenRootAlreadyMatches(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ownership repair requires root")
	}
	path := t.TempDir() + "/state/user-a"
	if err := os.MkdirAll(filepath.Join(path, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(path, ".claude", ".credentials.json")
	if err := os.WriteFile(credential, []byte(`{"token":"seed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	const uid, gid = 10001, 10001
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := prepareBindDir(path, "10001:10001"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{path, filepath.Join(path, ".claude"), credential} {
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		if int(stat.Uid) != uid || int(stat.Gid) != gid {
			t.Fatalf("%s owner=%d:%d, want %d:%d", target, stat.Uid, stat.Gid, uid, gid)
		}
	}
}

func TestEnsureSeedsPrivateStateWithoutLegacyClaudeCredential(t *testing.T) {
	seedRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(seedRoot, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	seedCredential := filepath.Join(seedRoot, ".claude", ".credentials.json")
	if err := os.WriteFile(seedCredential, []byte(`{"token":"seed-v1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	seedSettings := filepath.Join(seedRoot, ".claude", "settings.json")
	if err := os.WriteFile(seedSettings, []byte(`{"theme":"dark"}`), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot := t.TempDir()
	identity := fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid())
	d := Docker{
		Image: "executor:test", Prefix: "pie-", StateRoot: stateRoot, StateSeedRoot: seedRoot, User: identity,
		Command: func(_ context.Context, _ ...string) ([]byte, error) { return nil, nil },
	}
	executor := manager.Executor{UserID: "user-seeded", ID: "executor-user-seeded"}
	if err := d.Ensure(context.Background(), executor); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(stateRoot, executor.UserID, ".claude", ".credentials.json")
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy Claude credential was seeded: %v", err)
	}
	settingsDestination := filepath.Join(stateRoot, executor.UserID, ".claude", "settings.json")
	content, err := os.ReadFile(settingsDestination)
	if err != nil || string(content) != `{"theme":"dark"}` {
		t.Fatalf("seeded settings=%q err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, executor.UserID, stateSeedMarker)); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if err := os.WriteFile(settingsDestination, []byte(`{"theme":"user-updated"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedCredential, []byte(`{"token":"seed-v2"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.Ensure(context.Background(), executor); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(settingsDestination)
	if err != nil || string(content) != `{"theme":"user-updated"}` {
		t.Fatalf("existing settings were overwritten: %q err=%v", content, err)
	}
}

type recordingCredentialProvisioner struct {
	users []string
	err   error
}

func (p *recordingCredentialProvisioner) EnsureUser(_ context.Context, userID string) error {
	p.users = append(p.users, userID)
	return p.err
}

func TestReconcileProvisionsCurrentCredentialBeforeContainerStateMount(t *testing.T) {
	provisioner := &recordingCredentialProvisioner{}
	d := Docker{
		StateRoot:             t.TempDir(),
		User:                  fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()),
		CredentialProvisioner: provisioner,
	}
	executor := manager.Executor{UserID: "user-auth", ID: "executor-user-auth"}
	if err := d.Reconcile(context.Background(), executor); err != nil {
		t.Fatal(err)
	}
	if len(provisioner.users) != 1 || provisioner.users[0] != executor.UserID {
		t.Fatalf("provisioned users=%v", provisioner.users)
	}
	provisioner.err = errors.New("authentication unavailable")
	if err := d.Reconcile(context.Background(), executor); err == nil || !strings.Contains(err.Error(), "provision executor authentication") {
		t.Fatalf("Reconcile error=%v", err)
	}
}

func TestSeedStateRejectsSymlinkedInput(t *testing.T) {
	seedRoot := t.TempDir()
	if err := os.Symlink("/tmp", filepath.Join(seedRoot, ".claude")); err != nil {
		t.Fatal(err)
	}
	err := seedStateDir(filepath.Join(t.TempDir(), "user"), seedRoot)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}
}

func TestSessionControlUsesContainerLocalAPI(t *testing.T) {
	var gotContainer, gotMethod, gotPath string
	var gotBody []byte
	d := Docker{Prefix: "pie-", SessionCommand: func(_ context.Context, container, method, path string, body []byte) ([]byte, error) {
		gotContainer, gotMethod, gotPath, gotBody = container, method, path, append([]byte(nil), body...)
		if method == "GET" {
			return []byte(`[{"id":"session-a","state":"running"}]`), nil
		}
		return []byte(`{"state":"running"}`), nil
	}}
	e := manager.Executor{ID: "executor-user"}
	if err := d.StartSession(context.Background(), e, manager.SessionSpec{ID: "session-a", RelayURL: "ws://relay/ws/agent", Token: "secret", ClaudeOAuthToken: "oauth-secret", ClaudeAuthVersion: "v-oauth-1"}); err != nil {
		t.Fatal(err)
	}
	if gotContainer != "pie-executor-user" || gotMethod != "POST" || gotPath != "/v1/sessions" || !strings.Contains(string(gotBody), `"token":"secret"`) || !strings.Contains(string(gotBody), `"claudeOAuthToken":"oauth-secret"`) {
		t.Fatalf("container=%s method=%s path=%s body=%s", gotContainer, gotMethod, gotPath, gotBody)
	}
	if err := d.StopSession(context.Background(), e, "session-a"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "DELETE" || gotPath != "/v1/sessions/session-a" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	observed, err := d.ObserveSessions(context.Background(), e)
	if err != nil || len(observed) != 1 || observed[0].ID != "session-a" || observed[0].State != "running" {
		t.Fatalf("observed=%+v err=%v", observed, err)
	}
}

func TestStartSessionRedactsRelayAndClaudeSecretsFromErrors(t *testing.T) {
	d := Docker{Prefix: "pie-", SessionCommand: func(_ context.Context, _, _, _ string, _ []byte) ([]byte, error) {
		return nil, errors.New("rejected relay-secret and oauth-secret")
	}}
	err := d.StartSession(context.Background(), manager.Executor{ID: "executor-user"}, manager.SessionSpec{
		ID: "session-a", RelayURL: "ws://relay/ws/agent", Token: "relay-secret", ClaudeOAuthToken: "oauth-secret",
	})
	if err == nil || strings.Contains(err.Error(), "relay-secret") || strings.Contains(err.Error(), "oauth-secret") || strings.Count(err.Error(), "[REDACTED]") != 2 {
		t.Fatalf("redacted error=%v", err)
	}
}

func TestSessionRequestArgsOnlyKeepsStdinOpenForRequestBodies(t *testing.T) {
	getArgs := sessionRequestArgs("pie-executor", "GET", "/v1/sessions", false)
	if strings.Contains(strings.Join(getArgs, " "), "exec -i") {
		t.Fatalf("bodyless request unexpectedly keeps stdin open: %v", getArgs)
	}
	postArgs := sessionRequestArgs("pie-executor", "POST", "/v1/sessions", true)
	joined := strings.Join(postArgs, " ")
	if !strings.Contains(joined, "exec -i") || !strings.Contains(joined, "timeout 20s") || !strings.Contains(joined, "--timeout 15s") {
		t.Fatalf("request body or timeout safeguards missing: %v", postArgs)
	}
}

func TestEnsurePreviewNetworkCreatesPrivatePerUserPath(t *testing.T) {
	var calls [][]string
	d := Docker{Prefix: "pie-manager-", Scope: "manager-a", Command: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	}}
	backend, err := d.EnsurePreviewNetwork(context.Background(), manager.Executor{ID: "executor-user-a", UserID: "user-a"}, "preview-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(backend, "preview-backend-") || len(calls) != 3 {
		t.Fatalf("backend=%q calls=%v", backend, calls)
	}
	created := strings.Join(calls[0], " ")
	if !strings.Contains(created, "network create --internal") || !strings.Contains(created, "pie.network_purpose=preview") || !strings.Contains(created, "pie.user_id=user-a") {
		t.Fatalf("network create=%s", created)
	}
	if got := strings.Join(calls[1], " "); !strings.Contains(got, "network connect --alias preview-gateway") || !strings.HasSuffix(got, " preview-gateway") {
		t.Fatalf("gateway connect=%s", got)
	}
	if got := strings.Join(calls[2], " "); !strings.Contains(got, "network connect --alias "+backend) || !strings.HasSuffix(got, " pie-manager-executor-user-a") {
		t.Fatalf("executor connect=%s", got)
	}
}

func TestCleanupOrphansRemovesOnlyUnregisteredPreviewNetworks(t *testing.T) {
	var calls [][]string
	d := Docker{Scope: "manager-a", Prefix: "pie-", PreviewGatewayContainer: "preview-gateway", Command: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "ps -a "):
			return []byte("pie-executor-orphan\n"), nil
		case strings.HasPrefix(joined, "network ls "):
			return []byte("preview-kept\npreview-orphan\npreview-unowned\n"), nil
		case joined == `network inspect --format {{index .Labels "pie.manager_id"}}|{{index .Labels "pie.user_id"}}|{{index .Labels "pie.network_purpose"}} preview-kept`:
			return []byte("manager-a|user-kept|preview\n"), nil
		case joined == `network inspect --format {{index .Labels "pie.manager_id"}}|{{index .Labels "pie.user_id"}}|{{index .Labels "pie.network_purpose"}} preview-orphan`:
			return []byte("manager-a|user-orphan|preview\n"), nil
		case joined == `network inspect --format {{index .Labels "pie.manager_id"}}|{{index .Labels "pie.user_id"}}|{{index .Labels "pie.network_purpose"}} preview-unowned`:
			return []byte("other-manager|user-orphan|preview\n"), nil
		default:
			return nil, nil
		}
	}}
	if err := d.CleanupOrphans(context.Background(), []manager.Executor{{ID: "executor-kept", UserID: "user-kept"}}); err != nil {
		t.Fatal(err)
	}
	all := make([]string, len(calls))
	for index, call := range calls {
		all[index] = strings.Join(call, " ")
	}
	joined := strings.Join(all, "\n")
	for _, expected := range []string{
		"rm -f pie-executor-orphan",
		"network disconnect -f preview-orphan preview-gateway",
		"network rm preview-orphan",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing %q in calls:\n%s", expected, joined)
		}
	}
	if strings.Contains(joined, "network rm preview-kept") || strings.Contains(joined, "network rm preview-unowned") {
		t.Fatalf("registered or unowned network was removed:\n%s", joined)
	}
}

func TestStopMissingExecutorStillRemovesOwnedPreviewNetwork(t *testing.T) {
	var calls [][]string
	d := Docker{Scope: "manager-a", Prefix: "pie-", PreviewGatewayContainer: "preview-gateway", Command: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) >= 3 && args[0] == "rm" && args[1] == "-f" {
			return nil, errors.New("No such container")
		}
		return nil, nil
	}}
	err := d.Stop(context.Background(), manager.Executor{ID: "executor-user-a", UserID: "user-a"})
	if !errors.Is(err, manager.ErrExecutorNotFound) {
		t.Fatalf("err=%v", err)
	}
	joined := make([]string, len(calls))
	for index, call := range calls {
		joined[index] = strings.Join(call, " ")
	}
	all := strings.Join(joined, "\n")
	if !strings.Contains(all, "network disconnect -f pie-preview-") || !strings.Contains(all, "network rm pie-preview-") {
		t.Fatalf("preview network cleanup missing:\n%s", all)
	}
}

func TestInitializeProjectRunsKrootInsideOpaqueWorkspace(t *testing.T) {
	var calls [][]string
	d := Docker{Prefix: "pie-", WorkRoot: t.TempDir(), Command: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) >= 4 && args[0] == "exec" && args[2] == "test" {
			return nil, errors.New("exit status 1")
		}
		return nil, nil
	}}
	err := d.InitializeProject(context.Background(), manager.Executor{ID: "executor-user-a"}, manager.ProjectSpec{ID: "project-safe-id", Name: "쇼핑몰 관리자", Locale: "ko"})
	if err != nil {
		t.Fatal(err)
	}
	joined := make([]string, len(calls))
	for index, call := range calls {
		joined[index] = strings.Join(call, " ")
	}
	all := strings.Join(joined, "\n")
	for _, expected := range []string{
		"exec pie-executor-user-a rm -rf -- /workspace/projects/project-safe-id",
		"exec pie-executor-user-a mkdir -p -- /workspace/projects/project-safe-id",
		"exec --workdir /workspace/projects/project-safe-id pie-executor-user-a kroot init . 쇼핑몰 관리자 --non-interactive --locale ko",
		"exec pie-executor-user-a touch -- /workspace/projects/project-safe-id/.pie-kroot-initialized",
	} {
		if !strings.Contains(all, expected) {
			t.Fatalf("missing %q in calls:\n%s", expected, all)
		}
	}
	if strings.Contains(all, "/workspace/projects/쇼핑몰") {
		t.Fatalf("display name leaked into workspace path:\n%s", all)
	}
}

func TestInitializeProjectCanCreateKrootBackendLink(t *testing.T) {
	var calls [][]string
	d := Docker{Prefix: "pie-", WorkRoot: t.TempDir(), KrootAutoLink: true, Command: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) >= 4 && args[0] == "exec" && args[2] == "test" {
			return nil, errors.New("exit status 1")
		}
		return nil, nil
	}}
	if err := d.InitializeProject(context.Background(), manager.Executor{ID: "executor-user-a"}, manager.ProjectSpec{ID: "project-safe-id", Name: "사용자 프로젝트", Locale: "ko"}); err != nil {
		t.Fatal(err)
	}
	joined := make([]string, len(calls))
	for index, call := range calls {
		joined[index] = strings.Join(call, " ")
	}
	all := strings.Join(joined, "\n")
	if !strings.Contains(all, "exec --workdir /workspace/projects/project-safe-id pie-executor-user-a kroot link --create --name 사용자 프로젝트 --description Provisioned by Pie Relay") {
		t.Fatalf("Kroot backend link command missing:\n%s", all)
	}
	if strings.Index(all, "kroot link --create") > strings.Index(all, "touch -- /workspace/projects/project-safe-id/.pie-kroot-initialized") {
		t.Fatalf("ready marker was written before backend link:\n%s", all)
	}
}

func TestDiscoverProjectApplicationsUsesExecutorProjectEndpoint(t *testing.T) {
	var gotContainer, gotMethod, gotPath string
	d := Docker{Prefix: "pie-", SessionCommand: func(_ context.Context, container, method, path string, body []byte) ([]byte, error) {
		gotContainer, gotMethod, gotPath = container, method, path
		if len(body) != 0 {
			t.Fatalf("unexpected body=%q", body)
		}
		return []byte(`[{"path":"apps/web","name":"Customer Web","profile":"next"}]`), nil
	}}
	applications, err := d.DiscoverProjectApplications(context.Background(), manager.Executor{ID: "executor-user-a"}, "project-safe-id")
	if err != nil {
		t.Fatal(err)
	}
	if gotContainer != "pie-executor-user-a" || gotMethod != "GET" || gotPath != "/v1/projects/project-safe-id/apps" {
		t.Fatalf("request=%s %s %s", gotContainer, gotMethod, gotPath)
	}
	if len(applications) != 1 || applications[0].Path != "apps/web" || applications[0].Name != "Customer Web" || applications[0].Profile != "next" {
		t.Fatalf("applications=%+v", applications)
	}
}

func TestEnsureRejectsMismatchedExistingContainer(t *testing.T) {
	d := Docker{Image: "executor:test", Prefix: "pie-", Scope: "manager-a", Command: func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "create":
			return nil, fmt.Errorf("already exists")
		case "inspect":
			return []byte("other|manager-b|wrong:image|default|v5|false\n"), nil
		default:
			return nil, nil
		}
	}}
	err := d.Ensure(context.Background(), manager.Executor{UserID: "user-1", ID: "executor-user-1"})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err=%v", err)
	}
}

func TestDockerNameConflictSupportsCurrentDaemonWording(t *testing.T) {
	for _, message := range []string{
		"container already exists",
		"Conflict. The container name /pie-user is already in use by container abc",
	} {
		if !dockerNameConflict(errors.New(message)) {
			t.Fatalf("name conflict was not recognized: %s", message)
		}
	}
	if dockerNameConflict(errors.New("permission denied")) {
		t.Fatal("unrelated Docker error was treated as a name conflict")
	}
}

func TestResetStopsBeforeRestartingExecutor(t *testing.T) {
	var calls []string
	d := Docker{Command: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}}
	if err := d.reset(context.Background(), "pie-executor"); err != nil {
		t.Fatal(err)
	}
	want := []string{"stop -t 2 pie-executor", "start pie-executor"}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("calls=%v", calls)
	}
}
