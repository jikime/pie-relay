package thirdparty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/credential"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

type testRuntime struct {
	mu       sync.Mutex
	ensures  map[string]int
	stops    map[string]int
	projects []manager.ProjectSpec
	apps     map[string][]manager.ProjectApplication
}

func (r *testRuntime) Ensure(_ context.Context, value manager.Executor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensures[value.UserID]++
	return nil
}
func (*testRuntime) Run(context.Context, manager.Job) ([]byte, error) { return nil, nil }
func (r *testRuntime) Stop(_ context.Context, value manager.Executor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops[value.UserID]++
	return nil
}
func (r *testRuntime) InitializeProject(_ context.Context, _ manager.Executor, spec manager.ProjectSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.projects = append(r.projects, spec)
	return nil
}
func (r *testRuntime) DiscoverProjectApplications(_ context.Context, _ manager.Executor, projectID string) ([]manager.ProjectApplication, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]manager.ProjectApplication(nil), r.apps[projectID]...), nil
}

type testManagerStore struct {
	mu        sync.Mutex
	executors map[string]manager.Executor
	jobs      map[string]manager.Job
}

func (s *testManagerStore) Load(context.Context) ([]manager.Executor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]manager.Executor, 0, len(s.executors))
	for _, value := range s.executors {
		values = append(values, value)
	}
	return values, nil
}
func (s *testManagerStore) LoadJobs(context.Context) ([]manager.Job, error) {
	return nil, nil
}
func (s *testManagerStore) SaveExecutor(_ context.Context, value manager.Executor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executors[value.UserID] = value
	return nil
}
func (s *testManagerStore) SaveJob(_ context.Context, value manager.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[value.ID] = value
	return nil
}

func newThirdPartyTestService(t *testing.T) (*Service, *control.Service, *testRuntime, string) {
	t.Helper()
	registryRoot, stateRoot := t.TempDir(), t.TempDir()
	controlService, err := control.NewService(context.Background(), control.NewDirectoryStore(registryRoot))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{ensures: map[string]int{}, stops: map[string]int{}, apps: map[string][]manager.ProjectApplication{}}
	executors, err := manager.New(context.Background(), runtime, &testManagerStore{executors: map[string]manager.Executor{}, jobs: map[string]manager.Job{}}, 8)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := credential.New(stateRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		executors.Close()
		_ = controlService.Close()
	})
	return &Service{Control: controlService, Executors: executors, Credentials: credentials, Random: bytes.NewReader(make([]byte, 64))}, controlService, runtime, stateRoot
}

func TestIntegrationTokenIsOneTimeAndAuthenticatedByDigest(t *testing.T) {
	service, controlService, _, _ := newThirdPartyTestService(t)
	registered, err := service.Register(context.Background(), control.Integration{ID: "partner-a", DisplayName: "Partner A", Credential: control.CredentialProfile{TargetPath: ".partner/credential.json"}}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if registered.ServiceToken == "" {
		t.Fatal("service token was not returned")
	}
	secret, ok := controlService.IntegrationSecret("partner-a")
	if !ok || secret.TokenHash == "" || secret.TokenHash == registered.ServiceToken {
		t.Fatalf("stored secret=%+v", secret)
	}
	encoded, err := json.Marshal(controlService.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(registered.ServiceToken)) || bytes.Contains(encoded, []byte(secret.TokenHash)) {
		t.Fatalf("snapshot exposed integration secret: %s", encoded)
	}
	if _, err := service.Authenticate("partner-a", registered.ServiceToken); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if _, err := service.Authenticate("partner-a", "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong token err=%v", err)
	}
}

func TestRotateTokenImmediatelyInvalidatesOldToken(t *testing.T) {
	service, _, _, _ := newThirdPartyTestService(t)
	service.Random = bytes.NewReader(append(make([]byte, 32), bytes.Repeat([]byte{1}, 32)...))
	registered, err := service.Register(context.Background(), control.Integration{ID: "partner-a", DisplayName: "Partner A", Credential: control.CredentialProfile{TargetPath: ".partner/credential.json"}}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := service.RotateToken(context.Background(), registered.Integration.ID)
	if err != nil || rotated == registered.ServiceToken {
		t.Fatalf("rotated=%q err=%v", rotated, err)
	}
	if _, err := service.Authenticate(registered.Integration.ID, registered.ServiceToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old token err=%v", err)
	}
	if _, err := service.Authenticate(registered.Integration.ID, rotated); err != nil {
		t.Fatalf("rotated token err=%v", err)
	}
}

func TestProvisionIsIdempotentIsolatedAndMaterializesCredential(t *testing.T) {
	service, controlService, runtime, stateRoot := newThirdPartyTestService(t)
	partnerA, err := service.Register(context.Background(), control.Integration{ID: "partner-a", DisplayName: "Partner A", Credential: control.CredentialProfile{TargetPath: ".partner/credential.json"}}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	partnerB, err := service.Register(context.Background(), control.Integration{ID: "partner-b", DisplayName: "Partner B", Credential: control.CredentialProfile{TargetPath: ".partner/credential.json"}}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	credentialJSON := json.RawMessage(`{"pat":"do-not-log-me","endpoint":"https://partner.example"}`)
	first, err := service.Provision(context.Background(), partnerA.Integration, "same-external-user", credentialJSON, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Provision(context.Background(), partnerA.Integration, "same-external-user", credentialJSON, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.Provision(context.Background(), partnerB.Integration, "same-external-user", credentialJSON, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.OwnerUserID != second.OwnerUserID {
		t.Fatalf("provision was not stable: first=%+v second=%+v", first, second)
	}
	if first.OwnerUserID == other.OwnerUserID {
		t.Fatalf("integration namespaces collided: A=%s B=%s", first.OwnerUserID, other.OwnerUserID)
	}
	runtime.mu.Lock()
	ensureCount := runtime.ensures[first.OwnerUserID]
	runtime.mu.Unlock()
	if ensureCount != 1 {
		t.Fatalf("same user created runtime %d times", ensureCount)
	}
	device, ok := controlService.Device("executor-" + first.OwnerUserID)
	if !ok || device.Kind != "docker" || device.OwnerUserID != first.OwnerUserID {
		t.Fatalf("Docker device=%+v ok=%t", device, ok)
	}
	path := filepath.Join(stateRoot, first.OwnerUserID, ".partner", "credential.json")
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, credentialJSON) {
		t.Fatalf("credential data=%q err=%v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credential mode=%v", info.Mode().Perm())
	}
	if first.CredentialVersion != 1 || second.CredentialVersion != 1 {
		t.Fatalf("credential versions first=%d second=%d", first.CredentialVersion, second.CredentialVersion)
	}
}

func TestSuspendRemovesRuntimeAndCredentialAndCanReactivate(t *testing.T) {
	service, _, runtime, stateRoot := newThirdPartyTestService(t)
	registered, err := service.Register(context.Background(), control.Integration{ID: "partner-a", DisplayName: "Partner A", Credential: control.CredentialProfile{TargetPath: ".partner/credential.json"}}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := service.Provision(context.Background(), registered.Integration, "user-a", json.RawMessage(`{"pat":"secret"}`), control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	suspended, err := service.Suspend(context.Background(), registered.Integration, binding, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if suspended.Status != "suspended" || suspended.CredentialDigest != "" {
		t.Fatalf("suspended binding=%+v", suspended)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, binding.OwnerUserID, ".partner", "credential.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential remains: %v", err)
	}
	runtime.mu.Lock()
	stopCount := runtime.stops[binding.OwnerUserID]
	runtime.mu.Unlock()
	if stopCount != 1 {
		t.Fatalf("runtime stopped %d times", stopCount)
	}
	reactivated, err := service.Provision(context.Background(), registered.Integration, "user-a", json.RawMessage(`{"pat":"new"}`), control.MutationMeta{})
	if err != nil || reactivated.Status != "ready" || reactivated.OwnerUserID != binding.OwnerUserID {
		t.Fatalf("reactivated=%+v err=%v", reactivated, err)
	}
}

func TestCreateProjectIsIdempotentAndBindsAContainerWorkingDirectory(t *testing.T) {
	service, controlService, runtime, _ := newThirdPartyTestService(t)
	registered, err := service.Register(context.Background(), control.Integration{ID: "partner-a", DisplayName: "Partner A", Credential: control.CredentialProfile{TargetPath: ".partner/credential.json"}}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := service.Provision(context.Background(), registered.Integration, "user-a", json.RawMessage(`{"pat":"secret"}`), control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.CreateProject(context.Background(), registered.Integration, binding, "project-stable", "쇼핑몰 관리자", "ko", control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateProject(context.Background(), registered.Integration, binding, "project-stable", "쇼핑몰 관리자", "ko", control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Status != "ready" || first.WorkingDir != "/workspace/projects/project-stable" || first.InitializedAt == nil {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	runtime.mu.Lock()
	projects := append([]manager.ProjectSpec(nil), runtime.projects...)
	runtime.mu.Unlock()
	if len(projects) != 1 || projects[0].ID != first.ID || projects[0].Name != first.Name {
		t.Fatalf("runtime projects=%+v", projects)
	}
	stored, ok := controlService.Project(first.ID)
	if !ok || stored.Status != "ready" {
		t.Fatalf("stored=%+v ok=%t", stored, ok)
	}
	if _, err := service.CreateProject(context.Background(), registered.Integration, binding, "project-stable", "다른 이름", "ko", control.MutationMeta{}); !errors.Is(err, control.ErrConflict) {
		t.Fatalf("idempotency conflict err=%v", err)
	}
}

func TestProjectApplicationSelectionIsDiscoveredAndPersisted(t *testing.T) {
	service, controlService, runtime, _ := newThirdPartyTestService(t)
	registered, err := service.Register(context.Background(), control.Integration{ID: "partner-a", DisplayName: "Partner A", Credential: control.CredentialProfile{TargetPath: ".partner/credential.json"}}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := service.Provision(context.Background(), registered.Integration, "user-a", json.RawMessage(`{"pat":"secret"}`), control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(context.Background(), registered.Integration, binding, "project-stable", "쇼핑몰 관리자", "ko", control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.apps[project.ID] = []manager.ProjectApplication{
		{Path: "apps/admin", Name: "Admin", Profile: "next"},
		{Path: "apps/store", Name: "Store", Profile: "vite"},
	}
	runtime.mu.Unlock()
	applications, err := service.ProjectApplications(context.Background(), registered.Integration, binding, project.ID)
	if err != nil || len(applications) != 2 {
		t.Fatalf("applications=%+v err=%v", applications, err)
	}
	selected, err := service.SelectProjectApplication(context.Background(), registered.Integration, binding, project.ID, "apps/store", control.MutationMeta{})
	if err != nil || selected.PreviewAppPath != "apps/store" {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
	stored, ok := controlService.Project(project.ID)
	if !ok || stored.PreviewAppPath != "apps/store" {
		t.Fatalf("stored=%+v ok=%t", stored, ok)
	}
	if _, err := service.SelectProjectApplication(context.Background(), registered.Integration, binding, project.ID, "apps/missing", control.MutationMeta{}); !errors.Is(err, control.ErrInvalid) {
		t.Fatalf("unknown application err=%v", err)
	}
}

func TestConcurrentProvisionReturnsOneStableExecutor(t *testing.T) {
	service, _, runtime, _ := newThirdPartyTestService(t)
	registered, err := service.Register(context.Background(), control.Integration{ID: "partner-a", DisplayName: "Partner A", Credential: control.CredentialProfile{TargetPath: ".partner/credential.json"}}, control.MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 24
	results := make(chan control.IntegrationUser, workers)
	errorsCh := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			value, provisionErr := service.Provision(context.Background(), registered.Integration, "concurrent-user", json.RawMessage(`{"pat":"stable"}`), control.MutationMeta{})
			results <- value
			errorsCh <- provisionErr
		}()
	}
	group.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent provision: %v", err)
		}
	}
	owner := ""
	for result := range results {
		if result.Status != "ready" || result.CredentialVersion != 1 {
			t.Fatalf("result=%+v", result)
		}
		if owner == "" {
			owner = result.OwnerUserID
		} else if owner != result.OwnerUserID {
			t.Fatalf("owners differ: %s != %s", owner, result.OwnerUserID)
		}
	}
	runtime.mu.Lock()
	ensureCount := runtime.ensures[owner]
	runtime.mu.Unlock()
	if ensureCount != 1 {
		t.Fatalf("runtime Ensure called %d times", ensureCount)
	}
}
