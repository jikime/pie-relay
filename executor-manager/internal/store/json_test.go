package store

import (
	"context"
	"testing"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

func TestJSONDeleteJobsPersists(t *testing.T) {
	path := t.TempDir() + "/manager.json"
	s := New(path)
	now := time.Now().UTC()
	for _, id := range []string{"job-1", "job-2"} {
		if err := s.SaveJob(context.Background(), manager.Job{ID: id, Status: "succeeded", CreatedAt: &now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteJobs(context.Background(), []string{"job-1"}); err != nil {
		t.Fatal(err)
	}
	reloaded := New(path)
	jobs, err := reloaded.LoadJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "job-2" {
		t.Fatalf("jobs=%+v", jobs)
	}
}
