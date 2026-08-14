package previewprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

var (
	ErrExists   = errors.New("preview already exists with different configuration")
	ErrNotFound = errors.New("preview not found")
	ErrLimit    = errors.New("preview limit reached")
)

const maxLogBytes = 1 << 20
const maxPackageJSONBytes = 1 << 20
const maxDiscoveryDepth = 5
const maxDiscoveryDirectories = 2048
const maxDiscoveredApplications = 64

type Config struct {
	ID         string `json:"id"`
	ProjectID  string `json:"projectId"`
	WorkingDir string `json:"workingDir"`
	Hostname   string `json:"hostname"`
	Profile    string `json:"profile"`
	Port       int    `json:"port"`
}

type Status struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"projectId"`
	WorkingDir string     `json:"workingDir"`
	Profile    string     `json:"profile"`
	Port       int        `json:"port"`
	State      string     `json:"state"`
	Ready      bool       `json:"ready"`
	StartedAt  time.Time  `json:"startedAt"`
	StoppedAt  *time.Time `json:"stoppedAt,omitempty"`
	LastError  string     `json:"lastError,omitempty"`
}

// Application is a runnable web application discovered inside one opaque
// project directory. Path is always project-relative; the absolute container
// path is deliberately never returned to callers.
type Application struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Profile string `json:"profile"`
}

type process struct {
	config Config
	status Status
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan struct{}
	logs   *logBuffer
}

type Manager struct {
	ctx      context.Context
	limit    int
	root     string
	probe    func(context.Context, int) error
	mu       sync.RWMutex
	previews map[string]*process
}

func New(ctx context.Context, workspaceRoot string, limit int) *Manager {
	if limit < 1 {
		limit = 8
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		workspaceRoot = "/workspace/projects"
	}
	return &Manager{
		ctx:      ctx,
		limit:    limit,
		root:     filepath.Clean(workspaceRoot),
		probe:    probePort,
		previews: map[string]*process{},
	}
}

// DiscoverApplications finds bounded package.json files that define a dev
// script. It never follows directory symlinks and skips dependency/build
// trees, so discovery cannot escape the selected project or scan an
// unbounded node_modules tree.
func (m *Manager) DiscoverApplications(projectID string) ([]Application, error) {
	if !validID(projectID) {
		return nil, errors.New("invalid project id")
	}
	projectRoot := filepath.Join(m.root, projectID)
	info, err := os.Lstat(projectRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("preview project directory does not exist")
	}
	projectReal, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		return nil, errors.New("preview project directory cannot be resolved")
	}
	directories := 0
	applications := make([]Application, 0, 4)
	var visit func(string, string, int) error
	visit = func(directory, relative string, depth int) error {
		directories++
		if directories > maxDiscoveryDirectories {
			return errors.New("preview application discovery directory limit exceeded")
		}
		entries, readErr := os.ReadDir(directory)
		if readErr != nil {
			if depth > 0 && (errors.Is(readErr, os.ErrPermission) || errors.Is(readErr, os.ErrNotExist)) {
				return nil
			}
			return fmt.Errorf("read preview project directory: %w", readErr)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				if depth >= maxDiscoveryDepth || skipDiscoveryDirectory(name) {
					continue
				}
				if entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				nextRelative := name
				if relative != "." {
					nextRelative = filepath.Join(relative, name)
				}
				if err := visit(filepath.Join(directory, name), nextRelative, depth+1); err != nil {
					return err
				}
				continue
			}
			if name != "package.json" || !entry.Type().IsRegular() {
				continue
			}
			workingReal, resolveErr := filepath.EvalSymlinks(directory)
			if resolveErr != nil {
				continue
			}
			realRelative, relativeErr := filepath.Rel(projectReal, workingReal)
			if relativeErr != nil || realRelative == ".." || strings.HasPrefix(realRelative, ".."+string(filepath.Separator)) {
				continue
			}
			manifest, manifestErr := readPackageManifest(directory)
			if manifestErr != nil || strings.TrimSpace(manifest.Scripts["dev"]) == "" {
				continue
			}
			if len(applications) >= maxDiscoveredApplications {
				return errors.New("preview application discovery result limit exceeded")
			}
			appPath := filepath.ToSlash(relative)
			if appPath == "" {
				appPath = "."
			}
			if !validDiscoveredApplicationPath(appPath) {
				continue
			}
			applications = append(applications, Application{
				Path: appPath, Name: applicationName(manifest.Name, appPath), Profile: detectProfile(manifest),
			})
		}
		return nil
	}
	if err := visit(projectRoot, ".", 0); err != nil {
		return nil, err
	}
	sort.Slice(applications, func(left, right int) bool {
		if applications[left].Path == applications[right].Path {
			return false
		}
		if applications[left].Path == "." {
			return true
		}
		if applications[right].Path == "." {
			return false
		}
		return applications[left].Path < applications[right].Path
	})
	return applications, nil
}

func validDiscoveredApplicationPath(value string) bool {
	if value == "." {
		return true
	}
	if value == "" || len(value) > 512 || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func skipDiscoveryDirectory(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "dist", "build", "out", "coverage", "vendor":
		return true
	}
	return false
}

func applicationName(value, appPath string) string {
	value = strings.TrimSpace(value)
	valid := value != "" && len([]rune(value)) <= 120
	for _, character := range value {
		if unicode.IsControl(character) {
			valid = false
			break
		}
	}
	if valid {
		return value
	}
	if appPath == "." {
		return "프로젝트 웹 앱"
	}
	return filepath.Base(filepath.FromSlash(appPath))
}

func (m *Manager) Start(config Config) (Status, bool, error) {
	if err := m.validate(config); err != nil {
		return Status{}, false, err
	}
	command, args, profile, err := resolveCommand(config)
	if err != nil {
		return Status{}, false, err
	}
	config.Profile = profile
	m.mu.Lock()
	if existing := m.previews[config.ID]; existing != nil {
		if sameConfig(existing.config, config) && (existing.status.State == "starting" || existing.status.State == "running") {
			status := existing.status
			m.mu.Unlock()
			return status, true, nil
		}
		if existing.status.State == "starting" || existing.status.State == "running" {
			m.mu.Unlock()
			return Status{}, false, ErrExists
		}
		delete(m.previews, config.ID)
	}
	active := 0
	for _, value := range m.previews {
		if value.status.State == "starting" || value.status.State == "running" {
			active++
		}
	}
	if active >= m.limit {
		m.mu.Unlock()
		return Status{}, false, ErrLimit
	}
	ctx, cancel := context.WithCancel(m.ctx)
	cmd := exec.Command(command, args...)
	cmd.Dir = config.WorkingDir
	cmd.Env = previewEnvironment(os.Environ(), config.Port, config.Hostname)
	configureProcessGroup(cmd)
	logs := &logBuffer{limit: maxLogBytes}
	cmd.Stdout, cmd.Stderr = logs, logs
	now := time.Now().UTC()
	value := &process{
		config: config,
		status: Status{ID: config.ID, ProjectID: config.ProjectID, WorkingDir: config.WorkingDir, Profile: profile, Port: config.Port, State: "starting", StartedAt: now},
		cmd:    cmd, cancel: cancel, done: make(chan struct{}), logs: logs,
	}
	m.previews[config.ID] = value
	if err := cmd.Start(); err != nil {
		delete(m.previews, config.ID)
		m.mu.Unlock()
		cancel()
		return Status{}, false, fmt.Errorf("start preview process: %w", err)
	}
	status := value.status
	m.mu.Unlock()
	go m.monitor(ctx, value)
	return status, false, nil
}

func (m *Manager) monitor(ctx context.Context, value *process) {
	exit := make(chan error, 1)
	go func() { exit <- value.cmd.Wait() }()
	probeTicker := time.NewTicker(250 * time.Millisecond)
	defer probeTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = signalProcess(value.cmd)
			var err error
			select {
			case err = <-exit:
			case <-time.After(3 * time.Second):
				_ = killProcess(value.cmd)
				err = <-exit
			}
			m.finish(value, "stopped", false, cancellationError(err))
			return
		case err := <-exit:
			state := "stopped"
			if err != nil {
				state = "failed"
			}
			m.finish(value, state, false, err)
			return
		case <-probeTicker.C:
			probeCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			err := m.probe(probeCtx, value.config.Port)
			cancel()
			if err == nil {
				m.mu.Lock()
				if value.status.State == "starting" {
					value.status.State = "running"
					value.status.Ready = true
				}
				m.mu.Unlock()
			}
		}
	}
}

func (m *Manager) finish(value *process, state string, ready bool, err error) {
	now := time.Now().UTC()
	m.mu.Lock()
	value.status.State = state
	value.status.Ready = ready
	value.status.StoppedAt = &now
	if err != nil {
		value.status.LastError = bounded(err.Error(), 2000)
	}
	close(value.done)
	m.mu.Unlock()
}

func (m *Manager) Stop(ctx context.Context, id string) (Status, error) {
	m.mu.RLock()
	value := m.previews[id]
	m.mu.RUnlock()
	if value == nil {
		return Status{}, ErrNotFound
	}
	value.cancel()
	select {
	case <-value.done:
	case <-ctx.Done():
		return Status{}, ctx.Err()
	}
	m.mu.RLock()
	status := value.status
	m.mu.RUnlock()
	return status, nil
}

func (m *Manager) Get(id string) (Status, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	value := m.previews[id]
	if value == nil {
		return Status{}, false
	}
	return value.status, true
}

func (m *Manager) List() []Status {
	m.mu.RLock()
	out := make([]Status, 0, len(m.previews))
	for _, value := range m.previews {
		out = append(out, value.status)
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

func (m *Manager) Logs(id string, tailBytes int) ([]byte, error) {
	m.mu.RLock()
	value := m.previews[id]
	m.mu.RUnlock()
	if value == nil {
		return nil, ErrNotFound
	}
	return value.logs.Tail(tailBytes), nil
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.RLock()
	values := make([]*process, 0, len(m.previews))
	for _, value := range m.previews {
		values = append(values, value)
	}
	m.mu.RUnlock()
	for _, value := range values {
		value.cancel()
	}
	for _, value := range values {
		select {
		case <-value.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (m *Manager) validate(config Config) error {
	if !validID(config.ID) || !validID(config.ProjectID) || !validHostname(config.Hostname) || config.Port < 1024 || config.Port > 65535 {
		return errors.New("invalid preview id, project id, or port")
	}
	projectRoot := filepath.Join(m.root, config.ProjectID)
	workingDir := filepath.Clean(config.WorkingDir)
	relative, err := filepath.Rel(projectRoot, workingDir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("preview working directory must stay inside its selected project")
	}
	projectInfo, err := os.Stat(projectRoot)
	if err != nil || !projectInfo.IsDir() {
		return errors.New("preview project directory does not exist")
	}
	projectReal, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		return errors.New("preview project directory cannot be resolved")
	}
	workingReal, err := filepath.EvalSymlinks(workingDir)
	if err != nil {
		return errors.New("preview application path does not exist")
	}
	realRelative, err := filepath.Rel(projectReal, workingReal)
	if err != nil || realRelative == ".." || strings.HasPrefix(realRelative, ".."+string(filepath.Separator)) {
		return errors.New("preview application path escapes its selected project")
	}
	info, err := os.Stat(workingReal)
	if err != nil || !info.IsDir() {
		return errors.New("preview application path is not a directory")
	}
	return nil
}

func resolveCommand(config Config) (string, []string, string, error) {
	manifest, err := readPackageManifest(config.WorkingDir)
	if err != nil {
		return "", nil, "", err
	}
	if strings.TrimSpace(manifest.Scripts["dev"]) == "" {
		return "", nil, "", errors.New("selected preview application has no package.json scripts.dev command")
	}
	profile := strings.TrimSpace(config.Profile)
	if profile == "" || profile == "auto" {
		profile = detectProfile(manifest)
	}
	port := strconv.Itoa(config.Port)
	if launcher := strings.TrimSpace(os.Getenv("PIE_PREVIEW_LAUNCHER_PATH")); launcher != "" {
		info, statErr := os.Stat(launcher)
		if statErr != nil || info.IsDir() {
			return "", nil, "", fmt.Errorf("preview launcher is unavailable: %s", launcher)
		}
		switch profile {
		case "next", "vite", "npm":
			return "node", []string{launcher, "--profile", profile, "--port", port}, profile, nil
		default:
			return "", nil, "", fmt.Errorf("unsupported preview profile %q", profile)
		}
	}
	switch profile {
	case "next":
		return "npm", []string{"run", "dev", "--", "--hostname", "0.0.0.0", "--port", port}, profile, nil
	case "vite":
		return "npm", []string{"run", "dev", "--", "--host", "0.0.0.0", "--port", port}, profile, nil
	case "npm":
		return "npm", []string{"run", "dev"}, profile, nil
	default:
		return "", nil, "", fmt.Errorf("unsupported preview profile %q", profile)
	}
}

type packageManifest struct {
	Name            string                     `json:"name"`
	Scripts         map[string]string          `json:"scripts"`
	Dependencies    map[string]json.RawMessage `json:"dependencies"`
	DevDependencies map[string]json.RawMessage `json:"devDependencies"`
}

func readPackageManifest(workingDir string) (packageManifest, error) {
	path := filepath.Join(workingDir, "package.json")
	info, err := os.Lstat(path)
	if err != nil {
		return packageManifest{}, errors.New("selected preview application has no package.json")
	}
	if !info.Mode().IsRegular() || info.Size() > maxPackageJSONBytes {
		return packageManifest{}, errors.New("selected preview application package.json is not a bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return packageManifest{}, errors.New("selected preview application package.json cannot be read")
	}
	var manifest packageManifest
	if json.Unmarshal(data, &manifest) != nil {
		return packageManifest{}, errors.New("selected preview application package.json is invalid")
	}
	return manifest, nil
}

func detectProfile(manifest packageManifest) string {
	dev := strings.ToLower(manifest.Scripts["dev"])
	if _, ok := manifest.Dependencies["next"]; ok {
		return "next"
	}
	if _, ok := manifest.DevDependencies["next"]; ok {
		return "next"
	}
	if strings.Contains(dev, "next") {
		return "next"
	}
	if _, ok := manifest.Dependencies["vite"]; ok {
		return "vite"
	}
	if _, ok := manifest.DevDependencies["vite"]; ok {
		return "vite"
	}
	if strings.Contains(dev, "vite") {
		return "vite"
	}
	return "npm"
}

func sameConfig(left, right Config) bool {
	return left.ID == right.ID && left.ProjectID == right.ProjectID && left.WorkingDir == right.WorkingDir && left.Hostname == right.Hostname && left.Profile == right.Profile && left.Port == right.Port
}

func previewEnvironment(source []string, port int, hostname string) []string {
	out := make([]string, 0, len(source)+3)
	for _, entry := range source {
		key, _, _ := strings.Cut(entry, "=")
		if key == "PORT" || key == "HOST" || key == "__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS" {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "PORT="+strconv.Itoa(port), "HOST=0.0.0.0", "__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS="+hostname)
}

func validID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func validHostname(value string) bool {
	if value == "" || len(value) > 253 || strings.Contains(value, "..") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func probePort(ctx context.Context, port int) error {
	dialer := net.Dialer{Timeout: 200 * time.Millisecond}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return connection.Close()
}

func cancellationError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

type logBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (b *logBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	b.data = append(b.data, value...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	b.mu.Unlock()
	return len(value), nil
}

func (b *logBuffer) Tail(limit int) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit < 1 || limit > b.limit {
		limit = b.limit
	}
	start := len(b.data) - limit
	if start < 0 {
		start = 0
	}
	return bytes.Clone(b.data[start:])
}

var _ io.Writer = (*logBuffer)(nil)
