package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/capability"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

type managerStore struct {
	mu        sync.Mutex
	executors map[string]manager.Executor
	jobs      map[string]manager.Job
}

func newManagerStore() *managerStore {
	return &managerStore{executors: map[string]manager.Executor{}, jobs: map[string]manager.Job{}}
}
func (s *managerStore) Load(context.Context) ([]manager.Executor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]manager.Executor, 0, len(s.executors))
	for _, v := range s.executors {
		out = append(out, v)
	}
	return out, nil
}
func (s *managerStore) LoadJobs(context.Context) ([]manager.Job, error) { return nil, nil }
func (s *managerStore) SaveExecutor(_ context.Context, value manager.Executor) error {
	s.mu.Lock()
	s.executors[value.UserID] = value
	s.mu.Unlock()
	return nil
}
func (s *managerStore) SaveJob(_ context.Context, value manager.Job) error {
	s.mu.Lock()
	s.jobs[value.ID] = value
	s.mu.Unlock()
	return nil
}

type observedRuntime struct {
	mu               sync.Mutex
	running          map[string]bool
	sessions         map[string]manager.SessionSpec
	ensureCalls      int
	failEnsures      int
	observeDelay     time.Duration
	observeGate      <-chan struct{}
	observeStarted   chan<- struct{}
	observeActive    int
	maxObserveActive int
	startDelay       time.Duration
	startGate        <-chan struct{}
	startActive      int
	maxStartActive   int
	onStopSession    func()
}

type fakeRelayControl struct {
	disconnected string
	driverUser   string
}

type staticClaudeOAuth struct {
	token   string
	version string
	err     error
}

func (p staticClaudeOAuth) CurrentOAuthToken(context.Context) (string, string, error) {
	return p.token, p.version, p.err
}

func (f *fakeRelayControl) DisconnectConnection(_ context.Context, address, connectionID string) error {
	f.disconnected = address + "/" + connectionID
	return nil
}

func (f *fakeRelayControl) SetDriver(_ context.Context, address, room, deviceID, sessionID, userID string, relayGeneration int64) (DriverResult, error) {
	f.driverUser = fmt.Sprintf("%s/%s/%s/%s/%s/%d", address, room, deviceID, sessionID, userID, relayGeneration)
	return DriverResult{UserID: userID, Generation: 2, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (r *observedRuntime) StartSession(ctx context.Context, _ manager.Executor, spec manager.SessionSpec) error {
	r.mu.Lock()
	delay := r.startDelay
	gate := r.startGate
	tracked := delay > 0 || gate != nil
	if tracked {
		r.startActive++
		r.maxStartActive = max(r.maxStartActive, r.startActive)
	}
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			r.mu.Lock()
			r.startActive--
			r.mu.Unlock()
			return ctx.Err()
		}
	} else if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			r.mu.Lock()
			r.startActive--
			r.mu.Unlock()
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if tracked {
		r.startActive--
	}
	if r.sessions == nil {
		r.sessions = map[string]manager.SessionSpec{}
	}
	r.sessions[spec.ID] = spec
	return nil
}
func (r *observedRuntime) StopSession(_ context.Context, _ manager.Executor, id string) error {
	r.mu.Lock()
	delete(r.sessions, id)
	hook := r.onStopSession
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}
func (r *observedRuntime) ObserveSessions(_ context.Context, _ manager.Executor) ([]manager.SessionObservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]manager.SessionObservation, 0, len(r.sessions))
	for id := range r.sessions {
		out = append(out, manager.SessionObservation{ID: id, State: "running"})
	}
	return out, nil
}

func (r *observedRuntime) Ensure(_ context.Context, value manager.Executor) error {
	r.mu.Lock()
	r.ensureCalls++
	if r.failEnsures > 0 {
		r.failEnsures--
		r.mu.Unlock()
		return errors.New("injected ensure failure")
	}
	if r.running == nil {
		r.running = map[string]bool{}
	}
	r.running[value.UserID] = true
	r.mu.Unlock()
	return nil
}
func (r *observedRuntime) Run(context.Context, manager.Job) ([]byte, error) { return nil, nil }
func (r *observedRuntime) Stop(_ context.Context, value manager.Executor) error {
	r.mu.Lock()
	delete(r.running, value.UserID)
	r.mu.Unlock()
	return nil
}
func (r *observedRuntime) Observe(ctx context.Context, value manager.Executor) (manager.RuntimeObservation, error) {
	r.mu.Lock()
	running := r.running[value.UserID]
	delay := r.observeDelay
	gate := r.observeGate
	started := r.observeStarted
	tracked := delay > 0 || gate != nil
	if tracked {
		r.observeActive++
		r.maxObserveActive = max(r.maxObserveActive, r.observeActive)
	}
	r.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			r.mu.Lock()
			r.observeActive--
			r.mu.Unlock()
			return manager.RuntimeObservation{}, ctx.Err()
		}
	} else if delay > 0 {
		time.Sleep(delay)
	}
	if tracked {
		r.mu.Lock()
		r.observeActive--
		r.mu.Unlock()
	}
	if !running {
		return manager.RuntimeObservation{}, manager.ErrExecutorNotFound
	}
	return manager.RuntimeObservation{RuntimeID: "container-" + value.UserID, Image: "pie:test", Status: "running", Health: "healthy", Running: true}, nil
}

func TestStableReconciliationCoalescesControlWrites(t *testing.T) {
	c, m, service := testController(t)
	if _, err := m.Ensure(context.Background(), "stable-owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtimeBefore, ok := service.Runtime("executor-stable-owner")
	if !ok {
		t.Fatal("runtime was not reconciled")
	}
	deviceBefore, ok := service.Device("executor-stable-owner")
	if !ok {
		t.Fatal("device was not reconciled")
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtimeAfter, _ := service.Runtime(runtimeBefore.ID)
	deviceAfter, _ := service.Device(deviceBefore.ID)
	if runtimeAfter.Version != runtimeBefore.Version || deviceAfter.Version != deviceBefore.Version {
		t.Fatalf("stable reconcile wrote records: runtime %d->%d device %d->%d", runtimeBefore.Version, runtimeAfter.Version, deviceBefore.Version, deviceAfter.Version)
	}
}

func TestReconciliationAppliesPerUserExecutorQuota(t *testing.T) {
	c, m, service := testController(t)
	if _, err := m.Ensure(context.Background(), "quota-owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	user, ok := service.User("quota-owner")
	if !ok {
		t.Fatal("reconciled user is missing")
	}
	user.Quota = control.ResourceQuota{CPUs: "1.5", MemoryBytes: 1 << 30, PIDs: 96, MaxSessions: 2, MaxParticipants: 4}
	if _, err := service.PutUser(context.Background(), user, user.Version, control.MutationMeta{ActorUserID: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	executor, ok := m.Executor("quota-owner")
	if !ok || executor.CPUs != "1.5" || executor.MemoryBytes != 1<<30 || executor.PIDsLimit != 96 {
		t.Fatalf("executor=%+v ok=%t", executor, ok)
	}
	runtime, ok := service.Runtime(executor.ID)
	if !ok || runtime.Quota != user.Quota {
		t.Fatalf("runtime=%+v ok=%t", runtime, ok)
	}
}

func TestReconciliationRetriesFailedQuotaUpdateOnRunningExecutor(t *testing.T) {
	c, m, service, runtime := testControllerRuntime(t)
	if _, err := m.Ensure(context.Background(), "quota-retry-owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	user, ok := service.User("quota-retry-owner")
	if !ok {
		t.Fatal("reconciled user is missing")
	}
	user.Quota = control.ResourceQuota{CPUs: "1.5", MemoryBytes: 1 << 30, PIDs: 96}
	if _, err := service.PutUser(context.Background(), user, user.Version, control.MutationMeta{ActorUserID: "test"}); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.failEnsures = 1
	runtime.mu.Unlock()
	if err := c.reconcile(context.Background()); err == nil {
		t.Fatal("expected injected quota update failure")
	}
	failed, _ := m.Executor("quota-retry-owner")
	if failed.Status != "failed" {
		t.Fatalf("executor did not retain failed state: %+v", failed)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, _ := m.Executor("quota-retry-owner")
	if recovered.Status != "ready" || recovered.CPUs != "1.5" || recovered.MemoryBytes != 1<<30 || recovered.PIDsLimit != 96 {
		t.Fatalf("executor quota update did not recover: %+v", recovered)
	}
	runtime.mu.Lock()
	ensureCalls := runtime.ensureCalls
	runtime.mu.Unlock()
	if ensureCalls != 3 {
		t.Fatalf("ensure calls=%d want initial, failed update, retry", ensureCalls)
	}
}

func TestExecutorReconciliationUsesBoundedConcurrency(t *testing.T) {
	c, m, _, runtime := testControllerRuntime(t)
	gate := make(chan struct{})
	started := make(chan struct{}, 4)
	runtime.mu.Lock()
	runtime.observeGate = gate
	runtime.observeStarted = started
	runtime.mu.Unlock()
	for _, userID := range []string{"parallel-a", "parallel-b", "parallel-c", "parallel-d"} {
		if _, err := m.Ensure(context.Background(), userID); err != nil {
			t.Fatal(err)
		}
	}
	reconciled := make(chan error, 1)
	go func() { reconciled <- c.reconcile(context.Background()) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			close(gate)
			t.Fatal("reconciliation workers did not overlap")
		}
	}
	close(gate)
	if err := <-reconciled; err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	maxActive := runtime.maxObserveActive
	runtime.mu.Unlock()
	if maxActive < 2 || maxActive > c.options.ReconcileConcurrency {
		t.Fatalf("max concurrent observations=%d limit=%d", maxActive, c.options.ReconcileConcurrency)
	}
}

func testController(t *testing.T) (*Controller, *manager.Manager, *control.Service) {
	c, m, service, _ := testControllerRuntime(t)
	return c, m, service
}

func testControllerRuntime(t *testing.T) (*Controller, *manager.Manager, *control.Service, *observedRuntime) {
	t.Helper()
	ctx := context.Background()
	runtime := &observedRuntime{running: map[string]bool{}}
	m, err := manager.New(ctx, runtime, newManagerStore(), 4)
	if err != nil {
		t.Fatal(err)
	}
	service, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(ctx, m, service, Options{NodeID: "node-a", Image: "pie:test", ReconcileInterval: time.Hour, OperationTimeout: time.Second, RelayURL: "ws://relay.test/ws/agent", Issuer: capability.Issuer{Secret: []byte("01234567890123456789012345678901")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); m.Close(); _ = service.Close() })
	return c, m, service, runtime
}

func TestDockerSessionIsStartedAndMarkedReady(t *testing.T) {
	c, m, service := testController(t)
	if _, err := m.Ensure(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	created, err := service.PutSession(context.Background(), control.Session{ID: "session-a", OwnerUserID: "owner", DeviceID: "executor-owner", ExecutionTarget: "docker", AccessMode: "private", TransportMode: "relay", Status: "starting"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.reconcileDockerSessions(context.Background())
	got, ok := service.Session(created.ID)
	if !ok || got.Status != "ready" || got.SelectedTransport != "relay" || got.StartAttempts != 1 || got.LastError != "" {
		t.Fatalf("session=%+v", got)
	}
}

func TestMissingDockerSessionIsAutomaticallyRecovered(t *testing.T) {
	c, m, service, runtime := testControllerRuntime(t)
	if _, err := m.Ensure(context.Background(), "recover-owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	created, err := service.PutSession(context.Background(), control.Session{
		ID: "session-recover", OwnerUserID: "recover-owner", DeviceID: "executor-recover-owner",
		ExecutionTarget: "docker", AccessMode: "private", TransportMode: "relay", Status: "starting",
	}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.reconcileDockerSessions(context.Background())
	runtime.mu.Lock()
	delete(runtime.sessions, created.ID)
	runtime.mu.Unlock()

	c.reconcileActiveDockerSessions(context.Background())
	recovering, _ := service.Session(created.ID)
	if recovering.Status != "starting" || recovering.LastError == "" {
		t.Fatalf("missing session was not scheduled for recovery: %+v", recovering)
	}
	c.startDockerSessions(context.Background())
	recovered, _ := service.Session(created.ID)
	if recovered.Status != "ready" || recovered.StartAttempts != 2 || recovered.LastError != "" {
		t.Fatalf("session did not recover: %+v", recovered)
	}
}

func TestDockerSessionRecoversWhenRelayReturns(t *testing.T) {
	c, m, service := testController(t)
	c.options.Issuer.RoutingSecret = []byte("routing-secret-012345678901234567")
	if _, err := m.Ensure(context.Background(), "relay-recover-owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	session, err := service.PutSession(context.Background(), control.Session{
		ID: "session-relay-recover", OwnerUserID: "relay-recover-owner", DeviceID: "executor-relay-recover-owner",
		ApplicationID: "pie-control", PoolID: "pool-a", TenantID: "tenant-a", ResourceType: "device",
		ResourceID: "executor-relay-recover-owner", Protocol: "terminal", RelayGeneration: 2,
		ExecutionTarget: "docker", AccessMode: "shared", TransportMode: "relay", Status: "reconnecting",
	}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.startDockerSessionCandidate(context.Background(), session)
	waiting, _ := service.Session(session.ID)
	if waiting.Status != "reconnecting" || waiting.LastError != control.ErrNoRelayCapacity.Error() {
		t.Fatalf("session did not retain recoverable state: %+v", waiting)
	}
	if _, err := service.PutNode(context.Background(), control.Node{
		ID: "relay-a", Kind: "relay", Status: "ready", Address: "https://relay.test",
		ControlAddress: "https://relay.test", PoolID: "pool-a", AllowedApplications: []string{"pie-control"},
		LastHeartbeat: time.Now().UTC(),
	}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	c.startDockerSessionCandidate(context.Background(), waiting)
	recovered, _ := service.Session(session.ID)
	if recovered.Status != "ready" || recovered.RelayNodeID != "relay-a" || recovered.LastError != "" {
		t.Fatalf("session did not recover: %+v", recovered)
	}
}

func TestManagedChatSessionRebindsAfterDeploymentProfileChange(t *testing.T) {
	c, m, service, runtime := testControllerRuntime(t)
	ctx := context.Background()
	c.options.ClaudeOAuth = staticClaudeOAuth{token: "sk-ant-oat-controller-test-secret", version: "v-oauth-1"}
	c.options.Issuer.RoutingSecret = []byte("routing-secret-012345678901234567")
	if _, err := m.Ensure(ctx, "profile-owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	integration, err := service.PutIntegration(ctx, control.Integration{ID: "profile-integration", DisplayName: "Profile", Status: "active", Credential: control.CredentialProfile{TargetPath: ".profile/credential.json"}}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := service.PutIntegrationUser(ctx, control.IntegrationUser{ID: "profile-binding", IntegrationID: integration.ID, ExternalUserID: "external-profile", OwnerUserID: "profile-owner", Status: "ready"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.PutProject(ctx, control.Project{ID: "profile-project", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: binding.OwnerUserID, Name: "Profile", Locale: "ko", Status: "ready"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.PutSession(ctx, control.Session{ID: "session-profile-rebind", OwnerUserID: binding.OwnerUserID, DeviceID: "executor-profile-owner", ProjectID: project.ID, ApplicationID: "pie-control", PoolID: "pie-relay-sandbox", TenantID: binding.OwnerUserID, ResourceType: "device", ResourceID: "executor-profile-owner", Protocol: "terminal", ExecutionTarget: "docker", AgentMode: "chat", AccessMode: "private", TransportMode: "relay", Status: "reconnecting"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutConversation(ctx, control.Conversation{ID: "conversation-profile-rebind", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: binding.OwnerUserID, DeviceID: session.DeviceID, ProjectID: project.ID, SessionID: session.ID, Status: "connecting"}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := service.SetDefaultRelayContext("kroot-studio", "kroot-studio"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutNode(ctx, control.Node{ID: "relay-profile", Kind: "relay", Status: "ready", Address: "https://relay-test.example", PoolID: "kroot-studio", AllowedApplications: []string{"kroot-studio"}, LastHeartbeat: time.Now().UTC()}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}

	c.startDockerSessionCandidate(ctx, session)
	recovered, _ := service.Session(session.ID)
	if recovered.Status != "ready" || recovered.ApplicationID != "kroot-studio" || recovered.PoolID != "kroot-studio" || recovered.RelayNodeID != "relay-profile" || recovered.LastError != "" {
		t.Fatalf("recovered=%+v", recovered)
	}
	runtime.mu.Lock()
	spec, running := runtime.sessions[session.ID]
	runtime.mu.Unlock()
	if !running || spec.RelayURL != "wss://relay-test.example/ws/agent" || spec.ClaudeOAuthToken != "sk-ant-oat-controller-test-secret" || spec.ClaudeAuthVersion != "v-oauth-1" {
		t.Fatalf("running=%t spec=%+v", running, spec)
	}
}

func TestClaudeOAuthProviderFailureBlocksChatSession(t *testing.T) {
	c, m, service, runtime := testControllerRuntime(t)
	c.options.ClaudeOAuth = staticClaudeOAuth{err: errors.New("secret backend failure")}
	ctx := context.Background()
	if _, err := m.Ensure(ctx, "oauth-failure-owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	session, err := service.PutSession(ctx, control.Session{
		ID: "session-oauth-failure", OwnerUserID: "oauth-failure-owner", DeviceID: "executor-oauth-failure-owner",
		ExecutionTarget: "docker", AgentMode: "chat", AccessMode: "private", TransportMode: "relay", Status: "starting",
	}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.startDockerSessionCandidate(ctx, session)
	failed, _ := service.Session(session.ID)
	if failed.Status != "error" || !strings.Contains(failed.LastError, "subscription authentication is unavailable") || strings.Contains(failed.LastError, "backend") {
		t.Fatalf("failed session=%+v", failed)
	}
	runtime.mu.Lock()
	_, running := runtime.sessions[session.ID]
	runtime.mu.Unlock()
	if running {
		t.Fatal("chat session started without subscription authentication")
	}
}

func TestStaleRelayNodeRestartsDockerSessionOnNewGeneration(t *testing.T) {
	c, m, service, runtime := testControllerRuntime(t)
	c.options.Issuer.RoutingSecret = []byte("routing-secret-012345678901234567")
	ctx := context.Background()
	if _, err := m.Ensure(ctx, "relay-recover-owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, node := range []control.Node{
		{ID: "relay-stale", Kind: "relay", Status: "ready", Address: "https://stale.example", ControlAddress: "http://stale.internal", PoolID: "pool-a", AllowedApplications: []string{"pie-control"}, LastHeartbeat: now.Add(-2 * time.Minute)},
		{ID: "relay-healthy", Kind: "relay", Status: "ready", Address: "https://healthy.example", ControlAddress: "http://healthy.internal", PoolID: "pool-a", AllowedApplications: []string{"pie-control"}, LastHeartbeat: now},
	} {
		if _, err := service.PutNode(ctx, node, 0, control.MutationMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	session, err := service.PutSession(ctx, control.Session{
		ID: "session-relay-recover", OwnerUserID: "relay-recover-owner", DeviceID: "executor-relay-recover-owner",
		ApplicationID: "pie-control", PoolID: "pool-a", TenantID: "relay-recover-owner", ResourceType: "device", ResourceID: "executor-relay-recover-owner", Protocol: "terminal",
		ExecutionTarget: "docker", AccessMode: "private", TransportMode: "relay", Status: "active", RelayNodeID: "relay-stale", HostConnectionID: "old-host",
	}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.sessions = map[string]manager.SessionSpec{session.ID: {ID: session.ID, RelayURL: "wss://stale.example/ws/agent", Token: "generation-1"}}
	stopCalls := 0
	runtime.onStopSession = func() { stopCalls++ }
	runtime.mu.Unlock()

	if err := c.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _ := service.Session(session.ID)
	if recovered.Status != "ready" || recovered.RelayNodeID != "relay-healthy" || recovered.RelayGeneration != 2 || recovered.HostConnectionID != "" {
		t.Fatalf("recovered=%+v", recovered)
	}
	runtime.mu.Lock()
	spec := runtime.sessions[session.ID]
	runtime.mu.Unlock()
	if stopCalls != 1 || spec.RelayURL != "wss://healthy.example/ws/agent" || spec.Token == "" || spec.Token == "generation-1" {
		t.Fatalf("stopCalls=%d spec=%+v", stopCalls, spec)
	}
}

func TestFailedManagedChatSessionRetriesWithBackoff(t *testing.T) {
	c, m, service, runtime := testControllerRuntime(t)
	ctx := context.Background()
	if _, err := m.Ensure(ctx, "chat-recover-owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	integration, err := service.PutIntegration(ctx, control.Integration{
		ID: "recover-integration", DisplayName: "Recover", Status: "active",
		Credential: control.CredentialProfile{TargetPath: ".recover/credential.json", Format: "json", MaxBytes: 1024},
	}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := service.PutIntegrationUser(ctx, control.IntegrationUser{ID: "recover-binding", IntegrationID: integration.ID, ExternalUserID: "external", OwnerUserID: "chat-recover-owner", Status: "ready"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.PutProject(ctx, control.Project{ID: "recover-project", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: binding.OwnerUserID, Name: "Recover", Locale: "ko", Status: "ready"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.PutSession(ctx, control.Session{ID: "session-chat-retry", OwnerUserID: binding.OwnerUserID, DeviceID: "executor-chat-recover-owner", ProjectID: project.ID, ExecutionTarget: "docker", AgentMode: "chat", AccessMode: "private", TransportMode: "relay", Status: "error", LastError: "previous failure", UpdatedAt: time.Now().Add(-time.Minute)}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutConversation(ctx, control.Conversation{ID: "conversation-retry", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: binding.OwnerUserID, DeviceID: session.DeviceID, ProjectID: project.ID, SessionID: session.ID, Status: "reconnecting"}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	// PutSession owns UpdatedAt, so force the retry delay to its minimum and
	// wait just beyond it instead of reaching into the persistence layer.
	time.Sleep(dockerSessionRetryDelay(session.StartAttempts) + 20*time.Millisecond)
	c.startDockerSessions(ctx)
	recovered, _ := service.Session(session.ID)
	if recovered.Status != "ready" || recovered.StartAttempts != 1 {
		t.Fatalf("managed chat session was not retried: %+v", recovered)
	}
	runtime.mu.Lock()
	_, running := runtime.sessions[session.ID]
	runtime.mu.Unlock()
	if !running {
		t.Fatal("managed chat runtime session was not started")
	}
}

func TestDockerSessionStartsUseBoundedConcurrency(t *testing.T) {
	c, m, service, runtime := testControllerRuntime(t)
	owners := []string{"session-owner-a", "session-owner-b", "session-owner-c", "session-owner-d"}
	for _, owner := range owners {
		if _, err := m.Ensure(context.Background(), owner); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, owner := range owners {
		if _, err := service.PutSession(context.Background(), control.Session{
			ID: "session-" + owner, OwnerUserID: owner, DeviceID: "executor-" + owner,
			ExecutionTarget: "docker", AccessMode: "private", TransportMode: "relay", Status: "starting",
		}, 0, control.MutationMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	gate := make(chan struct{})
	runtime.mu.Lock()
	runtime.startGate = gate
	runtime.mu.Unlock()
	done := make(chan struct{})
	go func() {
		c.startDockerSessions(context.Background())
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.mu.Lock()
		active := runtime.startActive
		runtime.mu.Unlock()
		if active >= 2 {
			break
		}
		if time.Now().After(deadline) {
			close(gate)
			<-done
			t.Fatalf("session starts stayed serialized: active=%d", active)
		}
		time.Sleep(time.Millisecond)
	}
	close(gate)
	<-done
	runtime.mu.Lock()
	maxActive := runtime.maxStartActive
	runtime.mu.Unlock()
	if maxActive < 2 || maxActive > c.options.ReconcileConcurrency {
		t.Fatalf("max concurrent session starts=%d limit=%d", maxActive, c.options.ReconcileConcurrency)
	}
	for _, owner := range owners {
		session, _ := service.Session("session-" + owner)
		if session.Status != "ready" {
			t.Fatalf("session %s status=%s error=%s", session.ID, session.Status, session.LastError)
		}
	}
}

func TestSessionRestartStartsANewExecutorPTY(t *testing.T) {
	c, m, service := testController(t)
	if _, err := m.Ensure(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	created, err := service.PutSession(context.Background(), control.Session{ID: "session-restart", OwnerUserID: "owner", DeviceID: "executor-owner", ExecutionTarget: "docker", AccessMode: "private", TransportMode: "relay", Status: "starting"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.reconcileDockerSessions(context.Background())
	op, _, err := service.BeginOperation(context.Background(), control.Operation{ActorUserID: "operator", Type: "session.restart", TargetKind: control.KindSession, TargetID: created.ID}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.runOperation(context.Background(), op.ID)
	finished, _ := service.Operation(op.ID)
	got, _ := service.Session(created.ID)
	if finished.Status != "succeeded" || got.Status != "ready" || got.StartAttempts != 2 {
		t.Fatalf("operation=%+v session=%+v", finished, got)
	}
}

func TestSessionCloseRetriesPresenceVersionRace(t *testing.T) {
	c, m, service, runtime := testControllerRuntime(t)
	if _, err := m.Ensure(context.Background(), "close-race-owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	created, err := service.PutSession(context.Background(), control.Session{
		ID: "session-close-race", OwnerUserID: "close-race-owner", DeviceID: "executor-close-race-owner",
		ExecutionTarget: "docker", AccessMode: "shared", TransportMode: "relay", Status: "starting",
	}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.reconcileDockerSessions(context.Background())
	runtime.mu.Lock()
	runtime.onStopSession = func() {
		current, _ := service.Session(created.ID)
		current.HostConnectionID = "presence-raced"
		_, _ = service.PutSession(context.Background(), current, current.Version, control.MutationMeta{ActorUserID: "relay"})
	}
	runtime.mu.Unlock()
	op, _, err := service.BeginOperation(context.Background(), control.Operation{
		ActorUserID: "operator", Type: "session.close", TargetKind: control.KindSession, TargetID: created.ID,
	}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.runOperation(context.Background(), op.ID)
	finished, _ := service.Operation(op.ID)
	closed, _ := service.Session(created.ID)
	if finished.Status != "succeeded" || closed.Status != "closed" || closed.HostConnectionID != "" {
		t.Fatalf("operation=%+v session=%+v", finished, closed)
	}
}

func TestParticipantDisconnectAndDriverOperationsUseRelayControl(t *testing.T) {
	c, _, service := testController(t)
	relayControl := &fakeRelayControl{}
	c.options.RelayControl = relayControl
	c.options.RelayControlURL = "https://relay.test"
	if _, err := service.PutUser(context.Background(), control.User{ID: "owner", Status: "active"}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterDevice(context.Background(), control.Device{ID: "device-local", OwnerUserID: "owner", Name: "Local", Kind: "local"}, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutSession(context.Background(), control.Session{ID: "session-control", OwnerUserID: "owner", DeviceID: "device-local", ExecutionTarget: "local", AccessMode: "shared", TransportMode: "relay", Status: "active"}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	participant, err := service.PutParticipant(context.Background(), control.Participant{SessionID: "session-control", UserID: "owner", ConnectionID: "connection-a", Role: "host", Access: "control"}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	disconnect, _, _ := service.BeginOperation(context.Background(), control.Operation{ActorUserID: "operator", Type: "participant.disconnect", TargetKind: control.KindParticipant, TargetID: participant.ID}, control.MutationMeta{})
	c.runOperation(context.Background(), disconnect.ID)
	if relayControl.disconnected != "https://relay.test/connection-a" {
		t.Fatalf("disconnect=%q", relayControl.disconnected)
	}
	driver, _, _ := service.BeginOperation(context.Background(), control.Operation{ActorUserID: "operator", Type: "session.driver.set", TargetKind: control.KindSession, TargetID: "session-control", Request: map[string]any{"userId": "owner"}}, control.MutationMeta{})
	c.runOperation(context.Background(), driver.ID)
	if relayControl.driverUser != "https://relay.test/owner/device-local/session-control/owner/1" {
		t.Fatalf("driver=%q", relayControl.driverUser)
	}
	got, _ := service.Session("session-control")
	if got.DriverUserID != "owner" || got.DriverLeaseExpiresAt == nil {
		t.Fatalf("session=%+v", got)
	}
}

func TestRelayRoutingRoomMatchesResourceScopedCredential(t *testing.T) {
	c, _, _ := testController(t)
	c.options.Issuer.RoutingSecret = []byte("routing-secret-012345678901234567")
	session := control.Session{
		ID: "session-scoped", OwnerUserID: "owner", ApplicationID: "pie-control",
		TenantID: "tenant-a", ResourceType: "device", ResourceID: "executor-owner",
	}
	room, err := c.relayRoutingRoom(session)
	if err != nil {
		t.Fatal(err)
	}
	minted, err := c.options.Issuer.MintSession(capability.SessionCredential{
		Subject: "owner", Room: session.OwnerUserID, DeviceID: "executor-owner", SessionID: session.ID,
		ApplicationID: session.ApplicationID, PoolID: "pool-a", TenantID: session.TenantID,
		ResourceType: session.ResourceType, ResourceID: session.ResourceID, Role: "host", Access: "control",
		RelayNode: "relay-a", RelayGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if room != minted.Room || room == session.OwnerUserID {
		t.Fatalf("control room=%q credential room=%q", room, minted.Room)
	}
}

func TestReconcileProjectsExecutorIntoControlPlane(t *testing.T) {
	c, m, service := testController(t)
	if _, err := m.Ensure(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := service.Snapshot()
	if len(snapshot.Users) != 1 || len(snapshot.Devices) != 1 || len(snapshot.Runtimes) != 1 || len(snapshot.Nodes) != 1 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.Runtimes[0].ObservedState != "running" || snapshot.Runtimes[0].ContainerID != "container-owner" {
		t.Fatalf("runtime not observed: %+v", snapshot.Runtimes[0])
	}
	if snapshot.Devices[0].ObservedState != "degraded" || !snapshot.Devices[0].RuntimeHealthy {
		t.Fatalf("device state not composed: %+v", snapshot.Devices[0])
	}
}

func TestRuntimeOperationsExecuteAndAreIdempotent(t *testing.T) {
	c, m, service := testController(t)
	if _, err := m.Ensure(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, _, err := service.BeginOperation(context.Background(), control.Operation{IdempotencyKey: "stop-1", ActorUserID: "operator", Type: "runtime.stop", TargetKind: control.KindRuntime, TargetID: "executor-owner"}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.runOperation(context.Background(), op.ID)
	done, ok := service.Operation(op.ID)
	if !ok || done.Status != "succeeded" {
		t.Fatalf("operation=%+v", done)
	}
	executor, _ := m.Executor("owner")
	if executor.Status != "stopped" {
		t.Fatalf("executor=%+v", executor)
	}
	if _, err := m.Observe(context.Background(), "owner"); !errors.Is(err, manager.ErrExecutorNotFound) {
		t.Fatalf("runtime still present: %v", err)
	}
	start, _, err := service.BeginOperation(context.Background(), control.Operation{ActorUserID: "operator", Type: "runtime.start", TargetKind: control.KindRuntime, TargetID: "executor-owner"}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.runOperation(context.Background(), start.ID)
	if _, err := m.Observe(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredLocalDeviceBecomesOffline(t *testing.T) {
	c, _, service := testController(t)
	if _, err := service.PutUser(context.Background(), control.User{ID: "owner", Status: "active"}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	device, err := service.RegisterDevice(context.Background(), control.Device{ID: "local-a", OwnerUserID: "owner", Kind: "local"}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.HeartbeatDevice(context.Background(), device.ID, "owner", control.DeviceHeartbeat{ClientConnected: true, ObservedState: "online"}, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	c.options.HeartbeatTimeout = time.Nanosecond
	time.Sleep(time.Millisecond)
	c.expireLocalDevices(context.Background())
	got, _ := service.Device(device.ID)
	if got.ObservedState != "offline" || got.ClientConnected {
		t.Fatalf("device=%+v", got)
	}
}

func TestReconcileRepairsUnexpectedlyMissingExecutor(t *testing.T) {
	c, m, _, runtime := testControllerRuntime(t)
	if _, err := m.Ensure(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	delete(runtime.running, "owner")
	runtime.mu.Unlock()
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Observe(context.Background(), "owner"); err != nil {
		t.Fatalf("executor was not repaired: %v", err)
	}
	runtime.mu.Lock()
	ensureCalls := runtime.ensureCalls
	runtime.mu.Unlock()
	if ensureCalls != 2 {
		t.Fatalf("ensure calls=%d, want initial provision and one repair", ensureCalls)
	}
}

func TestDeviceDrainClosesSessionsDisconnectsParticipantsAndStopsRuntime(t *testing.T) {
	c, m, service := testController(t)
	relayControl := &fakeRelayControl{}
	c.options.RelayControl = relayControl
	c.options.RelayControlURL = "https://relay.test"
	if _, err := m.Ensure(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	session, err := service.PutSession(context.Background(), control.Session{
		ID: "session-drain", OwnerUserID: "owner", DeviceID: "executor-owner",
		ExecutionTarget: "docker", AccessMode: "shared", TransportMode: "relay", Status: "starting", HostConnectionID: "host-drain",
	}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.reconcileDockerSessions(context.Background())
	participant, err := service.PutParticipant(context.Background(), control.Participant{
		SessionID: session.ID, UserID: "viewer", ConnectionID: "connection-drain", Role: "participant", Access: "view",
	}, control.MutationMeta{Trusted: true})
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := service.BeginOperation(context.Background(), control.Operation{
		ActorUserID: "operator", Type: "device.drain", TargetKind: control.KindDevice, TargetID: "executor-owner",
	}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.runOperation(context.Background(), op.ID)
	finished, _ := service.Operation(op.ID)
	if finished.Status != "succeeded" {
		t.Fatalf("operation=%+v", finished)
	}
	if _, ok := service.Participant(participant.ID); ok {
		t.Fatal("participant remained after drain")
	}
	gotSession, _ := service.Session(session.ID)
	if gotSession.Status != "closed" {
		t.Fatalf("session=%+v", gotSession)
	}
	device, _ := service.Device("executor-owner")
	if device.ObservedState != "stopped" || device.RuntimeRunning || device.ActiveSessions != 0 {
		t.Fatalf("device=%+v", device)
	}
	runtime, _ := service.Runtime("executor-owner")
	if runtime.ObservedState != "stopped" || runtime.DesiredState != "stopped" {
		t.Fatalf("runtime=%+v", runtime)
	}
	if relayControl.disconnected != "https://relay.test/connection-drain" {
		t.Fatalf("disconnect=%q", relayControl.disconnected)
	}
}

func TestDeviceDrainFailsClosedWhenLiveRelayCannotBeControlled(t *testing.T) {
	c, _, service := testController(t)
	if _, err := service.PutUser(context.Background(), control.User{ID: "owner", Status: "active"}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterDevice(context.Background(), control.Device{
		ID: "local-live", OwnerUserID: "owner", Kind: "local", ObservedState: "online",
	}, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutSession(context.Background(), control.Session{
		ID: "session-live", OwnerUserID: "owner", DeviceID: "local-live",
		ExecutionTarget: "local", AccessMode: "shared", TransportMode: "relay", Status: "active", HostConnectionID: "host-live",
	}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	op, _, err := service.BeginOperation(context.Background(), control.Operation{
		ActorUserID: "operator", Type: "device.drain", TargetKind: control.KindDevice, TargetID: "local-live",
	}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	c.runOperation(context.Background(), op.ID)
	finished, _ := service.Operation(op.ID)
	if finished.Status != "failed" || finished.Error == "" {
		t.Fatalf("operation=%+v", finished)
	}
	session, _ := service.Session("session-live")
	if session.Status != "active" {
		t.Fatalf("a failed drain claimed the live session was closed: %+v", session)
	}
	device, _ := service.Device("local-live")
	if device.ObservedState == "stopped" {
		t.Fatalf("a failed drain claimed the device was stopped: %+v", device)
	}
}
