package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/capability"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/chatgateway"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

type lifecycleTestRuntime struct {
	mu              sync.Mutex
	stoppedSessions []string
	stoppedUsers    []string
}

func (*lifecycleTestRuntime) Ensure(context.Context, manager.Executor) error   { return nil }
func (*lifecycleTestRuntime) Run(context.Context, manager.Job) ([]byte, error) { return nil, nil }
func (r *lifecycleTestRuntime) Stop(_ context.Context, executor manager.Executor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stoppedUsers = append(r.stoppedUsers, executor.UserID)
	return nil
}
func (*lifecycleTestRuntime) StartSession(context.Context, manager.Executor, manager.SessionSpec) error {
	return nil
}
func (r *lifecycleTestRuntime) StopSession(_ context.Context, _ manager.Executor, sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stoppedSessions = append(r.stoppedSessions, sessionID)
	return nil
}
func (*lifecycleTestRuntime) ObserveSessions(context.Context, manager.Executor) ([]manager.SessionObservation, error) {
	return nil, nil
}

func TestChatLifecycleClosesIdleSessionAndRetryPreservesConversation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &lifecycleTestRuntime{}
	m, err := manager.New(ctx, runtime, &apiTestStore{}, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if _, err := m.Ensure(ctx, "owner-a"); err != nil {
		t.Fatal(err)
	}
	service, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	conversation := createLifecycleConversation(t, service, time.Now().Add(-time.Hour))
	journal, err := chatgateway.NewJournal(t.TempDir(), 1<<20, 1<<18)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := chatgateway.New(ctx, service, capability.Issuer{Secret: []byte("01234567890123456789012345678901")}, "ws://127.0.0.1:1/ws/agent", journal)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newChatLifecycle(m, service, gateway, chatLifecycleOptions{ScanInterval: time.Minute, SessionIdleTimeout: 15 * time.Minute, ExecutorIdleTimeout: time.Hour})
	if err := lifecycle.Sweep(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	closedConversation, _ := service.Conversation(conversation.ID)
	closedSession, _ := service.Session(conversation.SessionID)
	if closedConversation.Status != "closed" || closedSession.Status != "closed" {
		t.Fatalf("conversation=%s session=%s", closedConversation.Status, closedSession.Status)
	}
	if lifecycle.Stats().SessionsClosed != 1 {
		t.Fatalf("unexpected lifecycle stats: %+v", lifecycle.Stats())
	}
	if events, err := gateway.Events(ctx, conversation.ID, 0, 10); err != nil || len(events) != 1 || events[0].Type != "conversation.idle" {
		t.Fatalf("idle event=%+v err=%v", events, err)
	}
	// Use a future cutoff to verify the pure eligibility rule without waiting
	// for the freshly-created Project protection window to elapse.
	if !executorIdle(service, manager.Executor{UserID: "owner-a", LastUsedAt: time.Now().Add(-2 * time.Hour)}, time.Now().Add(time.Hour)) {
		t.Fatal("closed idle conversation should permit executor cleanup")
	}

	a := api{m: m, control: service, chat: gateway}
	retried, err := a.retryIntegrationConversation(ctx, closedConversation, control.MutationMeta{ActorUserID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	retriedSession, _ := service.Session(conversation.SessionID)
	if retried.ID != conversation.ID || retried.Status != "connecting" || retriedSession.Status != "starting" {
		t.Fatalf("retried=%+v session=%+v", retried, retriedSession)
	}
}

func TestCloseIntegrationRecordsIsIdempotentAndClearsSessionLease(t *testing.T) {
	ctx := context.Background()
	service, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	conversation := createLifecycleConversation(t, service, time.Now())
	session, _ := service.Session(conversation.SessionID)
	expires := time.Now().Add(time.Minute)
	session.Status, session.HostConnectionID, session.DriverUserID, session.DriverLeaseExpiresAt = "active", "host-a", "driver-a", &expires
	if _, err := service.PutSession(ctx, session, session.Version, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	meta := control.MutationMeta{ActorUserID: "integration-test"}
	if err := closeIntegrationSession(ctx, service, conversation.SessionID, meta); err != nil {
		t.Fatal(err)
	}
	closed, err := closeIntegrationConversation(ctx, service, conversation.ID, meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeIntegrationSession(ctx, service, conversation.SessionID, meta); err != nil {
		t.Fatalf("idempotent session close: %v", err)
	}
	if _, err := closeIntegrationConversation(ctx, service, conversation.ID, meta); err != nil {
		t.Fatalf("idempotent conversation close: %v", err)
	}
	closedSession, _ := service.Session(conversation.SessionID)
	if closed.Status != "closed" || closedSession.Status != "closed" || closedSession.HostConnectionID != "" || closedSession.DriverUserID != "" || closedSession.DriverLeaseExpiresAt != nil {
		t.Fatalf("conversation=%+v session=%+v", closed, closedSession)
	}
}

func createLifecycleConversation(t *testing.T, service *control.Service, activity time.Time) control.Conversation {
	t.Helper()
	ctx := context.Background()
	integration, err := service.PutIntegration(ctx, control.Integration{ID: "partner-a", DisplayName: "Partner A", MaxUsers: 10, MaxConversationsPerUser: 4, Credential: control.CredentialProfile{TargetPath: ".partner/credential.json"}}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutUser(ctx, control.User{ID: "owner-a", Status: "active"}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	binding, err := service.PutIntegrationUser(ctx, control.IntegrationUser{ID: "binding-a", IntegrationID: integration.ID, ExternalUserID: "external-a", OwnerUserID: "owner-a", Status: "ready"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	device, err := service.RegisterDevice(ctx, control.Device{ID: "executor-owner-a", OwnerUserID: "owner-a", Kind: "docker", Name: "Executor"}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.PutProject(ctx, control.Project{ID: "project-a", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: "owner-a", Name: "Project A", Locale: "ko", Status: "ready"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.PutSession(ctx, control.Session{ID: "chat-a", OwnerUserID: "owner-a", DeviceID: device.ID, ProjectID: project.ID, ExecutionTarget: "docker", AgentMode: "chat", AccessMode: "private", TransportMode: "relay", Status: "ready"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := service.PutConversation(ctx, control.Conversation{ID: "conversation-a", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: "owner-a", DeviceID: device.ID, ProjectID: project.ID, SessionID: session.ID, Status: "ready", LastActivityAt: activity}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}
