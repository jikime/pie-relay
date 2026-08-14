package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

const maxCapturedOutputBytes = 4 << 20

const sessionRequestProcessTimeout = 20 * time.Second

type Docker struct {
	Image                    string
	Prefix                   string
	BlobRoot                 string
	WorkRoot                 string
	StateRoot                string
	StateSeedRoot            string
	KrootCommonBundleRoot    string
	KrootCommonBundleVersion string
	Network                  string
	CPUs                     string
	Memory                   string
	MemorySwap               string
	PIDsLimit                string
	DiskQuotaBytes           int64
	DiskHeadroomBytes        int64
	Scope                    string
	User                     string
	PermissionMode           string
	AllowUserNamespaces      bool
	KrootAutoLink            bool
	PreviewGatewayContainer  string
	CredentialProvisioner    StateCredentialProvisioner
	Command                  func(context.Context, ...string) ([]byte, error)
	SessionCommand           func(context.Context, string, string, string, []byte) ([]byte, error)
}

// StateCredentialProvisioner updates credentials in the persistent per-user
// state directory before Docker mounts it into an executor container.
// Keeping this boundary in runtime avoids teaching the Manager about a
// particular credential file format.
type StateCredentialProvisioner interface {
	EnsureUser(context.Context, string) error
}

const executorIsolationVersion = "v5"

// ValidatePermissionMode keeps the Manager-side policy aligned with the
// permission modes supported by the Node Executor. An empty value preserves
// the request-level policy for backwards compatibility.
func ValidatePermissionMode(mode string) error {
	switch strings.TrimSpace(mode) {
	case "", "default", "acceptEdits", "plan", "bypassPermissions":
		return nil
	default:
		return fmt.Errorf("unsupported executor permission mode %q", mode)
	}
}

func (d Docker) Health(ctx context.Context) error {
	_, err := d.run(ctx, "info", "--format", "{{.ServerVersion}}")
	return err
}

// ValidateExecutorNetwork prevents a production Manager from silently falling
// back to Docker's shared default bridge. Executor networks need Internet
// egress, but container-to-container communication must be disabled.
func (d Docker) ValidateExecutorNetwork(ctx context.Context) error {
	network := strings.TrimSpace(d.Network)
	if network == "" || network == "bridge" || network == "host" || network == "none" {
		return errors.New("executor network must be a dedicated bridge")
	}
	out, err := d.run(ctx, "network", "inspect", "--format", `{{.Driver}}|{{.Internal}}|{{index .Options "com.docker.network.bridge.enable_icc"}}`, network)
	if err != nil {
		return fmt.Errorf("inspect executor network %s: %w", network, err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 3)
	if len(parts) != 3 || parts[0] != "bridge" || parts[1] != "false" || !strings.EqualFold(parts[2], "false") {
		return fmt.Errorf("executor network %s must be a non-internal bridge with ICC disabled", network)
	}
	return nil
}

func (d Docker) Observe(ctx context.Context, e manager.Executor) (manager.RuntimeObservation, error) {
	name := d.Prefix + e.ID
	out, err := d.run(ctx, "inspect", "--format", "{{json .}}", name)
	if err != nil {
		if strings.Contains(err.Error(), "No such object") || strings.Contains(err.Error(), "No such container") {
			return manager.RuntimeObservation{}, manager.ErrExecutorNotFound
		}
		return manager.RuntimeObservation{}, err
	}
	var inspect struct {
		ID     string `json:"Id"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		HostConfig struct {
			SecurityOpt   []string `json:"SecurityOpt"`
			MaskedPaths   []string `json:"MaskedPaths"`
			ReadonlyPaths []string `json:"ReadonlyPaths"`
		} `json:"HostConfig"`
		State struct {
			Status  string `json:"Status"`
			Running bool   `json:"Running"`
			Health  *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &inspect); err != nil {
		return manager.RuntimeObservation{}, fmt.Errorf("decode docker inspect for %s: %w", name, err)
	}
	health := "unknown"
	if inspect.State.Health != nil && inspect.State.Health.Status != "" {
		health = inspect.State.Health.Status
	} else if inspect.State.Running {
		health = "running"
	}
	permissionDrift := d.PermissionMode != "" && inspect.Config.Labels["pie.permission_mode"] != d.PermissionMode
	userNamespaceLabel := strconv.FormatBool(d.AllowUserNamespaces)
	userNamespaceDrift := inspect.Config.Labels["pie.user_namespaces"] != userNamespaceLabel
	seccompUnconfined := false
	for _, option := range inspect.HostConfig.SecurityOpt {
		if option == "seccomp=unconfined" {
			seccompUnconfined = true
		}
	}
	// Docker consumes systempaths=unconfined at create time and represents it
	// in inspect output as empty masked/readonly path lists; it does not retain
	// the option in HostConfig.SecurityOpt.
	systemPathsUnconfined := len(inspect.HostConfig.MaskedPaths) == 0 && len(inspect.HostConfig.ReadonlyPaths) == 0
	commonHomeLabel := strconv.FormatBool(strings.TrimSpace(d.KrootCommonBundleRoot) != "")
	commonHomeDrift := inspect.Config.Labels["pie.kroot_common_home"] != commonHomeLabel
	drifted := inspect.Config.Image != d.Image || inspect.Config.Labels["pie.isolation_version"] != executorIsolationVersion || permissionDrift || userNamespaceDrift || commonHomeDrift || seccompUnconfined != d.AllowUserNamespaces || systemPathsUnconfined != d.AllowUserNamespaces
	return manager.RuntimeObservation{RuntimeID: inspect.ID, Image: inspect.Config.Image, Status: inspect.State.Status, Health: health, Running: inspect.State.Running, Drifted: drifted}, nil
}

// CleanupOrphans removes containers created by this manager that are no longer
// present in the durable executor registry.
func (d Docker) CleanupOrphans(ctx context.Context, executors []manager.Executor) error {
	keep := map[string]bool{}
	keepUsers := map[string]bool{}
	for _, e := range executors {
		keep[d.Prefix+e.ID] = true
		keepUsers[e.UserID] = true
	}
	out, err := d.run(ctx, "ps", "-a", "--filter", "label=pie.user_id", "--filter", "label=pie.manager_id="+d.scope(), "--format", "{{.Names}}")
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(string(out)) {
		if !keep[name] {
			if _, e := d.run(ctx, "rm", "-f", name); e != nil {
				return e
			}
		}
	}
	// A process or host failure can leave the private per-user preview network
	// behind after its executor container has gone. Only networks carrying all
	// three Manager-owned labels are eligible for cleanup; an unrelated Docker
	// network is never removed based on its name alone.
	networks, err := d.run(ctx, "network", "ls", "--filter", "label=pie.manager_id="+d.scope(), "--filter", "label=pie.network_purpose=preview", "--format", "{{.Name}}")
	if err != nil {
		return err
	}
	for _, network := range strings.Fields(string(networks)) {
		labels, inspectErr := d.run(ctx, "network", "inspect", "--format", `{{index .Labels "pie.manager_id"}}|{{index .Labels "pie.user_id"}}|{{index .Labels "pie.network_purpose"}}`, network)
		if inspectErr != nil {
			return inspectErr
		}
		parts := strings.SplitN(strings.TrimSpace(string(labels)), "|", 3)
		if len(parts) != 3 || parts[0] != d.scope() || parts[1] == "" || parts[2] != "preview" || keepUsers[parts[1]] {
			continue
		}
		if d.PreviewGatewayContainer != "" {
			_, _ = d.run(ctx, "network", "disconnect", "-f", network, d.PreviewGatewayContainer)
		}
		if _, removeErr := d.run(ctx, "network", "rm", network); removeErr != nil {
			return removeErr
		}
	}
	return nil
}

func (d Docker) Ensure(ctx context.Context, e manager.Executor) error {
	permissionMode := strings.TrimSpace(d.PermissionMode)
	if err := ValidatePermissionMode(permissionMode); err != nil {
		return err
	}
	network := d.Network
	if network == "" {
		network = "bridge"
	}
	cpus := d.CPUs
	if e.CPUs != "" {
		cpus = e.CPUs
	}
	if cpus == "" {
		cpus = "2"
	}
	memory := d.Memory
	if e.MemoryBytes > 0 {
		memory = strconv.FormatInt(e.MemoryBytes, 10)
	}
	if memory == "" {
		memory = "2g"
	}
	memorySwap := strings.TrimSpace(d.MemorySwap)
	if memorySwap == "" {
		// Equal memory and memory-swap values disable container swap and stop one
		// tenant from consuming the node-wide swap pool under pressure.
		memorySwap = memory
	}
	pids := d.PIDsLimit
	if e.PIDsLimit > 0 {
		pids = strconv.FormatInt(e.PIDsLimit, 10)
	}
	if pids == "" {
		pids = "256"
	}
	containerUser := defaultContainerUser(d.User, os.Geteuid(), os.Getegid())
	if err := d.Reconcile(ctx, e); err != nil {
		return err
	}
	commonHomeEnabled := strings.TrimSpace(d.KrootCommonBundleRoot) != ""
	args := []string{"create", "--label", "pie.user_id=" + e.UserID, "--label", "pie.manager_id=" + d.scope(), "--label", "pie.isolation_version=" + executorIsolationVersion, "--label", "pie.user_namespaces=" + strconv.FormatBool(d.AllowUserNamespaces), "--label", "pie.kroot_common_home=" + strconv.FormatBool(commonHomeEnabled)}
	if permissionMode != "" {
		args = append(args, "--label", "pie.permission_mode="+permissionMode)
	}
	args = append(args,
		"--name", d.Prefix+e.ID,
		"--hostname", "pie-executor",
		"--network", network,
		"--user", containerUser,
		"--cpus", cpus,
		"--memory", memory,
		"--memory-swap", memorySwap,
		"--pids-limit", pids,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges=true",
		"--ipc", "private",
		"--init",
		"--ulimit", "core=0:0",
		"--ulimit", "nofile=4096:4096",
		"--restart", "no",
		"--stop-timeout", "10",
		"--read-only",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=256m,mode=1777",
		// Claude Code's subprocess environment scrub uses a nested bubblewrap
		// sandbox and stages an ephemeral MCP file directly below /home. Keep the
		// image root read-only, but give only /home a small non-persistent tmpfs;
		// the durable per-user HOME remains the nested /home/executor bind mount.
		"--tmpfs", "/home:rw,nosuid,nodev,size=16m,mode=1777",
		"--log-driver", "local",
		"--log-opt", "max-size=10m",
		"--log-opt", "max-file=3",
	)
	if d.AllowUserNamespaces {
		// Claude's subprocess credential scrub uses rootless bubblewrap. The
		// Docker's default seccomp profile blocks its user-namespace clone, and
		// Docker's masked system paths prevent the nested PID namespace from
		// mounting its own /proc. Scope both opt-outs to Executor containers that
		// explicitly request rootless bubblewrap. cap-drop ALL, non-root UID,
		// no-new-privileges, read-only rootfs, a private PID namespace and
		// resource/network isolation still form the outer security boundary.
		args = append(args,
			"--security-opt", "seccomp=unconfined",
			"--security-opt", "systempaths=unconfined",
		)
	}
	if permissionMode != "" {
		args = append(args, "-e", "CLI_RELAY_PERMISSION_MODE="+permissionMode)
	}
	if commonHomeEnabled {
		// The Manager has already synchronized Kroot's shared skills and agents
		// into the persistent user HOME. This environment flag keeps manual and
		// Manager-initiated `kroot init` calls from copying those same assets into
		// every project while leaving all other project templates unchanged.
		args = append(args, "-e", "KROOT_USER_COMMON=true")
	}
	if d.WorkRoot != "" {
		work := filepath.Join(d.WorkRoot, e.UserID)
		args = append(args, "-v", work+":/workspace:rw", "-w", "/workspace")
	}
	if d.BlobRoot != "" {
		blobs := filepath.Join(d.BlobRoot, e.UserID)
		args = append(args, "-v", blobs+":/workspace/input:ro")
	}
	if d.StateRoot != "" {
		state := filepath.Join(d.StateRoot, e.UserID)
		args = append(args, "-v", state+":/home/executor:rw", "-e", "HOME=/home/executor")
	}
	args = append(args,
		"--health-cmd", "pie-client sessions request --path /healthz",
		"--health-interval", "15s", "--health-timeout", "5s", "--health-retries", "3",
		d.Image, "pie-client", "sessions", "serve", "--listen", "127.0.0.1:19091",
	)
	name := d.Prefix + e.ID
	_, err := d.run(ctx, args...)
	if err != nil {
		if !dockerNameConflict(err) {
			return err
		}
		out, inspectErr := d.run(ctx, "inspect", "--format", `{{index .Config.Labels "pie.user_id"}}|{{index .Config.Labels "pie.manager_id"}}|{{.Config.Image}}|{{with index .Config.Labels "pie.permission_mode"}}{{.}}{{end}}|{{with index .Config.Labels "pie.isolation_version"}}{{.}}{{end}}|{{with index .Config.Labels "pie.user_namespaces"}}{{.}}{{end}}|{{with index .Config.Labels "pie.kroot_common_home"}}{{.}}{{end}}`, name)
		if inspectErr != nil {
			return inspectErr
		}
		parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 7)
		if len(parts) != 7 || parts[0] != e.UserID || parts[1] != d.scope() {
			return fmt.Errorf("existing executor container %s does not match manager ownership", name)
		}
		// Image and environment variables cannot be changed on an existing
		// Docker container. During a release or permission-policy change,
		// recreate only the owned container shell; per-user Home and workspace
		// remain safe in their bind mounts.
		imageChanged := parts[2] != d.Image
		policyChanged := (permissionMode != "" && parts[3] != permissionMode) || parts[4] != executorIsolationVersion || parts[5] != strconv.FormatBool(d.AllowUserNamespaces) || parts[6] != strconv.FormatBool(commonHomeEnabled)
		if imageChanged || policyChanged {
			if _, removeErr := d.run(ctx, "rm", "-f", name); removeErr != nil {
				return fmt.Errorf("remove executor container %s for runtime reconciliation: %w", name, removeErr)
			}
			if _, createErr := d.run(ctx, args...); createErr != nil {
				return fmt.Errorf("recreate executor container %s for runtime reconciliation: %w", name, createErr)
			}
			_, startErr := d.run(ctx, "start", name)
			return startErr
		}
		if _, updateErr := d.run(ctx, "update", "--cpus", cpus, "--memory", memory, "--memory-swap", memorySwap, "--pids-limit", pids, name); updateErr != nil {
			return fmt.Errorf("update executor container %s resources: %w", name, updateErr)
		}
	}
	_, err = d.run(ctx, "start", name)
	return err
}

// Reconcile repairs the private bind-mount roots even when the Executor
// container itself is already healthy. Credential provisioning can create the
// user root first and the common Claude seed afterwards; checking only the
// root owner would otherwise leave those later children owned by Manager root.
func (d Docker) Reconcile(ctx context.Context, e manager.Executor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	containerUser := defaultContainerUser(d.User, os.Geteuid(), os.Getegid())
	for _, root := range []string{d.WorkRoot, d.BlobRoot} {
		if root == "" {
			continue
		}
		if err := prepareBindDir(filepath.Join(root, e.UserID), containerUser); err != nil {
			return err
		}
	}
	if d.StateRoot != "" {
		state := filepath.Join(d.StateRoot, e.UserID)
		if err := seedStateDir(state, d.StateSeedRoot); err != nil {
			return fmt.Errorf("seed executor state for %s: %w", e.UserID, err)
		}
		if err := syncKrootCommonBundle(state, d.KrootCommonBundleRoot, d.KrootCommonBundleVersion); err != nil {
			return fmt.Errorf("sync Kroot common bundle for %s: %w", e.UserID, err)
		}
		if d.CredentialProvisioner != nil {
			if err := d.CredentialProvisioner.EnsureUser(ctx, e.UserID); err != nil {
				return fmt.Errorf("provision executor authentication for %s: %w", e.UserID, err)
			}
		}
		if err := prepareBindDir(state, containerUser); err != nil {
			return err
		}
	}
	return d.enforceStorage(ctx, e)
}

func (d Docker) ObserveStorage(ctx context.Context, e manager.Executor) (manager.StorageObservation, error) {
	observation := manager.StorageObservation{LimitBytes: e.DiskBytes}
	if observation.LimitBytes == 0 {
		observation.LimitBytes = d.DiskQuotaBytes
	}
	var err error
	if d.WorkRoot != "" {
		observation.WorkBytes, err = directoryUsage(ctx, filepath.Join(d.WorkRoot, e.UserID))
		if err != nil {
			return observation, fmt.Errorf("measure executor workspace: %w", err)
		}
	}
	if d.StateRoot != "" {
		observation.StateBytes, err = directoryUsage(ctx, filepath.Join(d.StateRoot, e.UserID))
		if err != nil {
			return observation, fmt.Errorf("measure executor state: %w", err)
		}
	}
	if d.BlobRoot != "" {
		observation.BlobBytes, err = directoryUsage(ctx, filepath.Join(d.BlobRoot, e.UserID))
		if err != nil {
			return observation, fmt.Errorf("measure executor blobs: %w", err)
		}
	}
	observation.UsedBytes = observation.WorkBytes + observation.StateBytes + observation.BlobBytes
	root := d.WorkRoot
	if root == "" {
		root = d.StateRoot
	}
	if root != "" {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(root, &stat); err != nil {
			return observation, fmt.Errorf("measure executor filesystem headroom: %w", err)
		}
		observation.FreeBytes = int64(stat.Bavail) * int64(stat.Bsize)
	}
	return observation, nil
}

func (d Docker) enforceStorage(ctx context.Context, e manager.Executor) error {
	observation, err := d.ObserveStorage(ctx, e)
	if err != nil {
		return err
	}
	if observation.LimitBytes > 0 && observation.UsedBytes > observation.LimitBytes {
		return fmt.Errorf("%w: used=%d limit=%d", manager.ErrExecutorDiskQuota, observation.UsedBytes, observation.LimitBytes)
	}
	if d.DiskHeadroomBytes > 0 && observation.FreeBytes > 0 && observation.FreeBytes < d.DiskHeadroomBytes {
		return fmt.Errorf("%w: free=%d required=%d", manager.ErrExecutorDiskCapacity, observation.FreeBytes, d.DiskHeadroomBytes)
	}
	return nil
}

func directoryUsage(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > 0 && total > math.MaxInt64-info.Size() {
			return errors.New("executor disk usage overflow")
		}
		total += info.Size()
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return total, err
}

const stateSeedMarker = ".pie-state-seed-v1"

// seedStateDir copies an operator-controlled configuration seed into
// a new user's private Home before Docker starts. Existing user files are never
// overwritten. The marker makes retries cheap while the exclusive file/link
// operations and symlink rejection keep a writable user Home from redirecting
// privileged Manager writes outside its own state directory. Claude's legacy
// refreshable credential is explicitly excluded: subscription OAuth is injected
// only into a Claude child process by the central broker.
func seedStateDir(stateRoot, seedRoot string) error {
	if strings.TrimSpace(seedRoot) == "" {
		return nil
	}
	seedInfo, err := os.Lstat(seedRoot)
	if err != nil {
		return fmt.Errorf("inspect seed root: %w", err)
	}
	if seedInfo.Mode()&os.ModeSymlink != 0 || !seedInfo.IsDir() {
		return errors.New("state seed root must be a real directory")
	}
	if err := ensureRealDirectory(stateRoot); err != nil {
		return err
	}
	marker := filepath.Join(stateRoot, stateSeedMarker)
	if info, markerErr := os.Lstat(marker); markerErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("state seed marker is not a regular file")
		}
		return nil
	} else if !errors.Is(markerErr, os.ErrNotExist) {
		return markerErr
	}
	if err := filepath.WalkDir(seedRoot, func(source string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(seedRoot, source)
		if err != nil || relative == "." {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("state seed contains symlink: %s", relative)
		}
		if filepath.ToSlash(relative) == ".claude/.credentials.json" {
			return nil
		}
		target := filepath.Join(stateRoot, relative)
		if entry.IsDir() {
			return ensureRealDirectory(target)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("state seed contains unsupported file: %s", relative)
		}
		if targetInfo, targetErr := os.Lstat(target); targetErr == nil {
			if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.Mode().IsRegular() {
				return fmt.Errorf("state seed target is not a regular file: %s", relative)
			}
			return nil
		} else if !errors.Is(targetErr, os.ErrNotExist) {
			return targetErr
		}
		return copySeedFile(source, target)
	}); err != nil {
		return err
	}
	if err := writeSeedMarker(marker); err != nil {
		return err
	}
	return syncDirectory(stateRoot)
}

func ensureRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.Mkdir(path, 0700)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("path is not a real directory: %s", path)
	}
	return nil
}

func copySeedFile(source, target string) (result error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	temporary, err := os.CreateTemp(filepath.Dir(target), ".pie-seed-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}()
	if err := temporary.Chmod(0600); err != nil {
		return err
	}
	if _, err := io.Copy(temporary, in); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryName, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	return syncDirectory(filepath.Dir(target))
}

func writeSeedMarker(path string) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pie-seed-marker-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}()
	if err := temporary.Chmod(0600); err != nil {
		return err
	}
	if _, err := temporary.WriteString("v1\n"); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryName, path); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func dockerNameConflict(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "already exists") || strings.Contains(message, "already in use")
}

func defaultContainerUser(configured string, effectiveUID, effectiveGID int) string {
	if configured != "" {
		return configured
	}
	// The production Manager needs Docker socket access and therefore normally
	// runs as root.  Mirroring its uid into an Executor silently disables the
	// image's USER pie boundary, so root Managers default to the image's fixed
	// non-root uid/gid instead.
	if effectiveUID == 0 {
		return "10001:10001"
	}
	return fmt.Sprintf("%d:%d", effectiveUID, effectiveGID)
}

func prepareBindDir(path, containerUser string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0700); err != nil {
		return err
	}
	parts := strings.Split(containerUser, ":")
	if len(parts) != 2 {
		return nil // Docker also accepts named users; ownership must then be pre-provisioned.
	}
	uid, uidErr := strconv.Atoi(parts[0])
	gid, gidErr := strconv.Atoi(parts[1])
	if uidErr != nil || gidErr != nil || uid < 0 || gid < 0 {
		return nil
	}
	// A previous release could run Executor containers as root.  When migrating
	// to the fixed non-root identity, changing only the mount root would leave
	// existing Claude credentials and workspace files unreadable. A credential
	// seed can also add root-owned children after the mount root was already
	// assigned to the Executor UID. Therefore the root owner alone is not a
	// sufficient fast path. WalkDir never follows symlinks; Lchown updates a
	// symlink itself without escaping the user-specific directory.
	if err := filepath.WalkDir(path, func(current string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) == uid && int(stat.Gid) == gid {
			return nil
		}
		return os.Lchown(current, uid, gid)
	}); err != nil {
		return fmt.Errorf("set bind directory owner %s to %s: %w", path, containerUser, err)
	}
	return nil
}
func (d Docker) Run(ctx context.Context, j manager.Job) ([]byte, error) {
	name := d.Prefix + j.ExecutorID
	return d.run(ctx, "exec", name, "env", "PIE_BLOB_REFS="+strings.Join(j.BlobRefs, ","), "sh", "-lc", string(j.Input))
}
func (d Docker) RunStream(ctx context.Context, j manager.Job, emit func([]byte)) ([]byte, error) {
	name := d.Prefix + j.ExecutorID
	cmd := exec.CommandContext(ctx, "docker", "exec", name, "env", "PIE_BLOB_REFS="+strings.Join(j.BlobRefs, ","), "sh", "-lc", string(j.Input))
	var buf bytes.Buffer
	w := &streamWriter{emit: emit, buf: &buf}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	if ctx.Err() != nil {
		resetCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if resetErr := d.reset(resetCtx, name); resetErr != nil {
			return buf.Bytes(), errors.Join(ctx.Err(), manager.ErrExecutorUnavailable, resetErr)
		}
		return buf.Bytes(), ctx.Err()
	}
	return buf.Bytes(), err
}

func (d Docker) reset(ctx context.Context, name string) error {
	_, stopErr := d.run(ctx, "stop", "-t", "2", name)
	_, startErr := d.run(ctx, "start", name)
	return errors.Join(stopErr, startErr)
}

type streamWriter struct {
	emit func([]byte)
	buf  *bytes.Buffer
}

func (w *streamWriter) Write(p []byte) (int, error) {
	original := len(p)
	remaining := maxCapturedOutputBytes - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = w.buf.Write(p[:remaining])
		} else {
			_, _ = w.buf.Write(p)
		}
	}
	w.emit(append([]byte(nil), p...))
	return original, nil
}
func (d Docker) Stop(ctx context.Context, e manager.Executor) error {
	_, err := d.run(ctx, "rm", "-f", d.Prefix+e.ID)
	missing := err != nil && (strings.Contains(err.Error(), "No such container") || strings.Contains(err.Error(), "No such object"))
	if err != nil && !missing {
		return err
	}
	if d.PreviewGatewayContainer != "" {
		network := d.previewNetworkName(e)
		_, _ = d.run(ctx, "network", "disconnect", "-f", network, d.PreviewGatewayContainer)
		_, _ = d.run(ctx, "network", "rm", network)
	}
	if missing {
		return manager.ErrExecutorNotFound
	}
	return nil
}

func (d Docker) StartSession(ctx context.Context, e manager.Executor, spec manager.SessionSpec) error {
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	defer func() {
		for index := range body {
			body[index] = 0
		}
	}()
	_, err = d.sessionRequest(ctx, d.Prefix+e.ID, "POST", "/v1/sessions", body)
	if err != nil {
		message := err.Error()
		for _, secret := range []string{spec.Token, spec.ClaudeOAuthToken} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[REDACTED]")
			}
		}
		return errors.New(message)
	}
	return err
}

// RestartForClaudeAuth stops any in-memory Claude processes after a central
// setup-token rotation. No credential is installed or mounted in the Executor;
// the next chat session receives the active version over the private runtime
// channel. Stopped executors remain stopped.
func (d Docker) RestartForClaudeAuth(ctx context.Context, e manager.Executor) error {
	name := d.Prefix + e.ID
	observation, err := d.Observe(ctx, e)
	if err != nil {
		return err
	}
	if !observation.Running {
		return nil
	}
	// A credential rollout commonly accompanies an Executor image release.
	// Reconcile the currently running, manager-owned shell first so the next
	// chat cannot restart successfully on an obsolete clientd implementation.
	if err := d.Ensure(ctx, e); err != nil {
		return fmt.Errorf("reconcile executor before Claude authentication update: %w", err)
	}
	if _, err := d.run(ctx, "restart", "--time", "10", name); err != nil {
		return fmt.Errorf("restart executor after Claude authentication update: %w", err)
	}
	deadline := time.Now().Add(45 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		observation, err = d.Observe(ctx, e)
		if err == nil && observation.Running && (observation.Health == "healthy" || observation.Health == "running") {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("executor did not become healthy after Claude authentication update")
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// InitializeProject creates one server-owned workspace inside the user's
// persistent /workspace mount and runs the image-bundled Kroot CLI there.
// Project IDs, never display names, form the path. A ready marker is written
// only after kroot exits successfully, so a Manager crash cannot mistake a
// half-written .kroot directory for a completed project on retry.
func (d Docker) InitializeProject(ctx context.Context, e manager.Executor, spec manager.ProjectSpec) error {
	if d.WorkRoot == "" {
		return errors.New("executor workspace storage is not configured")
	}
	if !manager.ValidUserID(spec.ID) || strings.TrimSpace(spec.Name) == "" {
		return errors.New("invalid project specification")
	}
	locale := strings.TrimSpace(spec.Locale)
	if locale == "" {
		locale = "ko"
	}
	switch locale {
	case "ko", "en", "ja", "zh":
	default:
		return errors.New("unsupported project locale")
	}
	container := d.Prefix + e.ID
	workingDir := "/workspace/projects/" + spec.ID
	readyMarker := workingDir + "/.pie-kroot-initialized"
	if _, err := d.run(ctx, "exec", container, "test", "-f", readyMarker); err == nil {
		return nil
	}
	// The directory is not visible to chat sessions until the durable Project
	// record reaches ready. It is therefore safe to remove a partial first
	// attempt whose success marker was never committed.
	if _, err := d.run(ctx, "exec", container, "rm", "-rf", "--", workingDir); err != nil {
		return fmt.Errorf("clean partial project %s: %w", spec.ID, err)
	}
	if _, err := d.run(ctx, "exec", container, "mkdir", "-p", "--", workingDir); err != nil {
		return fmt.Errorf("create project directory %s: %w", spec.ID, err)
	}
	_, initErr := d.run(ctx,
		"exec", "--workdir", workingDir, container,
		"kroot", "init", ".", strings.TrimSpace(spec.Name),
		"--non-interactive", "--locale", locale,
	)
	if initErr != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_, cleanupErr := d.run(cleanupCtx, "exec", container, "rm", "-rf", "--", workingDir)
		return errors.Join(fmt.Errorf("initialize Kroot project %s: %w", spec.ID, initErr), cleanupErr)
	}
	if d.KrootAutoLink {
		_, linkErr := d.run(ctx,
			"exec", "--workdir", workingDir, container,
			"kroot", "link", "--create", "--name", strings.TrimSpace(spec.Name),
			"--description", "Provisioned by Pie Relay",
		)
		if linkErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			_, cleanupErr := d.run(cleanupCtx, "exec", container, "rm", "-rf", "--", workingDir)
			return errors.Join(fmt.Errorf("link Kroot project %s: %w", spec.ID, linkErr), cleanupErr)
		}
	}
	if _, err := d.run(ctx, "exec", container, "touch", "--", readyMarker); err != nil {
		return fmt.Errorf("mark Kroot project %s ready: %w", spec.ID, err)
	}
	return nil
}

func (d Docker) DiscoverProjectApplications(ctx context.Context, e manager.Executor, projectID string) ([]manager.ProjectApplication, error) {
	if !manager.ValidUserID(projectID) {
		return nil, errors.New("invalid project id")
	}
	out, err := d.sessionRequest(ctx, d.Prefix+e.ID, "GET", "/v1/projects/"+projectID+"/apps", nil)
	if err != nil {
		return nil, err
	}
	var values []manager.ProjectApplication
	if err := json.Unmarshal(out, &values); err != nil {
		return nil, fmt.Errorf("decode executor project applications: %w", err)
	}
	return values, nil
}

func (d Docker) StopSession(ctx context.Context, e manager.Executor, sessionID string) error {
	if sessionID == "" || strings.ContainsAny(sessionID, "/\\") {
		return errors.New("invalid session id")
	}
	_, err := d.sessionRequest(ctx, d.Prefix+e.ID, "DELETE", "/v1/sessions/"+sessionID, nil)
	if err != nil && strings.Contains(err.Error(), "HTTP 404") {
		return nil
	}
	return err
}

func (d Docker) ObserveSessions(ctx context.Context, e manager.Executor) ([]manager.SessionObservation, error) {
	out, err := d.sessionRequest(ctx, d.Prefix+e.ID, "GET", "/v1/sessions", nil)
	if err != nil {
		return nil, err
	}
	var values []struct {
		ID         string `json:"id"`
		State      string `json:"state"`
		RelayState string `json:"relayState"`
		LastError  string `json:"lastError"`
	}
	if err := json.Unmarshal(out, &values); err != nil {
		return nil, fmt.Errorf("decode executor sessions: %w", err)
	}
	observations := make([]manager.SessionObservation, 0, len(values))
	for _, value := range values {
		observations = append(observations, manager.SessionObservation{ID: value.ID, State: value.State, RelayState: value.RelayState, LastError: value.LastError})
	}
	return observations, nil
}

func (d Docker) EnsurePreviewNetwork(ctx context.Context, e manager.Executor, gatewayContainer string) (string, error) {
	if strings.TrimSpace(gatewayContainer) == "" {
		gatewayContainer = d.PreviewGatewayContainer
	}
	if strings.TrimSpace(gatewayContainer) == "" {
		return "", errors.New("preview gateway container is not configured")
	}
	network := d.previewNetworkName(e)
	labels := []string{"pie.manager_id=" + d.scope(), "pie.user_id=" + e.UserID, "pie.network_purpose=preview"}
	args := []string{"network", "create", "--internal"}
	for _, label := range labels {
		args = append(args, "--label", label)
	}
	args = append(args, network)
	if _, err := d.run(ctx, args...); err != nil {
		if !dockerNameConflict(err) {
			return "", err
		}
		out, inspectErr := d.run(ctx, "network", "inspect", "--format", `{{index .Labels "pie.manager_id"}}|{{index .Labels "pie.user_id"}}|{{index .Labels "pie.network_purpose"}}`, network)
		if inspectErr != nil {
			return "", inspectErr
		}
		if strings.TrimSpace(string(out)) != d.scope()+"|"+e.UserID+"|preview" {
			return "", fmt.Errorf("existing preview network %s is not owned by this manager", network)
		}
	}
	backendAlias := d.previewBackendAlias(e)
	if err := d.connectPreviewNetwork(ctx, network, gatewayContainer, "preview-gateway"); err != nil {
		return "", err
	}
	if err := d.connectPreviewNetwork(ctx, network, d.Prefix+e.ID, backendAlias); err != nil {
		return "", err
	}
	return backendAlias, nil
}

func (d Docker) connectPreviewNetwork(ctx context.Context, network, container, alias string) error {
	_, err := d.run(ctx, "network", "connect", "--alias", alias, network, container)
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "already exists") || strings.Contains(message, "already connected") || strings.Contains(message, "endpoint with name") {
		return nil
	}
	return err
}

func (d Docker) previewNetworkName(e manager.Executor) string {
	sum := sha256.Sum256([]byte(d.scope() + "\x00" + e.UserID))
	return fmt.Sprintf("pie-preview-%x", sum[:12])
}

func (d Docker) previewBackendAlias(e manager.Executor) string {
	sum := sha256.Sum256([]byte(d.scope() + "\x00" + e.ID))
	return fmt.Sprintf("preview-backend-%x", sum[:8])
}

func (d Docker) StartPreview(ctx context.Context, e manager.Executor, spec manager.PreviewSpec) error {
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	_, err = d.sessionRequest(ctx, d.Prefix+e.ID, "POST", "/v1/previews", body)
	return err
}

func (d Docker) StopPreview(ctx context.Context, e manager.Executor, previewID string) error {
	if !manager.ValidUserID(previewID) {
		return errors.New("invalid preview id")
	}
	_, err := d.sessionRequest(ctx, d.Prefix+e.ID, "DELETE", "/v1/previews/"+previewID, nil)
	if err != nil && strings.Contains(err.Error(), "HTTP 404") {
		return nil
	}
	return err
}

func (d Docker) ObservePreviews(ctx context.Context, e manager.Executor) ([]manager.PreviewObservation, error) {
	out, err := d.sessionRequest(ctx, d.Prefix+e.ID, "GET", "/v1/previews", nil)
	if err != nil {
		return nil, err
	}
	var values []struct {
		ID        string `json:"id"`
		State     string `json:"state"`
		Ready     bool   `json:"ready"`
		LastError string `json:"lastError"`
	}
	if err := json.Unmarshal(out, &values); err != nil {
		return nil, fmt.Errorf("decode executor previews: %w", err)
	}
	observations := make([]manager.PreviewObservation, 0, len(values))
	for _, value := range values {
		observations = append(observations, manager.PreviewObservation{ID: value.ID, State: value.State, Ready: value.Ready, LastError: value.LastError})
	}
	return observations, nil
}

func (d Docker) PreviewLogs(ctx context.Context, e manager.Executor, previewID string) ([]byte, error) {
	if !manager.ValidUserID(previewID) {
		return nil, errors.New("invalid preview id")
	}
	return d.sessionRequest(ctx, d.Prefix+e.ID, "GET", "/v1/previews/"+previewID+"/logs", nil)
}

func (d Docker) sessionRequest(ctx context.Context, container, method, path string, body []byte) ([]byte, error) {
	if d.SessionCommand != nil {
		return d.SessionCommand(ctx, container, method, path, body)
	}
	cmd := exec.CommandContext(ctx, "docker", sessionRequestArgs(container, method, path, len(body) > 0)...)
	if len(body) > 0 {
		cmd.Stdin = bytes.NewReader(body)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker session request %s %s: %w: %s", method, path, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func sessionRequestArgs(container, method, path string, hasBody bool) []string {
	args := []string{"exec"}
	if hasBody {
		args = append(args, "-i")
	}
	return append(args,
		container,
		"timeout", sessionRequestProcessTimeout.String(),
		"pie-client", "sessions", "request",
		"--timeout", "15s", "--method", method, "--path", path,
	)
}

func (d Docker) run(ctx context.Context, args ...string) ([]byte, error) {
	if d.Command != nil {
		return d.Command(ctx, args...)
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (d Docker) scope() string {
	if d.Scope == "" {
		return "default"
	}
	return d.Scope
}

var _ manager.Runtime = Docker{}
var _ manager.RuntimeObserver = Docker{}
var _ manager.SessionRuntime = Docker{}
var _ manager.ProjectRuntime = Docker{}
var _ manager.ProjectApplicationRuntime = Docker{}
var _ manager.PreviewRuntime = Docker{}
