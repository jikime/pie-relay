package preview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

type previewRuntime struct {
	mu            sync.Mutex
	previews      map[string]manager.PreviewObservation
	startFailures int
	stopDelay     time.Duration
	lastStartSpec manager.PreviewSpec
}

func (r *previewRuntime) Ensure(context.Context, manager.Executor) error   { return nil }
func (r *previewRuntime) Run(context.Context, manager.Job) ([]byte, error) { return nil, nil }
func (r *previewRuntime) Stop(context.Context, manager.Executor) error     { return nil }
func (r *previewRuntime) EnsurePreviewNetwork(context.Context, manager.Executor, string) (string, error) {
	return "preview-backend-test", nil
}
func (r *previewRuntime) StartPreview(_ context.Context, _ manager.Executor, spec manager.PreviewSpec) error {
	r.mu.Lock()
	if r.startFailures > 0 {
		r.startFailures--
		r.mu.Unlock()
		return errors.New("temporary executor error")
	}
	r.previews[spec.ID] = manager.PreviewObservation{ID: spec.ID, State: "running", Ready: true}
	r.lastStartSpec = spec
	r.mu.Unlock()
	return nil
}

func TestTransientStartFailureIsRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &previewRuntime{previews: map[string]manager.PreviewObservation{}, startFailures: 1}
	executorManager, err := manager.NewWithOptions(ctx, runtime, &previewManagerStore{}, manager.Options{QueueCapacity: 2, Workers: 1, ProvisionConcurrency: 1, MaxExecutors: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(executorManager.Close)
	if _, err := executorManager.Ensure(ctx, "owner-a"); err != nil {
		t.Fatal(err)
	}
	controlService, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlService.Close() })
	integration, binding, project := seedPreviewControl(t, controlService)
	service, err := New(ctx, controlService, executorManager, Options{Domain: "preview.kroot.io", GatewayContainer: "preview-gateway", AccessSecret: []byte(strings.Repeat("s", 32)), ReconcileEvery: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if _, _, err := service.Create(ctx, CreateRequest{ID: "preview-retry", Integration: integration, Binding: binding, Project: project, Profile: "next", TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		value, _ := controlService.Preview("preview-retry")
		if value.Status == "ready" {
			if value.StartAttempts != 0 || value.NextRetryAt != nil {
				t.Fatalf("retry state was not cleared: %+v", value)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	value, _ := controlService.Preview("preview-retry")
	t.Fatalf("preview did not recover from a transient failure: %+v", value)
}
func (r *previewRuntime) StopPreview(_ context.Context, _ manager.Executor, id string) error {
	if r.stopDelay > 0 {
		time.Sleep(r.stopDelay)
	}
	r.mu.Lock()
	delete(r.previews, id)
	r.mu.Unlock()
	return nil
}
func (r *previewRuntime) ObservePreviews(context.Context, manager.Executor) ([]manager.PreviewObservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]manager.PreviewObservation, 0, len(r.previews))
	for _, value := range r.previews {
		out = append(out, value)
	}
	return out, nil
}
func (r *previewRuntime) PreviewLogs(context.Context, manager.Executor, string) ([]byte, error) {
	return []byte("preview log\n"), nil
}

type previewManagerStore struct {
	mu        sync.Mutex
	executors map[string]manager.Executor
	jobs      map[string]manager.Job
}

func (s *previewManagerStore) Load(context.Context) ([]manager.Executor, error) { return nil, nil }
func (s *previewManagerStore) LoadJobs(context.Context) ([]manager.Job, error)  { return nil, nil }
func (s *previewManagerStore) SaveExecutor(_ context.Context, value manager.Executor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.executors == nil {
		s.executors = map[string]manager.Executor{}
	}
	s.executors[value.UserID] = value
	return nil
}
func (s *previewManagerStore) SaveJob(_ context.Context, value manager.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		s.jobs = map[string]manager.Job{}
	}
	s.jobs[value.ID] = value
	return nil
}

func TestCreateReconcileRouteLaunchAndStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &previewRuntime{previews: map[string]manager.PreviewObservation{}}
	executorManager, err := manager.NewWithOptions(ctx, runtime, &previewManagerStore{}, manager.Options{QueueCapacity: 2, Workers: 1, ProvisionConcurrency: 1, MaxExecutors: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(executorManager.Close)
	if _, err := executorManager.Ensure(ctx, "owner-a"); err != nil {
		t.Fatal(err)
	}
	// Preview creation must wake an Executor that was previously reclaimed by
	// the idle lifecycle. This is the normal production path for an inactive
	// user returning to an existing project.
	if _, err := executorManager.StopExecutor(ctx, "owner-a"); err != nil {
		t.Fatal(err)
	}
	controlService, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlService.Close() })
	integration, binding, project := seedPreviewControl(t, controlService)
	service, err := New(ctx, controlService, executorManager, Options{Domain: "preview.kroot.io", PublicScheme: "https", PublicPort: 18443, GatewayContainer: "preview-gateway", AccessSecret: []byte(strings.Repeat("s", 32)), ReconcileEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	launch, duplicate, err := service.Create(ctx, CreateRequest{ID: "preview-a", Integration: integration, Binding: binding, Project: project, AppPath: "apps/web", Profile: "next", Visibility: "private", TTL: time.Hour})
	if err != nil || duplicate || launch.AccessURL == "" || !strings.HasPrefix(launch.Preview.Hostname, "p-") {
		t.Fatalf("launch=%+v duplicate=%v err=%v", launch, duplicate, err)
	}
	if executor, ok := executorManager.Executor("owner-a"); !ok || executor.Status != "ready" {
		t.Fatalf("preview creation did not wake the stopped executor: %+v ok=%v", executor, ok)
	}
	if !strings.HasPrefix(launch.URL, "https://"+launch.Preview.Hostname+":18443/") {
		t.Fatalf("local preview URL did not include configured public port: %s", launch.URL)
	}
	deadline := time.Now().Add(2 * time.Second)
	var value control.Preview
	for time.Now().Before(deadline) {
		value, _ = controlService.Preview("preview-a")
		if value.Status == "ready" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if value.Status != "ready" {
		t.Fatalf("preview did not reconcile: %+v", value)
	}
	runtime.mu.Lock()
	startSpec := runtime.lastStartSpec
	runtime.mu.Unlock()
	if startSpec.ProjectID != project.ID || startSpec.WorkingDir != "/workspace/projects/"+project.ID+"/apps/web" {
		t.Fatalf("unexpected preview start spec: %+v", startSpec)
	}
	route, ok := service.Route(value.Hostname)
	if !ok || route.Backend != "preview-backend-test" || route.Port != value.Port {
		t.Fatalf("route=%+v ok=%v", route, ok)
	}
	logs, err := service.Logs(ctx, value)
	if err != nil || string(logs) != "preview log\n" {
		t.Fatalf("logs=%q err=%v", logs, err)
	}
	reused, duplicate, err := service.Create(ctx, CreateRequest{ID: "preview-another-request", Integration: integration, Binding: binding, Project: project, AppPath: "apps/web", Profile: "next", Visibility: "public", TTL: time.Hour})
	if err != nil || !duplicate || reused.Preview.ID != value.ID || reused.Preview.Hostname != value.Hostname || reused.Preview.Port != value.Port {
		t.Fatalf("singleton reuse=%+v duplicate=%v err=%v", reused, duplicate, err)
	}
	publicLaunch, err := service.SetVisibility(ctx, value, "public", control.MutationMeta{})
	if err != nil || publicLaunch.Preview.Hostname != value.Hostname || publicLaunch.Preview.Port != value.Port || publicLaunch.Preview.AccessVersion != value.AccessVersion+1 || publicLaunch.AccessURL != "" {
		t.Fatalf("public visibility launch=%+v err=%v", publicLaunch, err)
	}
	privateLaunch, err := service.SetVisibility(ctx, publicLaunch.Preview, "private", control.MutationMeta{})
	if err != nil || privateLaunch.Preview.Hostname != value.Hostname || privateLaunch.Preview.Port != value.Port || privateLaunch.Preview.AccessVersion != publicLaunch.Preview.AccessVersion+1 || privateLaunch.AccessURL == "" {
		t.Fatalf("private visibility launch=%+v err=%v", privateLaunch, err)
	}
	value = privateLaunch.Preview
	stopped, err := service.Stop(ctx, value, control.MutationMeta{})
	if err != nil || stopped.Status != "stopped" {
		t.Fatalf("stopped=%+v err=%v", stopped, err)
	}
	if _, ok := service.Route(value.Hostname); ok {
		t.Fatal("stopped preview remained routable")
	}
	_, duplicate, err = service.Create(ctx, CreateRequest{ID: "preview-a", Integration: integration, Binding: binding, Project: project})
	if err != nil || !duplicate {
		t.Fatalf("idempotent create duplicate=%v err=%v", duplicate, err)
	}
	deleted, err := service.Delete(ctx, stopped, control.MutationMeta{ActorUserID: "owner-a"})
	if err != nil || deleted.ID != stopped.ID {
		t.Fatalf("delete stopped preview=%+v err=%v", deleted, err)
	}
	if _, ok := controlService.Preview(stopped.ID); ok {
		t.Fatal("deleted stopped preview remained in control")
	}
	createdAgain, duplicate, err := service.Create(ctx, CreateRequest{ID: "preview-after-delete", Integration: integration, Binding: binding, Project: project, AppPath: "apps/web", Profile: "next", Visibility: "private", TTL: time.Hour})
	if err != nil || duplicate || createdAgain.Preview.Hostname == stopped.Hostname {
		t.Fatalf("create after delete=%+v duplicate=%v err=%v", createdAgain, duplicate, err)
	}
	if _, err := service.Delete(ctx, createdAgain.Preview, control.MutationMeta{ActorUserID: "owner-a"}); err != nil {
		t.Fatalf("delete running preview: %v", err)
	}
	if _, ok := controlService.Preview(createdAgain.Preview.ID); ok {
		t.Fatal("deleted running preview remained in control")
	}
	expires := time.Now().UTC().Add(time.Hour)
	failed, err := controlService.PutPreview(ctx, control.Preview{
		ID: "preview-failed-delete", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: binding.OwnerUserID,
		ProjectID: project.ID, AppPath: "apps/web", Hostname: "p-cccccccccccccccccccccccccc.preview.kroot.io",
		BackendHost: "preview-backend-test", Port: 20000, Profile: "next", Visibility: "private", Status: "failed", ExpiresAt: &expires,
	}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.previews[failed.ID] = manager.PreviewObservation{ID: failed.ID, State: "failed"}
	runtime.mu.Unlock()
	if _, err := service.Delete(ctx, failed, control.MutationMeta{ActorUserID: "owner-a"}); err != nil {
		t.Fatalf("delete failed preview: %v", err)
	}
	if _, ok := controlService.Preview(failed.ID); ok {
		t.Fatal("deleted failed preview remained in control")
	}
	runtime.mu.Lock()
	_, runtimeExists := runtime.previews[failed.ID]
	runtime.mu.Unlock()
	if runtimeExists {
		t.Fatal("failed preview runtime was not cleaned before record deletion")
	}
}

func TestConcurrentRestartAcrossPreviewServicesIsSerialized(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &previewRuntime{previews: map[string]manager.PreviewObservation{}, stopDelay: 50 * time.Millisecond}
	executorManager, err := manager.NewWithOptions(ctx, runtime, &previewManagerStore{}, manager.Options{QueueCapacity: 2, Workers: 1, ProvisionConcurrency: 1, MaxExecutors: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(executorManager.Close)
	if _, err := executorManager.Ensure(ctx, "owner-a"); err != nil {
		t.Fatal(err)
	}
	controlService, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlService.Close() })
	integration, binding, project := seedPreviewControl(t, controlService)
	options := Options{Domain: "preview.kroot.io", GatewayContainer: "preview-gateway", AccessSecret: []byte(strings.Repeat("s", 32)), ReconcileEvery: time.Hour}
	serviceA, err := New(ctx, controlService, executorManager, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(serviceA.Close)
	created, _, err := serviceA.Create(ctx, CreateRequest{ID: "preview-concurrent", Integration: integration, Binding: binding, Project: project, Profile: "next", Visibility: "private", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		value, _ := controlService.Preview(created.Preview.ID)
		if value.Status == "ready" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	value, _ := controlService.Preview(created.Preview.ID)
	if value.Status != "ready" {
		t.Fatalf("preview did not become ready: %+v", value)
	}
	serviceB, err := New(ctx, controlService, executorManager, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(serviceB.Close)

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, service := range []*Service{serviceA, serviceB} {
		go func(service *Service) {
			<-start
			launch, restartErr := service.Restart(ctx, value, control.MutationMeta{})
			if restartErr == nil && launch.Preview.Hostname != value.Hostname {
				restartErr = errors.New("restart changed the preview hostname")
			}
			results <- restartErr
		}(service)
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent restart failed: %v", err)
		}
	}
}

func TestConcurrentCreatesForOneProjectAppReuseSinglePreview(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &previewRuntime{previews: map[string]manager.PreviewObservation{}}
	executorManager, err := manager.NewWithOptions(ctx, runtime, &previewManagerStore{}, manager.Options{QueueCapacity: 2, Workers: 1, ProvisionConcurrency: 1, MaxExecutors: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(executorManager.Close)
	if _, err := executorManager.Ensure(ctx, "owner-a"); err != nil {
		t.Fatal(err)
	}
	controlService, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlService.Close() })
	integration, binding, project := seedPreviewControl(t, controlService)
	options := Options{Domain: "preview.kroot.io", GatewayContainer: "preview-gateway", AccessSecret: []byte(strings.Repeat("s", 32)), ReconcileEvery: time.Hour}
	serviceA, err := New(ctx, controlService, executorManager, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(serviceA.Close)
	serviceB, err := New(ctx, controlService, executorManager, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(serviceB.Close)
	type result struct {
		launch    Launch
		duplicate bool
		err       error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for index, service := range []*Service{serviceA, serviceB} {
		go func(index int, service *Service) {
			<-start
			launch, duplicate, createErr := service.Create(ctx, CreateRequest{
				ID: fmt.Sprintf("preview-create-%d", index), Integration: integration, Binding: binding,
				Project: project, AppPath: "apps/web", Profile: "next", Visibility: "private", TTL: time.Hour,
			})
			results <- result{launch: launch, duplicate: duplicate, err: createErr}
		}(index, service)
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent create errors: %v, %v", first.err, second.err)
	}
	if first.launch.Preview.ID != second.launch.Preview.ID || first.launch.Preview.Hostname != second.launch.Preview.Hostname || first.launch.Preview.Port != second.launch.Preview.Port {
		t.Fatalf("creates did not reuse one preview: %+v %+v", first.launch.Preview, second.launch.Preview)
	}
	if first.duplicate == second.duplicate {
		t.Fatalf("expected exactly one new and one reused result: duplicate=%v,%v", first.duplicate, second.duplicate)
	}
	active := 0
	for _, value := range controlService.PreviewsForProject(project.ID, 0) {
		if value.Status != "stopped" && value.Status != "failed" {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active previews=%d, want 1", active)
	}
}

func TestRouteIsRevokedWhenIntegrationUserIsSuspended(t *testing.T) {
	ctx := context.Background()
	controlService, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlService.Close() })
	integration, binding, project := seedPreviewControl(t, controlService)
	expires := time.Now().Add(time.Hour)
	value, err := controlService.PutPreview(ctx, control.Preview{ID: "preview-a", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: binding.OwnerUserID, ProjectID: project.ID, Hostname: "p-aaaaaaaaaaaaaaaaaaaaaaaaaa.preview.kroot.io", BackendHost: "preview-backend-test", Port: 20000, Profile: "next", Visibility: "private", Status: "ready", ExpiresAt: &expires}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{control: controlService}
	if _, ok := service.Route(value.Hostname); !ok {
		t.Fatal("ready preview was not routable")
	}
	binding.Status = "suspended"
	if _, err := controlService.PutIntegrationUser(ctx, binding, binding.Version, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := service.Route(value.Hostname); ok {
		t.Fatal("suspended integration user retained preview access")
	}
}

func TestAllocatePortReusesStoppedPreviewPorts(t *testing.T) {
	ctx := context.Background()
	controlService, err := control.NewService(ctx, control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlService.Close() })
	integration, binding, project := seedPreviewControl(t, controlService)
	for _, value := range []control.Preview{
		{ID: "preview-stopped", Hostname: "p-aaaaaaaaaaaaaaaaaaaaaaaaaa.preview.kroot.io", Port: 20000, Status: "stopped"},
		{ID: "preview-active", Hostname: "p-bbbbbbbbbbbbbbbbbbbbbbbbbb.preview.kroot.io", Port: 20001, Status: "ready"},
	} {
		value.IntegrationID = integration.ID
		value.IntegrationUserID = binding.ID
		value.OwnerUserID = binding.OwnerUserID
		value.ProjectID = project.ID
		value.BackendHost = "preview-backend-test"
		value.Profile = "next"
		value.Visibility = "private"
		if _, err := controlService.PutPreview(ctx, value, 0, control.MutationMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{control: controlService}
	port, err := service.allocatePort(binding.ID)
	if err != nil || port != 20000 {
		t.Fatalf("port=%d err=%v", port, err)
	}
}

func seedPreviewControl(t *testing.T, service *control.Service) (control.Integration, control.IntegrationUser, control.Project) {
	t.Helper()
	ctx := context.Background()
	integration, err := service.PutIntegration(ctx, control.Integration{ID: "integration-a", DisplayName: "Integration A", Status: "active", MaxUsers: 10, MaxProjectsPerUser: 10, MaxPreviewsPerUser: 4, MaxConversationsPerUser: 2, Credential: control.CredentialProfile{TargetPath: ".pie/credential.json", Format: "json", MaxBytes: 1024}}, 0, control.MutationMeta{})
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
	project, err := service.PutProject(ctx, control.Project{ID: "project-a", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: "owner-a", Name: "Project A", Locale: "ko", Status: "ready"}, 0, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	return integration, binding, project
}
