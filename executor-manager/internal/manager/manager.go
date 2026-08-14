package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrQueueFull = errors.New("executor queue is full")
var ErrUserBusy = errors.New("user already has a running job")
var ErrJobNotCancelable = errors.New("job is not cancelable")
var ErrInputTooLarge = errors.New("job input exceeds limit")
var ErrInvalidUserID = errors.New("invalid user id")
var ErrExecutorUnavailable = errors.New("executor unavailable")
var ErrExecutorNotFound = errors.New("executor runtime not found")
var ErrInvalidExecutorLimits = errors.New("invalid executor resource limits")
var ErrExecutorCapacity = errors.New("executor node capacity reached")
var ErrExecutorDiskQuota = errors.New("executor disk quota exceeded")
var ErrExecutorDiskCapacity = errors.New("executor node disk headroom exhausted")

const maxJobLogBytes = 4 << 20

type Executor struct {
	UserID         string     `json:"userId"`
	ID             string     `json:"id"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastUsedAt     time.Time  `json:"lastUsedAt"`
	CPUs           string     `json:"cpus,omitempty"`
	MemoryBytes    int64      `json:"memoryBytes,omitempty"`
	PIDsLimit      int64      `json:"pidsLimit,omitempty"`
	DiskBytes      int64      `json:"diskBytes,omitempty"`
	DiskLimitBytes int64      `json:"diskLimitBytes,omitempty"`
	DiskUsedBytes  int64      `json:"diskUsedBytes,omitempty"`
	DiskCheckedAt  *time.Time `json:"diskCheckedAt,omitempty"`
}

type ExecutorLimits struct {
	CPUs        string
	MemoryBytes int64
	PIDsLimit   int64
	DiskBytes   int64
}
type Job struct {
	ID           string     `json:"id"`
	UserID       string     `json:"userId"`
	ExecutorID   string     `json:"executorId"`
	Status       string     `json:"status"`
	CreatedAt    *time.Time `json:"createdAt,omitempty"`
	StartedAt    *time.Time `json:"startedAt,omitempty"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	Input        []byte     `json:"input,omitempty"`
	Output       []byte     `json:"output,omitempty"`
	BlobRefs     []string   `json:"blobRefs,omitempty"`
	Logs         []string   `json:"logs,omitempty"`
	LogBytes     int        `json:"logBytes,omitempty"`
	LogTruncated bool       `json:"logTruncated,omitempty"`
	Err          string     `json:"error,omitempty"`
}

// UnmarshalJSON keeps the pre-camelCase registry compatible. Most legacy
// field names match the new tags case-insensitively; Err is the one exception
// because the public API now calls it "error".
func (j *Job) UnmarshalJSON(data []byte) error {
	type jobAlias Job
	var value jobAlias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value.Err == "" {
		var legacy struct {
			Err string `json:"Err"`
		}
		if err := json.Unmarshal(data, &legacy); err != nil {
			return err
		}
		value.Err = legacy.Err
	}
	*j = Job(value)
	return nil
}

type Runtime interface {
	Ensure(context.Context, Executor) error
	Run(context.Context, Job) ([]byte, error)
	Stop(context.Context, Executor) error
}
type RuntimeObservation struct {
	RuntimeID string
	Image     string
	Status    string
	Health    string
	Running   bool
	Drifted   bool
}
type RuntimeObserver interface {
	Observe(context.Context, Executor) (RuntimeObservation, error)
}
type RuntimeReconciler interface {
	// Reconcile repairs persistent runtime prerequisites that may drift while
	// the process itself is still running, such as bind-mount ownership.
	Reconcile(context.Context, Executor) error
}

type ClaudeAuthRuntime interface {
	RestartForClaudeAuth(context.Context, Executor) error
}
type StorageObservation struct {
	UsedBytes  int64
	LimitBytes int64
	WorkBytes  int64
	StateBytes int64
	BlobBytes  int64
	FreeBytes  int64
}
type StorageRuntime interface {
	ObserveStorage(context.Context, Executor) (StorageObservation, error)
}
type SessionSpec struct {
	ID                string `json:"id"`
	AgentID           string `json:"agentId,omitempty"`
	AgentMode         string `json:"agentMode,omitempty"`
	RelayURL          string `json:"relayUrl"`
	Token             string `json:"token"`
	StreamID          string `json:"streamId,omitempty"`
	WorkingDir        string `json:"workingDir,omitempty"`
	InitialDriver     string `json:"initialDriver,omitempty"`
	ClaudeOAuthToken  string `json:"claudeOAuthToken,omitempty"`
	ClaudeAuthVersion string `json:"claudeAuthVersion,omitempty"`
}
type SessionObservation struct {
	ID         string
	State      string
	RelayState string
	LastError  string
}
type SessionRuntime interface {
	StartSession(context.Context, Executor, SessionSpec) error
	StopSession(context.Context, Executor, string) error
	ObserveSessions(context.Context, Executor) ([]SessionObservation, error)
}
type ProjectSpec struct {
	ID     string
	Name   string
	Locale string
}
type ProjectRuntime interface {
	InitializeProject(context.Context, Executor, ProjectSpec) error
}

type ProjectApplication struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Profile string `json:"profile"`
}

type ProjectApplicationRuntime interface {
	DiscoverProjectApplications(context.Context, Executor, string) ([]ProjectApplication, error)
}

type PreviewSpec struct {
	ID         string `json:"id"`
	ProjectID  string `json:"projectId"`
	WorkingDir string `json:"workingDir"`
	Hostname   string `json:"hostname"`
	Profile    string `json:"profile"`
	Port       int    `json:"port"`
}

type PreviewObservation struct {
	ID        string
	State     string
	Ready     bool
	LastError string
}

// PreviewRuntime keeps preview process control inside clientd and Docker
// network control inside the privileged Manager runtime. Public gateways never
// receive access to the Docker socket.
type PreviewRuntime interface {
	EnsurePreviewNetwork(context.Context, Executor, string) (string, error)
	StartPreview(context.Context, Executor, PreviewSpec) error
	StopPreview(context.Context, Executor, string) error
	ObservePreviews(context.Context, Executor) ([]PreviewObservation, error)
	PreviewLogs(context.Context, Executor, string) ([]byte, error)
}
type StreamRuntime interface {
	RunStream(context.Context, Job, func([]byte)) ([]byte, error)
}
type Store interface {
	Load(context.Context) ([]Executor, error)
	LoadJobs(context.Context) ([]Job, error)
	SaveExecutor(context.Context, Executor) error
	SaveJob(context.Context, Job) error
}
type JobDeleter interface {
	DeleteJobs(context.Context, []string) error
}

type Manager struct {
	mu             sync.RWMutex
	executors      map[string]Executor
	jobs           map[string]Job
	queue          chan string
	runtime        Runtime
	store          Store
	stop           context.CancelFunc
	running        map[string]string
	cancel         map[string]context.CancelFunc
	jobTimeout     time.Duration
	wg             sync.WaitGroup
	closeOnce      sync.Once
	provisioning   map[string]*provisionCall
	provisionSlots chan struct{}
	subscribers    map[string]map[chan struct{}]struct{}
	terminalOrder  []string
	retainedJobs   int
	maxExecutors   int
	diskScanErrors uint64
	diskFreeBytes  int64
}

type provisionCall struct {
	done     chan struct{}
	executor Executor
	err      error
}

type Options struct {
	QueueCapacity        int
	Workers              int
	JobTimeout           time.Duration
	ProvisionConcurrency int
	RetainedJobs         int
	MaxExecutors         int
	StorageScanInterval  time.Duration
}
type Stats struct {
	Executors         int
	ActiveExecutors   int
	ExecutorCapacity  int
	Jobs              map[string]int
	QueueDepth        int
	QueueCapacity     int
	Running           int
	ActiveUsers       int
	Provisioning      int
	DiskUsedBytes     int64
	DiskQuotaBytes    int64
	DiskQuotaExceeded int
	DiskScanErrors    uint64
	DiskFreeBytes     int64
}

func New(ctx context.Context, runtime Runtime, store Store, capacity int) (*Manager, error) {
	return NewWithOptions(ctx, runtime, store, Options{QueueCapacity: capacity, Workers: 1, JobTimeout: 30 * time.Minute, ProvisionConcurrency: capacity})
}

func NewWithOptions(ctx context.Context, runtime Runtime, store Store, options Options) (*Manager, error) {
	if runtime == nil || store == nil {
		return nil, errors.New("runtime and store are required")
	}
	if options.QueueCapacity < 1 || options.Workers < 1 {
		return nil, errors.New("queue capacity must be positive")
	}
	if options.ProvisionConcurrency < 0 {
		return nil, errors.New("provision concurrency cannot be negative")
	}
	if options.ProvisionConcurrency == 0 {
		options.ProvisionConcurrency = options.Workers
	}
	if options.RetainedJobs < 0 {
		return nil, errors.New("retained jobs cannot be negative")
	}
	if options.MaxExecutors < 0 {
		return nil, errors.New("max executors cannot be negative")
	}
	if options.RetainedJobs == 0 {
		options.RetainedJobs = 1000
	}
	if options.JobTimeout <= 0 {
		options.JobTimeout = 30 * time.Minute
	}
	execs, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	jobs, err := store.LoadJobs(ctx)
	if err != nil {
		return nil, err
	}
	m := &Manager{executors: map[string]Executor{}, jobs: map[string]Job{}, queue: make(chan string, options.QueueCapacity), runtime: runtime, store: store, running: map[string]string{}, cancel: map[string]context.CancelFunc{}, jobTimeout: options.JobTimeout, provisioning: map[string]*provisionCall{}, provisionSlots: make(chan struct{}, options.ProvisionConcurrency), subscribers: map[string]map[chan struct{}]struct{}{}, retainedJobs: options.RetainedJobs, maxExecutors: options.MaxExecutors}
	for _, e := range execs {
		m.executors[e.UserID] = e
	}
	for _, j := range jobs {
		if j.Status == "queued" || j.Status == "running" {
			j.Status = "failed"
			j.Err = "manager restarted before job completion"
			now := time.Now().UTC()
			j.FinishedAt = &now
			if err := store.SaveJob(ctx, j); err != nil {
				return nil, err
			}
		}
		m.jobs[j.ID] = j
		if terminal(j.Status) {
			m.terminalOrder = append(m.terminalOrder, j.ID)
		}
	}
	sort.SliceStable(m.terminalOrder, func(i, k int) bool {
		return jobFinishedAt(m.jobs[m.terminalOrder[i]]).Before(jobFinishedAt(m.jobs[m.terminalOrder[k]]))
	})
	removed := m.pruneLocked()
	if err := deleteJobs(ctx, store, removed); err != nil {
		return nil, err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	m.stop = cancel
	for range options.Workers {
		m.wg.Add(1)
		go func() { defer m.wg.Done(); m.worker(workerCtx) }()
	}
	if options.StorageScanInterval > 0 {
		m.wg.Add(1)
		go func() { defer m.wg.Done(); m.storageLoop(workerCtx, options.StorageScanInterval) }()
	}
	return m, nil
}
func (m *Manager) Close() { m.closeOnce.Do(func() { m.stop(); m.wg.Wait() }) }
func (m *Manager) Ensure(ctx context.Context, userID string) (Executor, error) {
	return m.ensure(ctx, userID, nil)
}

func (m *Manager) EnsureWithLimits(ctx context.Context, userID string, limits ExecutorLimits) (Executor, error) {
	if !validExecutorLimits(limits) {
		return Executor{}, ErrInvalidExecutorLimits
	}
	return m.ensure(ctx, userID, &limits)
}

func (m *Manager) ensure(ctx context.Context, userID string, limits *ExecutorLimits) (Executor, error) {
	if !ValidUserID(userID) {
		return Executor{}, ErrInvalidUserID
	}
	m.mu.Lock()
	if e, ok := m.executors[userID]; ok && e.Status == "ready" {
		limitsChanged := limits != nil && !executorHasLimits(e, *limits)
		m.mu.Unlock()
		if reconciler, supported := m.runtime.(RuntimeReconciler); supported {
			if err := reconciler.Reconcile(ctx, e); err != nil {
				if errors.Is(err, ErrExecutorDiskQuota) {
					m.markExecutorStatus(e, "quota_exceeded")
				}
				return Executor{}, err
			}
		}
		if observer, supported := m.runtime.(RuntimeObserver); supported {
			observation, err := observer.Observe(ctx, e)
			if err == nil && observation.Running && !observation.Drifted && !limitsChanged {
				return e, nil
			}
			if err != nil && !errors.Is(err, ErrExecutorNotFound) {
				return Executor{}, err
			}
			// Registry says ready but the runtime disappeared or stopped. Fall
			// through to the normal idempotent Ensure path and repair it.
			m.mu.Lock()
		} else if !limitsChanged {
			return e, nil
		} else {
			m.mu.Lock()
		}
	}
	if call := m.provisioning[userID]; call != nil {
		done := call.done
		m.mu.Unlock()
		select {
		case <-done:
			return call.executor, call.err
		case <-ctx.Done():
			return Executor{}, ctx.Err()
		}
	}
	now := time.Now().UTC()
	e, exists := m.executors[userID]
	if (!exists || !executorOccupiesCapacity(e)) && m.maxExecutors > 0 && m.activeExecutorsLocked() >= m.maxExecutors {
		m.mu.Unlock()
		return Executor{}, ErrExecutorCapacity
	}
	if !exists {
		e = Executor{UserID: userID, ID: "executor-" + userID, CreatedAt: now}
	}
	if limits != nil {
		e.CPUs, e.MemoryBytes, e.PIDsLimit, e.DiskBytes = limits.CPUs, limits.MemoryBytes, limits.PIDsLimit, limits.DiskBytes
		e.DiskLimitBytes = limits.DiskBytes
	}
	e.Status = "provisioning"
	e.LastUsedAt = now
	call := &provisionCall{done: make(chan struct{})}
	m.provisioning[userID] = call
	m.executors[userID] = e
	m.mu.Unlock()
	if err := m.store.SaveExecutor(ctx, e); err != nil {
		return m.finishProvision(userID, e, err)
	}
	select {
	case m.provisionSlots <- struct{}{}:
		defer func() { <-m.provisionSlots }()
	case <-ctx.Done():
		e.Status = "failed"
		_ = m.store.SaveExecutor(context.Background(), e)
		return m.finishProvision(userID, e, ctx.Err())
	}
	if err := m.runtime.Ensure(ctx, e); err != nil {
		if errors.Is(err, ErrExecutorDiskQuota) {
			e.Status = "quota_exceeded"
		} else {
			e.Status = "failed"
		}
		_ = m.store.SaveExecutor(context.Background(), e)
		return m.finishProvision(userID, e, err)
	}
	e.Status = "ready"
	if err := m.store.SaveExecutor(ctx, e); err != nil {
		return m.finishProvision(userID, e, err)
	}
	return m.finishProvision(userID, e, nil)
}

func (m *Manager) markExecutorStatus(executor Executor, status string) {
	executor.Status = status
	m.mu.Lock()
	if current, ok := m.executors[executor.UserID]; ok && current.ID == executor.ID {
		current.Status = status
		executor = current
		m.executors[executor.UserID] = current
	}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.store.SaveExecutor(ctx, executor)
}

func executorOccupiesCapacity(executor Executor) bool {
	return executor.Status != "stopped" && executor.Status != "quota_exceeded"
}

func (m *Manager) activeExecutorsLocked() int {
	active := 0
	for _, executor := range m.executors {
		if executorOccupiesCapacity(executor) {
			active++
		}
	}
	return active
}

func executorHasLimits(executor Executor, limits ExecutorLimits) bool {
	return executor.CPUs == limits.CPUs && executor.MemoryBytes == limits.MemoryBytes && executor.PIDsLimit == limits.PIDsLimit && executor.DiskBytes == limits.DiskBytes
}

func validExecutorLimits(limits ExecutorLimits) bool {
	if limits.MemoryBytes < 0 || limits.PIDsLimit < 0 || limits.DiskBytes < 0 {
		return false
	}
	if limits.CPUs == "" {
		return true
	}
	value, err := strconv.ParseFloat(limits.CPUs, 64)
	return err == nil && value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (m *Manager) Executor(userID string) (Executor, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.executors[userID]
	return e, ok
}

func (m *Manager) Observe(ctx context.Context, userID string) (RuntimeObservation, error) {
	m.mu.RLock()
	e, ok := m.executors[userID]
	m.mu.RUnlock()
	if !ok {
		return RuntimeObservation{}, ErrExecutorNotFound
	}
	observer, ok := m.runtime.(RuntimeObserver)
	if !ok {
		return RuntimeObservation{}, errors.New("runtime observation is not supported")
	}
	return observer.Observe(ctx, e)
}

func (m *Manager) StopExecutor(ctx context.Context, userID string) (Executor, error) {
	if !ValidUserID(userID) {
		return Executor{}, ErrInvalidUserID
	}
	m.mu.RLock()
	e, ok := m.executors[userID]
	m.mu.RUnlock()
	if !ok {
		return Executor{}, ErrExecutorNotFound
	}
	if err := m.runtime.Stop(ctx, e); err != nil && !errors.Is(err, ErrExecutorNotFound) {
		return Executor{}, err
	}
	e.Status = "stopped"
	e.LastUsedAt = time.Now().UTC()
	if err := m.store.SaveExecutor(ctx, e); err != nil {
		return Executor{}, err
	}
	m.mu.Lock()
	m.executors[userID] = e
	m.mu.Unlock()
	return e, nil
}

func (m *Manager) StartSession(ctx context.Context, userID string, spec SessionSpec) error {
	e, err := m.Ensure(ctx, userID)
	if err != nil {
		return err
	}
	runtime, ok := m.runtime.(SessionRuntime)
	if !ok {
		return errors.New("runtime session control is not supported")
	}
	return runtime.StartSession(ctx, e, spec)
}

func (m *Manager) RestartForClaudeAuth(ctx context.Context, userID string) error {
	m.mu.RLock()
	e, ok := m.executors[userID]
	m.mu.RUnlock()
	if !ok {
		return ErrExecutorNotFound
	}
	runtime, ok := m.runtime.(ClaudeAuthRuntime)
	if !ok {
		return errors.New("runtime Claude authentication control is not supported")
	}
	return runtime.RestartForClaudeAuth(ctx, e)
}

func (m *Manager) EnsurePreviewNetwork(ctx context.Context, userID, gatewayContainer string) (string, error) {
	runtime, ok := m.runtime.(PreviewRuntime)
	if !ok {
		return "", errors.New("preview runtime is not supported")
	}
	e, err := m.Ensure(ctx, userID)
	if err != nil {
		return "", err
	}
	return runtime.EnsurePreviewNetwork(ctx, e, gatewayContainer)
}

func (m *Manager) StartPreview(ctx context.Context, userID string, spec PreviewSpec) error {
	runtime, ok := m.runtime.(PreviewRuntime)
	if !ok {
		return errors.New("preview runtime is not supported")
	}
	e, err := m.Ensure(ctx, userID)
	if err != nil {
		return err
	}
	return runtime.StartPreview(ctx, e, spec)
}

func (m *Manager) StopPreview(ctx context.Context, userID, previewID string) error {
	m.mu.RLock()
	e, ok := m.executors[userID]
	m.mu.RUnlock()
	if !ok {
		return ErrExecutorNotFound
	}
	runtime, ok := m.runtime.(PreviewRuntime)
	if !ok {
		return errors.New("preview runtime is not supported")
	}
	return runtime.StopPreview(ctx, e, previewID)
}

func (m *Manager) ObservePreviews(ctx context.Context, userID string) ([]PreviewObservation, error) {
	m.mu.RLock()
	e, ok := m.executors[userID]
	m.mu.RUnlock()
	if !ok || e.Status != "ready" {
		return nil, ErrExecutorNotFound
	}
	runtime, ok := m.runtime.(PreviewRuntime)
	if !ok {
		return nil, errors.New("preview runtime is not supported")
	}
	return runtime.ObservePreviews(ctx, e)
}

func (m *Manager) PreviewLogs(ctx context.Context, userID, previewID string) ([]byte, error) {
	m.mu.RLock()
	e, ok := m.executors[userID]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrExecutorNotFound
	}
	runtime, ok := m.runtime.(PreviewRuntime)
	if !ok {
		return nil, errors.New("preview runtime is not supported")
	}
	return runtime.PreviewLogs(ctx, e, previewID)
}

func (m *Manager) InitializeProject(ctx context.Context, userID string, spec ProjectSpec) error {
	if !ValidUserID(userID) || !ValidUserID(spec.ID) || strings.TrimSpace(spec.Name) == "" {
		return ErrInvalidUserID
	}
	e, err := m.Ensure(ctx, userID)
	if err != nil {
		return err
	}
	runtime, ok := m.runtime.(ProjectRuntime)
	if !ok {
		return errors.New("runtime project initialization is not supported")
	}
	return runtime.InitializeProject(ctx, e, spec)
}

func (m *Manager) DiscoverProjectApplications(ctx context.Context, userID, projectID string) ([]ProjectApplication, error) {
	if !ValidUserID(userID) || !ValidUserID(projectID) {
		return nil, ErrInvalidUserID
	}
	e, err := m.Ensure(ctx, userID)
	if err != nil {
		return nil, err
	}
	runtime, ok := m.runtime.(ProjectApplicationRuntime)
	if !ok {
		return nil, errors.New("runtime project application discovery is not supported")
	}
	return runtime.DiscoverProjectApplications(ctx, e, projectID)
}

func (m *Manager) StopSession(ctx context.Context, userID, sessionID string) error {
	m.mu.RLock()
	e, ok := m.executors[userID]
	m.mu.RUnlock()
	if !ok {
		return ErrExecutorNotFound
	}
	runtime, supported := m.runtime.(SessionRuntime)
	if !supported {
		return errors.New("runtime session control is not supported")
	}
	return runtime.StopSession(ctx, e, sessionID)
}

func (m *Manager) ObserveSessions(ctx context.Context, userID string) ([]SessionObservation, error) {
	m.mu.RLock()
	e, ok := m.executors[userID]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrExecutorNotFound
	}
	runtime, supported := m.runtime.(SessionRuntime)
	if !supported {
		return nil, errors.New("runtime session control is not supported")
	}
	return runtime.ObserveSessions(ctx, e)
}

func (m *Manager) finishProvision(userID string, e Executor, err error) (Executor, error) {
	m.mu.Lock()
	m.executors[userID] = e
	call := m.provisioning[userID]
	delete(m.provisioning, userID)
	if call != nil {
		call.executor = e
		call.err = err
		close(call.done)
	}
	m.mu.Unlock()
	return e, err
}
func (m *Manager) Submit(ctx context.Context, userID string, input []byte, blobRefs ...string) (Job, error) {
	if !ValidUserID(userID) {
		return Job{}, ErrInvalidUserID
	}
	if len(input) == 0 {
		return Job{}, errors.New("job input is required")
	}
	if len(input) > 256*1024 {
		return Job{}, ErrInputTooLarge
	}
	for _, ref := range blobRefs {
		if ref == "" || len(ref) > 512 || len(ref) < len(userID)+2 || ref[:len(userID)+1] != userID+"/" {
			return Job{}, errors.New("invalid blob reference")
		}
	}
	e, err := m.Ensure(ctx, userID)
	if err != nil {
		return Job{}, err
	}
	now := time.Now().UTC()
	id, err := randomID("job")
	if err != nil {
		return Job{}, err
	}
	j := Job{ID: id, UserID: userID, ExecutorID: e.ID, Status: "queued", CreatedAt: &now, Input: append([]byte(nil), input...), BlobRefs: append([]string(nil), blobRefs...)}
	m.mu.Lock()
	if existing := m.running[userID]; existing != "" {
		m.mu.Unlock()
		return Job{}, ErrUserBusy
	}
	m.jobs[j.ID] = j
	m.running[userID] = j.ID
	m.mu.Unlock()
	if err := m.store.SaveJob(ctx, j); err != nil {
		m.mu.Lock()
		delete(m.jobs, j.ID)
		delete(m.running, userID)
		m.mu.Unlock()
		return Job{}, err
	}
	select {
	case m.queue <- j.ID:
		return j, nil
	default:
		m.mu.Lock()
		j.Status = "rejected"
		j.Err = ErrQueueFull.Error()
		m.jobs[j.ID] = j
		delete(m.running, userID)
		m.terminalOrder = append(m.terminalOrder, j.ID)
		removed := m.pruneLocked()
		m.mu.Unlock()
		_ = m.store.SaveJob(context.Background(), j)
		_ = deleteJobs(context.Background(), m.store, removed)
		return Job{}, ErrQueueFull
	}
}
func (m *Manager) Job(id string) (Job, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[id]
	return cloneJob(j), ok
}

// Subscribe returns a snapshot and a notification channel for event-driven
// consumers. Notifications are coalesced; callers must fetch the latest job.
func (m *Manager) Subscribe(id string) (Job, <-chan struct{}, func(), bool) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return Job{}, nil, func() {}, false
	}
	ch := make(chan struct{}, 1)
	if m.subscribers[id] == nil {
		m.subscribers[id] = map[chan struct{}]struct{}{}
	}
	m.subscribers[id][ch] = struct{}{}
	m.mu.Unlock()
	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.subscribers[id], ch)
			if len(m.subscribers[id]) == 0 {
				delete(m.subscribers, id)
			}
			m.mu.Unlock()
		})
	}
	return cloneJob(j), ch, unsubscribe, true
}
func (m *Manager) Executors() []Executor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Executor, 0, len(m.executors))
	for _, e := range m.executors {
		out = append(out, e)
	}
	return out
}
func (m *Manager) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := Stats{Executors: len(m.executors), ActiveExecutors: m.activeExecutorsLocked(), ExecutorCapacity: m.maxExecutors, Jobs: map[string]int{}, QueueDepth: len(m.queue), QueueCapacity: cap(m.queue), ActiveUsers: len(m.running), Provisioning: len(m.provisioning)}
	for _, j := range m.jobs {
		s.Jobs[j.Status]++
		if j.Status == "running" {
			s.Running++
		}
	}
	for _, executor := range m.executors {
		s.DiskUsedBytes += executor.DiskUsedBytes
		if executor.DiskLimitBytes > 0 {
			s.DiskQuotaBytes += executor.DiskLimitBytes
		} else {
			s.DiskQuotaBytes += executor.DiskBytes
		}
		if executor.Status == "quota_exceeded" {
			s.DiskQuotaExceeded++
		}
	}
	s.DiskScanErrors = m.diskScanErrors
	s.DiskFreeBytes = m.diskFreeBytes
	return s
}

func (m *Manager) storageLoop(ctx context.Context, interval time.Duration) {
	m.scanStorage(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.scanStorage(ctx)
		}
	}
}

func (m *Manager) scanStorage(ctx context.Context) {
	runtime, ok := m.runtime.(StorageRuntime)
	if !ok {
		return
	}
	for _, executor := range m.Executors() {
		if err := ctx.Err(); err != nil {
			return
		}
		observation, err := runtime.ObserveStorage(ctx, executor)
		if err != nil {
			m.mu.Lock()
			m.diskScanErrors++
			m.mu.Unlock()
			continue
		}
		now := time.Now().UTC()
		executor.DiskUsedBytes = observation.UsedBytes
		executor.DiskLimitBytes = observation.LimitBytes
		executor.DiskCheckedAt = &now
		if observation.LimitBytes > 0 && observation.UsedBytes > observation.LimitBytes && executor.Status != "stopped" && executor.Status != "quota_exceeded" {
			if stopErr := m.runtime.Stop(ctx, executor); stopErr != nil && !errors.Is(stopErr, ErrExecutorNotFound) {
				m.mu.Lock()
				m.diskScanErrors++
				m.mu.Unlock()
				continue
			}
			executor.Status = "quota_exceeded"
		}
		m.mu.Lock()
		m.diskFreeBytes = observation.FreeBytes
		current, exists := m.executors[executor.UserID]
		if exists && current.ID == executor.ID {
			current.DiskUsedBytes = executor.DiskUsedBytes
			current.DiskLimitBytes = executor.DiskLimitBytes
			current.DiskCheckedAt = executor.DiskCheckedAt
			if executor.Status == "quota_exceeded" {
				current.Status = executor.Status
			}
			executor = current
			m.executors[executor.UserID] = executor
		}
		m.mu.Unlock()
		if exists {
			saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := m.store.SaveExecutor(saveCtx, executor); err != nil {
				m.mu.Lock()
				m.diskScanErrors++
				m.mu.Unlock()
			}
			cancel()
		}
	}
}
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return errors.New("job not found")
	}
	if j.Status != "queued" && j.Status != "running" {
		m.mu.Unlock()
		return ErrJobNotCancelable
	}
	if c := m.cancel[id]; c != nil {
		c()
		m.mu.Unlock()
		return nil
	}
	j.Status = "canceled"
	now := time.Now().UTC()
	j.FinishedAt = &now
	m.jobs[id] = j
	delete(m.running, j.UserID)
	m.terminalOrder = append(m.terminalOrder, id)
	m.notifyLocked(id)
	removed := m.pruneLocked()
	m.mu.Unlock()
	if err := m.store.SaveJob(context.Background(), j); err != nil {
		return err
	}
	return deleteJobs(context.Background(), m.store, removed)
}
func (m *Manager) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-m.queue:
			m.run(ctx, id)
		}
	}
}
func (m *Manager) run(ctx context.Context, id string) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok || j.Status != "queued" {
		m.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	j.Status = "running"
	j.StartedAt = &now
	m.jobs[id] = j
	m.running[j.UserID] = id
	runCtx, cancel := context.WithTimeout(ctx, m.jobTimeout)
	m.cancel[id] = cancel
	m.notifyLocked(id)
	m.mu.Unlock()
	if err := m.store.SaveJob(runCtx, j); err != nil {
		cancel()
		m.finishRun(id, j, nil, err)
		return
	}
	var out []byte
	var err error
	if sr, ok := m.runtime.(StreamRuntime); ok {
		out, err = sr.RunStream(runCtx, j, func(chunk []byte) {
			m.mu.Lock()
			remaining := maxJobLogBytes - j.LogBytes
			if remaining > 0 {
				if len(chunk) > remaining {
					chunk = chunk[:remaining]
					j.LogTruncated = true
				}
				j.Logs = append(j.Logs, string(chunk))
				j.LogBytes += len(chunk)
			} else {
				j.LogTruncated = true
			}
			m.jobs[id] = j
			m.notifyLocked(id)
			m.mu.Unlock()
		})
	} else {
		out, err = m.runtime.Run(runCtx, j)
	}
	if runCtx.Err() != nil {
		if err == nil {
			err = runCtx.Err()
		} else if !errors.Is(err, runCtx.Err()) {
			err = errors.Join(runCtx.Err(), err)
		}
	}
	if len(out) > maxJobLogBytes {
		out = out[:maxJobLogBytes]
		j.LogTruncated = true
	}
	cancel()
	m.finishRun(id, j, out, err)
}

func (m *Manager) finishRun(id string, j Job, out []byte, err error) {
	m.mu.Lock()
	delete(m.running, j.UserID)
	delete(m.cancel, id)
	done := time.Now().UTC()
	j.FinishedAt = &done
	if errors.Is(err, context.Canceled) {
		j.Status = "canceled"
		j.Err = err.Error()
	} else if err != nil {
		j.Status = "failed"
		j.Err = err.Error()
	} else {
		j.Status = "succeeded"
		j.Output = out
	}
	j.Input = nil
	var executorToSave *Executor
	if errors.Is(err, ErrExecutorUnavailable) {
		executor := m.executors[j.UserID]
		executor.Status = "failed"
		m.executors[j.UserID] = executor
		executorToSave = &executor
	}
	m.jobs[id] = j
	m.terminalOrder = append(m.terminalOrder, id)
	m.notifyLocked(id)
	removed := m.pruneLocked()
	m.mu.Unlock()
	_ = m.store.SaveJob(context.Background(), j)
	if executorToSave != nil {
		_ = m.store.SaveExecutor(context.Background(), *executorToSave)
	}
	_ = deleteJobs(context.Background(), m.store, removed)
}

func (m *Manager) notifyLocked(id string) {
	for ch := range m.subscribers[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func cloneJob(j Job) Job {
	j.Input = append([]byte(nil), j.Input...)
	j.Output = append([]byte(nil), j.Output...)
	j.BlobRefs = append([]string(nil), j.BlobRefs...)
	j.Logs = append([]string(nil), j.Logs...)
	return j
}

func (m *Manager) pruneLocked() []string {
	if len(m.terminalOrder) <= m.retainedJobs {
		return nil
	}
	removeCount := len(m.terminalOrder) - m.retainedJobs
	removed := make([]string, 0, removeCount)
	for _, id := range m.terminalOrder[:removeCount] {
		if j, ok := m.jobs[id]; ok && terminal(j.Status) {
			delete(m.jobs, id)
			removed = append(removed, id)
		}
	}
	copy(m.terminalOrder, m.terminalOrder[removeCount:])
	m.terminalOrder = m.terminalOrder[:len(m.terminalOrder)-removeCount]
	return removed
}

func terminal(status string) bool {
	return status == "succeeded" || status == "failed" || status == "canceled" || status == "rejected"
}

func jobFinishedAt(j Job) time.Time {
	if j.FinishedAt != nil {
		return *j.FinishedAt
	}
	if j.CreatedAt != nil {
		return *j.CreatedAt
	}
	return time.Time{}
}

func deleteJobs(ctx context.Context, store Store, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if deleter, ok := store.(JobDeleter); ok {
		return deleter.DeleteJobs(ctx, ids)
	}
	return nil
}

func randomID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(raw[:]), nil
}

func ValidUserID(id string) bool {
	if len(id) < 1 || len(id) > 128 || id == "." || id == ".." {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}
