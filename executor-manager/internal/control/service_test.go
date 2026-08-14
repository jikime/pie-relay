package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDefaultQuotaAppliesOnlyToNewUsersWithoutExplicitQuota(t *testing.T) {
	service, _ := newTestService(t)
	service.SetDefaultQuota(ResourceQuota{MaxSessions: 3, MaxParticipants: 9})
	defaulted, err := service.PutUser(context.Background(), User{ID: "defaulted", Status: "active"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.Quota.MaxSessions != 3 || defaulted.Quota.MaxParticipants != 9 {
		t.Fatalf("quota=%+v", defaulted.Quota)
	}
	explicit, err := service.PutUser(context.Background(), User{ID: "explicit", Status: "active", Quota: ResourceQuota{MaxSessions: 1}}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Quota.MaxSessions != 1 || explicit.Quota.MaxParticipants != 0 {
		t.Fatalf("explicit quota was overwritten: %+v", explicit.Quota)
	}
}

func TestInvalidResourceQuotaIsRejected(t *testing.T) {
	service, _ := newTestService(t)
	invalid := []ResourceQuota{
		{CPUs: "not-a-number"}, {CPUs: "NaN"}, {MemoryBytes: -1}, {PIDs: -1}, {DiskBytes: -1}, {MaxSessions: -1}, {MaxParticipants: -1},
	}
	for index, quota := range invalid {
		if _, err := service.PutUser(context.Background(), User{ID: fmt.Sprintf("invalid-quota-%d", index), Status: "active", Quota: quota}, 0, MutationMeta{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("quota=%+v err=%v", quota, err)
		}
	}
}

func TestInvalidExecutorUserIDsAreRejectedAtControlBoundary(t *testing.T) {
	service, _ := newTestService(t)
	for _, id := range []string{"auth0|subject", "email@example.com", ".", "..", strings.Repeat("a", 129)} {
		if _, err := service.PutUser(context.Background(), User{ID: id, Status: "active"}, 0, MutationMeta{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected invalid user ID %q to be rejected, got %v", id, err)
		}
	}
}

func TestDeviceMetadataIsSizeBounded(t *testing.T) {
	service, _ := newTestService(t)
	if _, err := service.PutUser(context.Background(), User{ID: "owner", Status: "active"}, 0, MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterDevice(context.Background(), Device{
		ID: "device-a", OwnerUserID: "owner", Kind: "local",
		Metadata: map[string]string{"aiRuntimesV1": strings.Repeat("a", (24<<10)+1)},
	}, MutationMeta{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized registration metadata err=%v", err)
	}
	device, err := service.RegisterDevice(context.Background(), Device{
		ID: "device-a", OwnerUserID: "owner", Kind: "local",
		Metadata: map[string]string{"aiRuntimesV1": `{"codex":{"status":"READY"}}`},
	}, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.HeartbeatDevice(context.Background(), device.ID, "owner", DeviceHeartbeat{
		ClientConnected: true,
		Metadata:        map[string]string{"aiRuntimesV1": strings.Repeat("b", (24<<10)+1)},
	}, MutationMeta{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized heartbeat metadata err=%v", err)
	}
}

func TestRelayPoolSchedulerPinsLeastLoadedHealthyNode(t *testing.T) {
	service, _ := newTestService(t)
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	for _, node := range []Node{
		{ID: "relay-b", Kind: "relay", Status: "ready", Address: "https://relay-b.example", PoolID: "canvas-seoul", AllowedApplications: []string{"pie-canvas"}, Usage: NodeUsage{Connections: 3}, LastHeartbeat: now},
		{ID: "relay-a", Kind: "relay", Status: "ready", Address: "https://relay-a.example", PoolID: "canvas-seoul", AllowedApplications: []string{"pie-canvas"}, Usage: NodeUsage{Connections: 1}, LastHeartbeat: now},
		{ID: "relay-full", Kind: "relay", Status: "ready", Address: "https://relay-full.example", PoolID: "canvas-seoul", AllowedApplications: []string{"pie-canvas"}, Capacity: NodeCapacity{MaxConnections: 1}, Usage: NodeUsage{Connections: 1}, LastHeartbeat: now},
		{ID: "relay-other", Kind: "relay", Status: "ready", Address: "https://relay-other.example", PoolID: "other-pool", AllowedApplications: []string{"pie-canvas"}, LastHeartbeat: now},
	} {
		if _, err := service.PutNode(context.Background(), node, 0, MutationMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	_, device := seedUserDevice(t, service, "owner", "device-a")
	session, err := service.PutSession(context.Background(), Session{
		ID: "session-a", OwnerUserID: "owner", DeviceID: device.ID,
		ApplicationID: "pie-canvas", PoolID: "canvas-seoul", TenantID: "workspace-a",
		ResourceType: "project", ResourceID: "project-a", Protocol: "acp",
		ExecutionTarget: "local", AccessMode: "private", TransportMode: "relay", Status: "ready",
	}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	assigned, node, err := service.EnsureSessionRelayNode(context.Background(), session.ID, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if node.ID != "relay-a" || assigned.RelayNodeID != "relay-a" {
		t.Fatalf("node=%+v session=%+v", node, assigned)
	}
	again, sameNode, err := service.EnsureSessionRelayNode(context.Background(), session.ID, MutationMeta{})
	if err != nil || again.RelayNodeID != assigned.RelayNodeID || sameNode.ID != node.ID {
		t.Fatalf("assignment was not sticky: session=%+v node=%+v err=%v", again, sameNode, err)
	}
}

func TestDefaultRelayContextScopesNewSessions(t *testing.T) {
	service, _ := newTestService(t)
	if err := service.SetDefaultRelayContext("pie-control", "pool-a"); err != nil {
		t.Fatal(err)
	}
	user, device := seedUserDevice(t, service, "owner", "device-a")
	session, err := service.PutSession(context.Background(), Session{
		ID: "session-default-scope", OwnerUserID: user.ID, DeviceID: device.ID,
		ExecutionTarget: "local", AgentMode: "terminal", AccessMode: "private", TransportMode: "relay", Status: "starting",
	}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if session.ApplicationID != "pie-control" || session.PoolID != "pool-a" || session.TenantID != user.ID ||
		session.ResourceType != "device" || session.ResourceID != device.ID || session.Protocol != "terminal" || session.RelayGeneration != 1 {
		t.Fatalf("default Relay context=%+v", session)
	}
	session.RelayGeneration = 2
	if _, err := service.PutSession(context.Background(), session, session.Version, MutationMeta{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("caller changed Relay generation: %v", err)
	}
}

func TestPartialRelayContextIsRejectedAtSessionBoundary(t *testing.T) {
	service, _ := newTestService(t)
	user, device := seedUserDevice(t, service, "owner", "device-a")
	_, err := service.PutSession(context.Background(), Session{
		ID: "session-partial", OwnerUserID: user.ID, DeviceID: device.ID,
		ApplicationID: "pie-control", PoolID: "pool-a",
		ExecutionTarget: "local", AccessMode: "private", TransportMode: "relay", Status: "starting",
	}, 0, MutationMeta{})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("partial Relay context error=%v", err)
	}
}

func TestExistingUnscopedRelaySessionIsMigratedOnce(t *testing.T) {
	service, _ := newTestService(t)
	user, device := seedUserDevice(t, service, "owner", "device-a")
	legacy, err := service.PutSession(context.Background(), Session{
		ID: "session-legacy", OwnerUserID: user.ID, DeviceID: device.ID,
		ExecutionTarget: "local", AccessMode: "private", TransportMode: "relay", Status: "ready",
	}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ApplicationID != "" {
		t.Fatalf("legacy session unexpectedly scoped: %+v", legacy)
	}
	if err := service.SetDefaultRelayContext("pie-control", "pool-a"); err != nil {
		t.Fatal(err)
	}
	migrated, err := service.MigrateDefaultRelaySessions(context.Background(), MutationMeta{ActorUserID: "controller"})
	if err != nil || migrated != 1 {
		t.Fatalf("migrated=%d err=%v", migrated, err)
	}
	upgraded, _ := service.Session(legacy.ID)
	if upgraded.ApplicationID != "pie-control" || upgraded.PoolID != "pool-a" || upgraded.RelayGeneration != 1 || upgraded.Version != legacy.Version+1 {
		t.Fatalf("upgraded=%+v", upgraded)
	}
	migrated, err = service.MigrateDefaultRelaySessions(context.Background(), MutationMeta{ActorUserID: "controller"})
	if err != nil || migrated != 0 {
		t.Fatalf("second migration=%d err=%v", migrated, err)
	}
}

func TestManagedChatSessionCanRebindToHealthyDefaultRelayContext(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	user, err := service.PutUser(ctx, User{ID: "owner-rebind", Status: "active", Quota: ResourceQuota{MaxSessions: 4}}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	device, err := service.RegisterDevice(ctx, Device{ID: "executor-owner-rebind", OwnerUserID: user.ID, Kind: "docker", Name: "Executor"}, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	integration, err := service.PutIntegration(ctx, Integration{ID: "integration-rebind", DisplayName: "Rebind", Status: "active", Credential: CredentialProfile{TargetPath: ".rebind/credential.json"}}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := service.PutIntegrationUser(ctx, IntegrationUser{ID: "binding-rebind", IntegrationID: integration.ID, ExternalUserID: "external-rebind", OwnerUserID: user.ID, Status: "ready"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.PutProject(ctx, Project{ID: "project-rebind", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: user.ID, Name: "Rebind", Locale: "ko", Status: "ready"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.PutSession(ctx, Session{
		ID: "session-rebind", OwnerUserID: user.ID, DeviceID: device.ID, ProjectID: project.ID,
		ApplicationID: "pie-control", PoolID: "pie-relay-sandbox", TenantID: user.ID,
		ResourceType: "device", ResourceID: device.ID, Protocol: "terminal",
		ExecutionTarget: "docker", AgentMode: "chat", AccessMode: "private", TransportMode: "relay", Status: "reconnecting",
	}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutConversation(ctx, Conversation{ID: "conversation-rebind", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: user.ID, DeviceID: device.ID, ProjectID: project.ID, SessionID: session.ID, Status: "connecting"}, 0, MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := service.SetDefaultRelayContext("kroot-studio", "kroot-studio"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutNode(ctx, Node{ID: "relay-kroot", Kind: "relay", Status: "ready", Address: "https://relay-test.example", PoolID: "kroot-studio", AllowedApplications: []string{"kroot-studio"}, LastHeartbeat: now}, 0, MutationMeta{}); err != nil {
		t.Fatal(err)
	}

	updated, changed, err := service.RebindManagedChatSessionToDefaultRelayContext(ctx, session.ID, MutationMeta{ActorUserID: "controller"})
	if err != nil || !changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	if updated.ApplicationID != "kroot-studio" || updated.PoolID != "kroot-studio" || updated.RelayNodeID != "" || updated.RelayGeneration != session.RelayGeneration+1 {
		t.Fatalf("rebound session=%+v", updated)
	}
	assigned, node, err := service.EnsureSessionRelayNode(ctx, updated.ID, MutationMeta{ActorUserID: "controller"})
	if err != nil || node.ID != "relay-kroot" || assigned.RelayNodeID != node.ID {
		t.Fatalf("assigned=%+v node=%+v err=%v", assigned, node, err)
	}
}

func TestRelayContextRebindRejectsUnmanagedDockerSession(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	user, err := service.PutUser(ctx, User{ID: "owner-unmanaged", Status: "active", Quota: ResourceQuota{MaxSessions: 2}}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	device, err := service.RegisterDevice(ctx, Device{ID: "executor-owner-unmanaged", OwnerUserID: user.ID, Kind: "docker"}, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.PutSession(ctx, Session{ID: "session-unmanaged", OwnerUserID: user.ID, DeviceID: device.ID, ApplicationID: "old-app", PoolID: "old-pool", TenantID: user.ID, ResourceType: "device", ResourceID: device.ID, Protocol: "terminal", ExecutionTarget: "docker", AgentMode: "chat", AccessMode: "private", TransportMode: "relay", Status: "reconnecting"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetDefaultRelayContext("new-app", "new-pool"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutNode(ctx, Node{ID: "relay-new", Kind: "relay", Status: "ready", Address: "https://relay.example", PoolID: "new-pool", AllowedApplications: []string{"new-app"}, LastHeartbeat: now}, 0, MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := service.RebindManagedChatSessionToDefaultRelayContext(ctx, session.ID, MutationMeta{}); !errors.Is(err, ErrForbidden) || changed {
		t.Fatalf("unmanaged rebind changed=%t err=%v", changed, err)
	}
	unchanged, _ := service.Session(session.ID)
	if unchanged.ApplicationID != "old-app" || unchanged.PoolID != "old-pool" {
		t.Fatalf("unmanaged session changed=%+v", unchanged)
	}
}

func TestStaleRelaySessionIsFencedAndReassigned(t *testing.T) {
	service, _ := newTestService(t)
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	for _, node := range []Node{
		{ID: "relay-stale", Kind: "relay", Status: "ready", Address: "https://stale.example", PoolID: "pool-a", AllowedApplications: []string{"pie-control"}, LastHeartbeat: now.Add(-2 * time.Minute)},
		{ID: "relay-healthy", Kind: "relay", Status: "ready", Address: "https://healthy.example", PoolID: "pool-a", AllowedApplications: []string{"pie-control"}, LastHeartbeat: now},
	} {
		if _, err := service.PutNode(context.Background(), node, 0, MutationMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	user, device := seedUserDevice(t, service, "owner", "device-a")
	leaseExpiry := now.Add(time.Minute)
	session, err := service.PutSession(context.Background(), Session{
		ID: "session-fenced", OwnerUserID: user.ID, DeviceID: device.ID,
		ApplicationID: "pie-control", PoolID: "pool-a", TenantID: user.ID, ResourceType: "device", ResourceID: device.ID,
		ExecutionTarget: "local", AgentMode: "terminal", AccessMode: "private", TransportMode: "relay", Status: "active", RelayNodeID: "relay-stale", HostConnectionID: "old-host", DriverUserID: user.ID, DriverLeaseExpiresAt: &leaseExpiry,
	}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	participant, err := service.PutParticipant(context.Background(), Participant{SessionID: session.ID, UserID: user.ID, ConnectionID: "old-participant", Role: "participant", Access: "control", Transport: "relay"}, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := service.RecoverStaleRelaySessions(context.Background(), 90*time.Second, MutationMeta{ActorUserID: "controller"})
	if err != nil || recovered != 1 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	fenced, _ := service.Session(session.ID)
	if fenced.Status != "reconnecting" || fenced.RelayNodeID != "" || fenced.HostConnectionID != "" || fenced.DriverUserID != "" || fenced.DriverLeaseExpiresAt != nil || fenced.RelayGeneration != 2 {
		t.Fatalf("fenced session=%+v", fenced)
	}
	if _, exists := service.Participant(participant.ID); exists {
		t.Fatal("stale Relay participant survived session fencing")
	}
	if err := service.ApplyRelayPresence(context.Background(), RelayPresence{
		EventID: "old-generation", NodeID: "relay-stale", ApplicationID: "pie-control", PoolID: "pool-a",
		DeviceID: device.ID, SessionID: session.ID, RelayGeneration: 1, UserID: user.ID,
		Role: "host", Access: "control", ConnectionID: "old-host", Kind: "host", Connected: true, HostOnline: true, At: now,
	}, MutationMeta{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("old generation presence was accepted: %v", err)
	}
	assigned, node, err := service.EnsureSessionRelayNode(context.Background(), session.ID, MutationMeta{})
	if err != nil || node.ID != "relay-healthy" || assigned.RelayNodeID != "relay-healthy" || assigned.RelayGeneration != 2 {
		t.Fatalf("assigned=%+v node=%+v err=%v", assigned, node, err)
	}
}

type testDistributedLockStore struct {
	Store
	mu    sync.Mutex
	calls int
}

func (s *testDistributedLockStore) WithLock(_ context.Context, _ string, fn func() error) error {
	s.mu.Lock()
	s.calls++
	defer s.mu.Unlock()
	return fn()
}

func TestRelayAssignmentUsesStoreDistributedLock(t *testing.T) {
	store := &testDistributedLockStore{Store: NewDirectoryStore(t.TempDir())}
	service, err := NewService(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	if _, err := service.PutNode(context.Background(), Node{ID: "relay-a", Kind: "relay", Status: "ready", Address: "https://relay.example", PoolID: "pool-a", AllowedApplications: []string{"pie-control"}, LastHeartbeat: now}, 0, MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	user, device := seedUserDevice(t, service, "owner", "device-a")
	session, err := service.PutSession(context.Background(), Session{ID: "session-lock", OwnerUserID: user.ID, DeviceID: device.ID, ApplicationID: "pie-control", PoolID: "pool-a", TenantID: user.ID, ResourceType: "device", ResourceID: device.ID, ExecutionTarget: "local", AccessMode: "private", TransportMode: "relay", Status: "ready"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.EnsureSessionRelayNode(context.Background(), session.ID, MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if store.calls != 1 {
		t.Fatalf("distributed lock calls=%d", store.calls)
	}
}

func TestPruneTerminalOperationsKeepsQueuedAndRecentWork(t *testing.T) {
	service, _ := newTestService(t)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	old, _, err := service.BeginOperation(context.Background(), Operation{ActorUserID: "operator", Type: "device.drain", TargetKind: KindDevice, TargetID: "device-old", IdempotencyKey: "old"}, MutationMeta{SkipAudit: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateOperation(context.Background(), old.ID, "succeeded", 100, nil, "", MutationMeta{SkipAudit: true}); err != nil {
		t.Fatal(err)
	}
	queued, _, err := service.BeginOperation(context.Background(), Operation{ActorUserID: "operator", Type: "device.drain", TargetKind: KindDevice, TargetID: "device-queued"}, MutationMeta{SkipAudit: true})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	recent, _, err := service.BeginOperation(context.Background(), Operation{ActorUserID: "operator", Type: "device.drain", TargetKind: KindDevice, TargetID: "device-recent"}, MutationMeta{SkipAudit: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateOperation(context.Background(), recent.ID, "failed", 100, nil, "test", MutationMeta{SkipAudit: true}); err != nil {
		t.Fatal(err)
	}
	removed, err := service.PruneTerminalOperations(context.Background(), now.Add(7*24*time.Hour), 10)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if _, exists := service.Operation(old.ID); exists {
		t.Fatal("old terminal operation was retained")
	}
	if _, exists := service.Operation(queued.ID); !exists {
		t.Fatal("queued operation was pruned")
	}
	if _, exists := service.Operation(recent.ID); !exists {
		t.Fatal("recent terminal operation was pruned")
	}
	if _, duplicate, err := service.BeginOperation(context.Background(), Operation{ActorUserID: "operator", Type: "device.drain", TargetKind: KindDevice, TargetID: "device-old", IdempotencyKey: "old"}, MutationMeta{SkipAudit: true}); err != nil || duplicate {
		t.Fatalf("pruned idempotency key was retained: duplicate=%t err=%v", duplicate, err)
	}
}

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	service, err := NewService(context.Background(), NewDirectoryStore(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, root
}

func seedUserDevice(t *testing.T, service *Service, userID, deviceID string) (User, Device) {
	t.Helper()
	user, err := service.PutUser(context.Background(), User{ID: userID, Status: "active", Quota: ResourceQuota{MaxSessions: 4, MaxParticipants: 4}}, 0, MutationMeta{ActorUserID: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	device, err := service.RegisterDevice(context.Background(), Device{ID: deviceID, OwnerUserID: userID, Kind: "local", Name: "Mac"}, MutationMeta{ActorUserID: userID})
	if err != nil {
		t.Fatal(err)
	}
	return user, device
}

func TestServiceLifecyclePersistsAcrossRestart(t *testing.T) {
	service, root := newTestService(t)
	_, device := seedUserDevice(t, service, "owner", "device-a")
	device, err := service.HeartbeatDevice(context.Background(), device.ID, "owner", DeviceHeartbeat{ClientConnected: true, RelayRegistered: true, RuntimeRunning: true, RuntimeHealthy: true, ActiveSessions: 1}, MutationMeta{ActorUserID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if device.ObservedState != "online" {
		t.Fatalf("state=%s", device.ObservedState)
	}
	session, err := service.PutSession(context.Background(), Session{ID: "session-a", OwnerUserID: "owner", DeviceID: device.ID, ExecutionTarget: "local", AccessMode: "private", TransportMode: "auto", Status: "active"}, 0, MutationMeta{ActorUserID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutParticipant(context.Background(), Participant{SessionID: session.ID, UserID: "owner", ConnectionID: "connection-a", Role: "host", Access: "control", Transport: "relay"}, MutationMeta{ActorUserID: "owner"}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewService(context.Background(), NewDirectoryStore(root))
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	snapshot := reloaded.Snapshot()
	if len(snapshot.Users) != 1 || len(snapshot.Devices) != 1 || len(snapshot.Sessions) != 1 || len(snapshot.Participants) != 1 {
		t.Fatalf("incomplete reload: %+v", snapshot)
	}
	if len(snapshot.Audit) < 3 {
		t.Fatalf("audit not persisted: %d", len(snapshot.Audit))
	}
	overview := reloaded.Overview()
	if overview.OnlineDevices != 1 || overview.ActiveSessions != 1 || overview.Participants != 1 || overview.RelayConnections != 1 {
		t.Fatalf("bad overview: %+v", overview)
	}
}

func TestServiceOptimisticConflictAndOwnership(t *testing.T) {
	service, _ := newTestService(t)
	user, _ := seedUserDevice(t, service, "owner", "device-a")
	user.Status = "suspended"
	if _, err := service.PutUser(context.Background(), user, 0, MutationMeta{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if _, err := service.RegisterDevice(context.Background(), Device{ID: "device-a", OwnerUserID: "attacker", Kind: "local"}, MutationMeta{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ownership rejection, got %v", err)
	}
}

func TestSessionExecutionTargetMustMatchDeviceKind(t *testing.T) {
	service, _ := newTestService(t)
	_, hostDevice := seedUserDevice(t, service, "owner", "host-device")
	if _, err := service.PutSession(context.Background(), Session{
		ID:              "docker-on-host",
		OwnerUserID:     "owner",
		DeviceID:        hostDevice.ID,
		ExecutionTarget: "docker",
		AccessMode:      "private",
		TransportMode:   "relay",
		Status:          "starting",
	}, 0, MutationMeta{ActorUserID: "owner"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("docker session accepted for Host OS device: %v", err)
	}

	dockerDevice, err := service.RegisterDevice(context.Background(), Device{
		ID:          "docker-device",
		OwnerUserID: "owner",
		Kind:        "docker",
		Name:        "Container",
	}, MutationMeta{ActorUserID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutSession(context.Background(), Session{
		ID:              "host-on-docker",
		OwnerUserID:     "owner",
		DeviceID:        dockerDevice.ID,
		ExecutionTarget: "local",
		AccessMode:      "private",
		TransportMode:   "relay",
		Status:          "starting",
	}, 0, MutationMeta{ActorUserID: "owner"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Host OS session accepted for Docker device: %v", err)
	}
}

func TestServiceGrantControlsParticipantAdmission(t *testing.T) {
	service, _ := newTestService(t)
	_, device := seedUserDevice(t, service, "owner", "device-a")
	if _, err := service.PutUser(context.Background(), User{ID: "guest", Status: "active"}, 0, MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	session, err := service.PutSession(context.Background(), Session{ID: "session-a", OwnerUserID: "owner", DeviceID: device.ID, ExecutionTarget: "local", AccessMode: "shared", TransportMode: "relay", Status: "active"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	participant := Participant{SessionID: session.ID, UserID: "guest", ConnectionID: "guest-connection", Role: "participant", Access: "control"}
	if _, err := service.PutParticipant(context.Background(), participant, MutationMeta{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ungranted participant admitted: %v", err)
	}
	grant, err := service.PutGrant(context.Background(), AccessGrant{OwnerUserID: "owner", SubjectUserID: "guest", TargetDeviceID: device.ID, SessionID: session.ID, Access: "view", ExpiresAt: time.Now().Add(time.Hour)}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutParticipant(context.Background(), participant, MutationMeta{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("view grant allowed control: %v", err)
	}
	participant.Access = "view"
	joined, err := service.PutParticipant(context.Background(), participant, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if joined.Version != 1 {
		t.Fatalf("version=%d", joined.Version)
	}
	if _, err := service.RevokeGrant(context.Background(), grant.ID, grant.Version, MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	participant.ConnectionID = "guest-connection-2"
	participant.ID = ""
	if _, err := service.PutParticipant(context.Background(), participant, MutationMeta{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked grant admitted participant: %v", err)
	}
}

func TestManagedDockerSessionsAreOwnerOnly(t *testing.T) {
	service, _ := newTestService(t)
	if _, err := service.PutUser(context.Background(), User{ID: "owner", Status: "active"}, 0, MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutUser(context.Background(), User{ID: "guest", Status: "active"}, 0, MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	device, err := service.RegisterDevice(context.Background(), Device{ID: "executor-owner", OwnerUserID: "owner", Kind: "docker", Name: "Owner workspace"}, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.PutSession(context.Background(), Session{ID: "docker-session", OwnerUserID: "owner", DeviceID: device.ID, ExecutionTarget: "docker", AgentMode: "chat", AccessMode: "private", TransportMode: "relay", Status: "starting"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, allowed := service.CanAccessSession("owner", session.ID, "control", time.Now()); !allowed {
		t.Fatal("owner was denied its Docker session")
	}
	if _, allowed := service.CanAccessSession("guest", session.ID, "view", time.Now()); allowed {
		t.Fatal("non-owner was admitted to a Docker session")
	}
	if _, err := service.PutGrant(context.Background(), AccessGrant{OwnerUserID: "owner", SubjectUserID: "guest", TargetDeviceID: device.ID, SessionID: session.ID, Access: "view", ExpiresAt: time.Now().Add(time.Hour)}, 0, MutationMeta{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Docker share grant err=%v", err)
	}
	if _, err := service.PutParticipant(context.Background(), Participant{SessionID: session.ID, UserID: "guest", ConnectionID: "guest-connection", Role: "participant", Access: "view", Transport: "relay"}, MutationMeta{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Docker participant admission err=%v", err)
	}
}

func TestIntegrationUserAndConversationQuotasAreAuthoritative(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	integration, err := service.PutIntegration(ctx, Integration{ID: "partner-a", DisplayName: "Partner A", MaxUsers: 1, MaxConversationsPerUser: 1, Credential: CredentialProfile{TargetPath: ".partner/credential.json"}}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"owner-a", "owner-b"} {
		if _, err := service.PutUser(ctx, User{ID: owner, Status: "active"}, 0, MutationMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	binding, err := service.PutIntegrationUser(ctx, IntegrationUser{ID: "binding-a", IntegrationID: integration.ID, ExternalUserID: "external-a", OwnerUserID: "owner-a", Status: "ready"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutIntegrationUser(ctx, IntegrationUser{ID: "binding-b", IntegrationID: integration.ID, ExternalUserID: "external-b", OwnerUserID: "owner-b", Status: "ready"}, 0, MutationMeta{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("integration user quota err=%v", err)
	}
	device, err := service.RegisterDevice(ctx, Device{ID: "executor-owner-a", OwnerUserID: "owner-a", Kind: "docker", Name: "Executor"}, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.PutProject(ctx, Project{ID: "project-a", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: "owner-a", Name: "Project A", Locale: "ko", Status: "ready"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	for index, id := range []string{"chat-a", "chat-b"} {
		session, sessionErr := service.PutSession(ctx, Session{ID: id, OwnerUserID: "owner-a", DeviceID: device.ID, ProjectID: project.ID, ExecutionTarget: "docker", AgentMode: "chat", AccessMode: "private", TransportMode: "relay", Status: "ready"}, 0, MutationMeta{})
		if sessionErr != nil {
			t.Fatal(sessionErr)
		}
		_, conversationErr := service.PutConversation(ctx, Conversation{ID: "conversation-" + id, IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: "owner-a", DeviceID: device.ID, ProjectID: project.ID, SessionID: session.ID, Status: "ready"}, 0, MutationMeta{})
		if index == 0 && conversationErr != nil {
			t.Fatal(conversationErr)
		}
		if index == 1 && !errors.Is(conversationErr, ErrQuota) {
			t.Fatalf("conversation quota err=%v", conversationErr)
		}
	}
}

func TestServiceOperationIdempotencyAndTerminalState(t *testing.T) {
	service, _ := newTestService(t)
	op := Operation{IdempotencyKey: "request-1", ActorUserID: "operator", Type: "runtime.start", TargetKind: KindRuntime, TargetID: "runtime-a"}
	first, duplicate, err := service.BeginOperation(context.Background(), op, MutationMeta{})
	if err != nil || duplicate {
		t.Fatalf("begin=%+v duplicate=%t err=%v", first, duplicate, err)
	}
	second, duplicate, err := service.BeginOperation(context.Background(), op, MutationMeta{})
	if err != nil || !duplicate || second.ID != first.ID {
		t.Fatalf("idempotency failed: %+v duplicate=%t err=%v", second, duplicate, err)
	}
	changed := op
	changed.TargetID = "runtime-b"
	if _, _, err := service.BeginOperation(context.Background(), changed, MutationMeta{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency key accepted a different operation: %v", err)
	}
	running, err := service.UpdateOperation(context.Background(), first.ID, "running", 50, nil, "", MutationMeta{})
	if err != nil || running.StartedAt == nil {
		t.Fatalf("running=%+v err=%v", running, err)
	}
	done, err := service.UpdateOperation(context.Background(), first.ID, "succeeded", 90, map[string]any{"ok": true}, "", MutationMeta{})
	if err != nil || done.Progress != 100 || done.FinishedAt == nil {
		t.Fatalf("done=%+v err=%v", done, err)
	}
	if _, err := service.UpdateOperation(context.Background(), first.ID, "failed", 100, nil, "late", MutationMeta{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal operation changed: %v", err)
	}
}

func TestServiceConcurrentHeartbeatsRemainVersioned(t *testing.T) {
	service, _ := newTestService(t)
	_, device := seedUserDevice(t, service, "owner", "device-a")
	const count = 32
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := service.HeartbeatDevice(context.Background(), device.ID, "owner", DeviceHeartbeat{ClientConnected: true, RuntimeRunning: true, RuntimeHealthy: true, ActiveSessions: i}, MutationMeta{})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, ok := service.Device(device.ID)
	if !ok {
		t.Fatal("device missing")
	}
	if got.Version != device.Version+count {
		t.Fatalf("version=%d want=%d", got.Version, device.Version+count)
	}
	if got.ActiveSessions < 0 || got.ActiveSessions >= count {
		t.Fatalf("bad sessions=%d", got.ActiveSessions)
	}
}

func TestRelayPresenceHeartbeatIsIdempotentAndWriteThrottled(t *testing.T) {
	service, _ := newTestService(t)
	_, device := seedUserDevice(t, service, "owner", "device-a")
	session, err := service.PutSession(context.Background(), Session{ID: "session-a", OwnerUserID: "owner", DeviceID: device.ID, ExecutionTarget: "local", AccessMode: "shared", TransportMode: "relay", Status: "ready"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	connected := RelayPresence{EventID: "event-connect", NodeID: "cell-a", PublicURL: "https://relay.cookai.dev", ControlURL: "http://relay.internal:13412", Room: "owner", DeviceID: device.ID, SessionID: session.ID, RelayGeneration: session.RelayGeneration, UserID: "owner", Role: "host", Access: "control", ConnectionID: "connection-a", Kind: "host", Connected: true, HostOnline: true, At: time.Now().UTC()}
	if err := service.ApplyRelayPresence(context.Background(), connected, MutationMeta{ActorUserID: "relay", Trusted: true}); err != nil {
		t.Fatal(err)
	}
	afterConnectSession, _ := service.Session(session.ID)
	afterConnectDevice, _ := service.Device(device.ID)
	afterConnectNode, _ := service.Node("cell-a")
	auditCount := len(service.Snapshot().Audit)
	if afterConnectNode.Address != "https://relay.cookai.dev" || afterConnectNode.ControlAddress != "http://relay.internal:13412" {
		t.Fatalf("node=%+v", afterConnectNode)
	}

	heartbeat := connected
	heartbeat.EventID, heartbeat.Heartbeat = "event-heartbeat", true
	if err := service.ApplyRelayPresence(context.Background(), heartbeat, MutationMeta{ActorUserID: "relay", Trusted: true}); err != nil {
		t.Fatal(err)
	}
	if err := service.ApplyRelayPresence(context.Background(), heartbeat, MutationMeta{ActorUserID: "relay", Trusted: true}); err != nil {
		t.Fatal(err)
	}
	afterHeartbeatSession, _ := service.Session(session.ID)
	afterHeartbeatDevice, _ := service.Device(device.ID)
	afterHeartbeatNode, _ := service.Node("cell-a")
	if afterHeartbeatSession.Version != afterConnectSession.Version || afterHeartbeatDevice.Version != afterConnectDevice.Version || afterHeartbeatNode.Version != afterConnectNode.Version {
		t.Fatalf("heartbeat wrote state: session %d->%d device %d->%d node %d->%d", afterConnectSession.Version, afterHeartbeatSession.Version, afterConnectDevice.Version, afterHeartbeatDevice.Version, afterConnectNode.Version, afterHeartbeatNode.Version)
	}
	if len(service.Snapshot().Audit) != auditCount {
		t.Fatalf("heartbeat added audit events: %d -> %d", auditCount, len(service.Snapshot().Audit))
	}
}

func TestDirectoryStoreRejectsStaleWrite(t *testing.T) {
	store := NewDirectoryStore(t.TempDir())
	record, _ := makeRecord(KindUser, "u", 1, User{ID: "u", Version: 1})
	if err := store.Put(context.Background(), record, 0); err != nil {
		t.Fatal(err)
	}
	stale, _ := makeRecord(KindUser, "u", 1, User{ID: "u", Version: 1})
	if err := store.Put(context.Background(), stale, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write=%v", err)
	}
	if err := store.Delete(context.Background(), KindUser, "u", 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete=%v", err)
	}
	if err := store.Delete(context.Background(), KindUser, "u", 1); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkServiceHeartbeat(b *testing.B) {
	service, err := NewService(context.Background(), NewDirectoryStore(b.TempDir()))
	if err != nil {
		b.Fatal(err)
	}
	defer service.Close()
	_, _ = service.PutUser(context.Background(), User{ID: "owner", Status: "active"}, 0, MutationMeta{})
	for i := 0; i < 32; i++ {
		_, _ = service.RegisterDevice(context.Background(), Device{ID: fmt.Sprintf("device-%d", i), OwnerUserID: "owner", Kind: "local"}, MutationMeta{})
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_, _ = service.HeartbeatDevice(context.Background(), fmt.Sprintf("device-%d", i%32), "owner", DeviceHeartbeat{ClientConnected: true}, MutationMeta{})
			i++
		}
	})
}
