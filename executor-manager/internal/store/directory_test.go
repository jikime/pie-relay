package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

func TestDirectoryStorePersistsIndependentRecords(t *testing.T) {
	root := t.TempDir()
	s := NewDirectory(root)
	now := time.Now().UTC()
	executor := manager.Executor{UserID: "user-1", ID: "executor-user-1", Status: "ready", CreatedAt: now}
	if err := s.SaveExecutor(context.Background(), executor); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"job-1", "job-2"} {
		if err := s.SaveJob(context.Background(), manager.Job{ID: id, UserID: "user-1", Status: "succeeded", CreatedAt: &now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteJobs(context.Background(), []string{"job-1"}); err != nil {
		t.Fatal(err)
	}
	executors, err := NewDirectory(root).Load(context.Background())
	if err != nil || len(executors) != 1 || executors[0].UserID != "user-1" {
		t.Fatalf("executors=%+v err=%v", executors, err)
	}
	jobs, err := NewDirectory(root).LoadJobs(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].ID != "job-2" {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	info, err := os.Stat(filepath.Join(root, "jobs", "job-2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v", info.Mode().Perm())
	}
}

func TestDirectoryStoreRejectsUnsafeIDsAndCorruptRecords(t *testing.T) {
	root := t.TempDir()
	s := NewDirectory(root)
	if err := s.SaveJob(context.Background(), manager.Job{ID: "../job"}); err == nil {
		t.Fatal("unsafe job id accepted")
	}
	dir := filepath.Join(root, "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadJobs(context.Background()); err == nil {
		t.Fatal("corrupt record accepted")
	}
}
