package manager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu        sync.Mutex
	executors []Executor
	jobs      []Job
	deleted   []string
}

func (s *memoryStore) Load(context.Context) ([]Executor, error) {
	return append([]Executor(nil), s.executors...), nil
}
func (s *memoryStore) LoadJobs(context.Context) ([]Job, error) {
	return append([]Job(nil), s.jobs...), nil
}
func (s *memoryStore) SaveExecutor(_ context.Context, e Executor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executors = append(s.executors, e)
	return nil
}
func (s *memoryStore) SaveJob(_ context.Context, j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, j)
	return nil
}
func (s *memoryStore) DeleteJobs(_ context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, ids...)
	return nil
}

type fakeRuntime struct{}

func (fakeRuntime) Ensure(context.Context, Executor) error { return nil }
func (fakeRuntime) Run(_ context.Context, j Job) ([]byte, error) {
	return []byte("ok:" + string(j.Input)), nil
}
func (fakeRuntime) Stop(context.Context, Executor) error { return nil }

type limitsRuntime struct {
	mu   sync.Mutex
	seen []Executor
}

type storageRuntime struct {
	fakeRuntime
	observation StorageObservation
	stopped     int
}

func (r *storageRuntime) ObserveStorage(context.Context, Executor) (StorageObservation, error) {
	return r.observation, nil
}

func (r *storageRuntime) Stop(context.Context, Executor) error {
	r.stopped++
	return nil
}

func (r *limitsRuntime) Ensure(_ context.Context, executor Executor) error {
	r.mu.Lock()
	r.seen = append(r.seen, executor)
	r.mu.Unlock()
	return nil
}
func (*limitsRuntime) Run(context.Context, Job) ([]byte, error) { return nil, nil }
func (*limitsRuntime) Stop(context.Context, Executor) error     { return nil }

type readyReconcileRuntime struct {
	fakeRuntime
	reconciled int
}

type driftReconcileRuntime struct {
	fakeRuntime
	ensured int
}

func (r *driftReconcileRuntime) Ensure(context.Context, Executor) error {
	r.ensured++
	return nil
}

func (*driftReconcileRuntime) Reconcile(context.Context, Executor) error { return nil }

func (*driftReconcileRuntime) Observe(context.Context, Executor) (RuntimeObservation, error) {
	return RuntimeObservation{Running: true, Status: "running", Health: "healthy", Drifted: true}, nil
}

func (r *readyReconcileRuntime) Reconcile(context.Context, Executor) error {
	r.reconciled++
	return nil
}

func (*readyReconcileRuntime) Observe(context.Context, Executor) (RuntimeObservation, error) {
	return RuntimeObservation{Running: true, Status: "running", Health: "healthy"}, nil
}

type blockingRuntime struct{ started chan struct{} }

func (r blockingRuntime) Ensure(context.Context, Executor) error { return nil }
func (r blockingRuntime) Run(ctx context.Context, _ Job) ([]byte, error) {
	close(r.started)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (r blockingRuntime) Stop(context.Context, Executor) error { return nil }

type noisyRuntime struct{ fakeRuntime }

func (noisyRuntime) RunStream(_ context.Context, _ Job, emit func([]byte)) ([]byte, error) {
	chunk := make([]byte, maxJobLogBytes+1024)
	emit(chunk)
	return chunk, nil
}

type killedOnCancelRuntime struct{ started chan struct{} }

func (r killedOnCancelRuntime) Ensure(context.Context, Executor) error { return nil }
func (r killedOnCancelRuntime) Run(ctx context.Context, _ Job) ([]byte, error) {
	close(r.started)
	<-ctx.Done()
	return nil, errors.New("signal: killed")
}
func (r killedOnCancelRuntime) Stop(context.Context, Executor) error { return nil }

type parallelRuntime struct {
	fakeRuntime
	started chan string
	release chan struct{}
}

type provisionRuntime struct {
	fakeRuntime
	started chan string
	release chan struct{}
}

func (r provisionRuntime) Ensure(ctx context.Context, e Executor) error {
	r.started <- e.UserID
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r parallelRuntime) Run(ctx context.Context, j Job) ([]byte, error) {
	r.started <- j.UserID
	select {
	case <-r.release:
		return []byte("ok"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestSubmitIsBoundedAndRuns(t *testing.T) {
	m, err := New(context.Background(), fakeRuntime{}, &memoryStore{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, err := m.Submit(context.Background(), "u1", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got, ok := m.Job(j.ID)
		if ok && got.Status == "succeeded" {
			return
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatal("job did not complete")
}

func TestCancelRunningJob(t *testing.T) {
	rt := blockingRuntime{started: make(chan struct{})}
	m, err := New(context.Background(), rt, &memoryStore{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, err := m.Submit(context.Background(), "u", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	<-rt.started
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	got, _ := m.Job(j.ID)
	if got.Status != "canceled" {
		t.Fatalf("status=%s", got.Status)
	}
}
func TestSubmitRejectsOversizedInput(t *testing.T) {
	m, err := New(context.Background(), fakeRuntime{}, &memoryStore{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err = m.Submit(context.Background(), "u", make([]byte, 256*1024+1)); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

func TestStreamingOutputIsBounded(t *testing.T) {
	m, err := New(context.Background(), noisyRuntime{}, &memoryStore{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, err := m.Submit(context.Background(), "u", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got, _ := m.Job(j.ID)
		if got.Status == "succeeded" {
			if got.LogBytes != maxJobLogBytes || !got.LogTruncated || len(got.Output) != maxJobLogBytes {
				t.Fatalf("bytes=%d truncated=%v output=%d", got.LogBytes, got.LogTruncated, len(got.Output))
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job did not finish")
}

func TestContextCancellationWinsOverProcessExitError(t *testing.T) {
	rt := killedOnCancelRuntime{started: make(chan struct{})}
	m, err := New(context.Background(), rt, &memoryStore{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.Submit(context.Background(), "u", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	<-rt.started
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	m.Close()
	got, _ := m.Job(j.ID)
	if got.Status != "canceled" {
		t.Fatalf("status=%s err=%s", got.Status, got.Err)
	}
}

func TestWorkerPoolRunsDifferentUsersConcurrentlyAndSerializesOneUser(t *testing.T) {
	rt := parallelRuntime{started: make(chan string, 2), release: make(chan struct{})}
	m, err := NewWithOptions(context.Background(), rt, &memoryStore{}, Options{QueueCapacity: 4, Workers: 2, JobTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err = m.Submit(context.Background(), "u1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Submit(context.Background(), "u1", []byte("y")); !errors.Is(err, ErrUserBusy) {
		t.Fatalf("same user err=%v", err)
	}
	if _, err = m.Submit(context.Background(), "u2", []byte("x")); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case user := <-rt.started:
			seen[user] = true
		case <-time.After(time.Second):
			t.Fatal("workers did not run concurrently")
		}
	}
	if !seen["u1"] || !seen["u2"] {
		t.Fatalf("seen=%v", seen)
	}
	close(rt.release)
}

func TestCanceledQueuedJobNeverRuns(t *testing.T) {
	rt := parallelRuntime{started: make(chan string, 2), release: make(chan struct{})}
	m, err := NewWithOptions(context.Background(), rt, &memoryStore{}, Options{QueueCapacity: 2, Workers: 1, JobTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err = m.Submit(context.Background(), "u1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	<-rt.started
	queued, err := m.Submit(context.Background(), "u2", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Cancel(queued.ID); err != nil {
		t.Fatal(err)
	}
	close(rt.release)
	time.Sleep(20 * time.Millisecond)
	select {
	case user := <-rt.started:
		t.Fatalf("canceled job ran for %s", user)
	default:
	}
	got, _ := m.Job(queued.ID)
	if got.Status != "canceled" {
		t.Fatalf("status=%s", got.Status)
	}
}

func TestProvisioningIsParallelAcrossUsersAndSingleFlightPerUser(t *testing.T) {
	rt := provisionRuntime{started: make(chan string, 4), release: make(chan struct{})}
	m, err := New(context.Background(), rt, &memoryStore{}, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	results := make(chan error, 3)
	go func() { _, e := m.Ensure(context.Background(), "u1"); results <- e }()
	go func() { _, e := m.Ensure(context.Background(), "u1"); results <- e }()
	go func() { _, e := m.Ensure(context.Background(), "u2"); results <- e }()
	seen := map[string]int{}
	for range 2 {
		select {
		case user := <-rt.started:
			seen[user]++
		case <-time.After(time.Second):
			t.Fatal("provisioning serialized")
		}
	}
	if seen["u1"] != 1 || seen["u2"] != 1 {
		t.Fatalf("started=%v", seen)
	}
	select {
	case extra := <-rt.started:
		t.Fatalf("duplicate provisioning for %s", extra)
	default:
	}
	close(rt.release)
	for range 3 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestEnsureWithLimitsPersistsAndReappliesChangedLimits(t *testing.T) {
	runtime := &limitsRuntime{}
	store := &memoryStore{}
	m, err := New(context.Background(), runtime, store, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	firstLimits := ExecutorLimits{CPUs: "1.5", MemoryBytes: 1 << 30, PIDsLimit: 128, DiskBytes: 10 << 30}
	first, err := m.EnsureWithLimits(context.Background(), "quota-user", firstLimits)
	if err != nil || !executorHasLimits(first, firstLimits) {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	secondLimits := ExecutorLimits{CPUs: "2", MemoryBytes: 2 << 30, PIDsLimit: 256, DiskBytes: 20 << 30}
	second, err := m.EnsureWithLimits(context.Background(), "quota-user", secondLimits)
	if err != nil || !executorHasLimits(second, secondLimits) {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	runtime.mu.Lock()
	seen := append([]Executor(nil), runtime.seen...)
	runtime.mu.Unlock()
	if len(seen) != 2 || !executorHasLimits(seen[0], firstLimits) || !executorHasLimits(seen[1], secondLimits) {
		t.Fatalf("runtime limits=%+v", seen)
	}
}

func TestEnsureWithLimitsRejectsInvalidValues(t *testing.T) {
	m, err := New(context.Background(), fakeRuntime{}, &memoryStore{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, limits := range []ExecutorLimits{{CPUs: "NaN"}, {CPUs: "invalid"}, {MemoryBytes: -1}, {PIDsLimit: -1}, {DiskBytes: -1}} {
		if _, err := m.EnsureWithLimits(context.Background(), "quota-user", limits); !errors.Is(err, ErrInvalidExecutorLimits) {
			t.Fatalf("limits=%+v err=%v", limits, err)
		}
	}
}

func TestStorageScanStopsQuotaExceededExecutorAndPersistsUsage(t *testing.T) {
	runtime := &storageRuntime{observation: StorageObservation{UsedBytes: 11, LimitBytes: 10, WorkBytes: 11}}
	store := &memoryStore{executors: []Executor{{UserID: "quota-user", ID: "executor-quota-user", Status: "ready", DiskBytes: 10}}}
	m, err := New(context.Background(), runtime, store, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.scanStorage(context.Background())
	executors := m.Executors()
	if len(executors) != 1 || executors[0].Status != "quota_exceeded" || executors[0].DiskUsedBytes != 11 || executors[0].DiskLimitBytes != 10 || executors[0].DiskCheckedAt == nil {
		t.Fatalf("executor=%+v", executors)
	}
	if runtime.stopped != 1 {
		t.Fatalf("stopped=%d", runtime.stopped)
	}
	stats := m.Stats()
	if stats.DiskQuotaExceeded != 1 || stats.DiskUsedBytes != 11 || stats.DiskQuotaBytes != 10 || stats.ActiveExecutors != 0 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestEnsureReconcilesStorageEvenWhenReadyRuntimeIsHealthy(t *testing.T) {
	runtime := &readyReconcileRuntime{}
	store := &memoryStore{executors: []Executor{{UserID: "ready-user", ID: "executor-ready-user", Status: "ready"}}}
	m, err := New(context.Background(), runtime, store, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.Ensure(context.Background(), "ready-user"); err != nil {
		t.Fatal(err)
	}
	if runtime.reconciled != 1 {
		t.Fatalf("reconciled=%d, want 1", runtime.reconciled)
	}
}

func TestEnsureRepairsRunningRuntimeWhenImageOrIsolationDrifted(t *testing.T) {
	runtime := &driftReconcileRuntime{}
	store := &memoryStore{executors: []Executor{{UserID: "drift-user", ID: "executor-drift-user", Status: "ready"}}}
	m, err := New(context.Background(), runtime, store, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.Ensure(context.Background(), "drift-user"); err != nil {
		t.Fatal(err)
	}
	if runtime.ensured != 1 {
		t.Fatalf("runtime Ensure calls=%d, want 1", runtime.ensured)
	}
}

func TestExecutorCapacityIsAtomicAndStoppedExecutorReleasesSlot(t *testing.T) {
	runtime := &limitsRuntime{}
	m, err := NewWithOptions(context.Background(), runtime, &memoryStore{}, Options{
		QueueCapacity: 1, Workers: 1, ProvisionConcurrency: 1, MaxExecutors: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.Ensure(context.Background(), "user-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), "user-b"); !errors.Is(err, ErrExecutorCapacity) {
		t.Fatalf("second executor err=%v", err)
	}
	if _, err := m.StopExecutor(context.Background(), "user-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), "user-b"); err != nil {
		t.Fatalf("released capacity was not reused: %v", err)
	}
	stats := m.Stats()
	if stats.ActiveExecutors != 1 || stats.ExecutorCapacity != 1 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestProvisioningConcurrencyIsBounded(t *testing.T) {
	rt := provisionRuntime{started: make(chan string, 3), release: make(chan struct{})}
	m, err := NewWithOptions(context.Background(), rt, &memoryStore{}, Options{QueueCapacity: 4, Workers: 1, ProvisionConcurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	done := make(chan error, 3)
	for _, user := range []string{"u1", "u2", "u3"} {
		go func() { _, err := m.Ensure(context.Background(), user); done <- err }()
	}
	for range 2 {
		select {
		case <-rt.started:
		case <-time.After(time.Second):
			t.Fatal("expected two concurrent provisions")
		}
	}
	select {
	case user := <-rt.started:
		t.Fatalf("provision limit exceeded by %s", user)
	case <-time.After(30 * time.Millisecond):
	}
	close(rt.release)
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestSubscriptionSignalsCompletionWithoutPolling(t *testing.T) {
	rt := parallelRuntime{started: make(chan string, 1), release: make(chan struct{})}
	m, err := NewWithOptions(context.Background(), rt, &memoryStore{}, Options{QueueCapacity: 2, Workers: 1, JobTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, err := m.Submit(context.Background(), "user", []byte("run"))
	if err != nil {
		t.Fatal(err)
	}
	<-rt.started
	initial, changes, unsubscribe, ok := m.Subscribe(j.ID)
	if !ok || initial.Status != "running" {
		t.Fatalf("initial=%+v ok=%v", initial, ok)
	}
	defer unsubscribe()
	close(rt.release)
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("missing completion notification")
	}
	completed, _ := m.Job(j.ID)
	if completed.Status != "succeeded" {
		t.Fatalf("status=%s", completed.Status)
	}
}

func TestJobSnapshotsDoNotExposeManagerSlices(t *testing.T) {
	now := time.Now().UTC()
	s := &memoryStore{jobs: []Job{{ID: "job-1", UserID: "u", Status: "succeeded", CreatedAt: &now, FinishedAt: &now, Input: []byte("secret"), Logs: []string{"one"}}}}
	m, err := New(context.Background(), fakeRuntime{}, s, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, _ := m.Job("job-1")
	j.Input[0] = 'X'
	j.Logs[0] = "changed"
	again, _ := m.Job("job-1")
	if string(again.Input) != "secret" || again.Logs[0] != "one" {
		t.Fatalf("internal job mutated: %+v", again)
	}
}

func TestRetainsOnlyConfiguredTerminalJobs(t *testing.T) {
	s := &memoryStore{}
	m, err := NewWithOptions(context.Background(), fakeRuntime{}, s, Options{QueueCapacity: 4, Workers: 1, RetainedJobs: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var ids []string
	for _, user := range []string{"u1", "u2", "u3"} {
		j, err := m.Submit(context.Background(), user, []byte("run"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
		deadline := time.Now().Add(time.Second)
		for {
			got, ok := m.Job(j.ID)
			if ok && got.Status == "succeeded" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("job did not complete")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if _, ok := m.Job(ids[0]); ok {
		t.Fatal("oldest terminal job was not pruned")
	}
	if _, ok := m.Job(ids[1]); !ok {
		t.Fatal("recent terminal job was pruned")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.deleted) != 1 || s.deleted[0] != ids[0] {
		t.Fatalf("deleted=%v", s.deleted)
	}
}

func TestRejectsUnsafeUserIDs(t *testing.T) {
	m, err := New(context.Background(), fakeRuntime{}, &memoryStore{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, id := range []string{"", ".", "..", "../user", "user/name", "user@example.com"} {
		if _, err := m.Ensure(context.Background(), id); !errors.Is(err, ErrInvalidUserID) {
			t.Fatalf("id=%q err=%v", id, err)
		}
	}
}
