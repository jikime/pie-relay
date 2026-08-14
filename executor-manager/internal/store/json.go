package store

import (
	"context"
	"encoding/json"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
	"os"
	"path/filepath"
	"sync"
)

type JSON struct {
	mu        sync.Mutex
	path      string
	executors map[string]manager.Executor
	jobs      map[string]manager.Job
}
type disk struct {
	Executors []manager.Executor `json:"executors"`
	Jobs      []manager.Job      `json:"jobs"`
}

func New(path string) *JSON {
	return &JSON{path: path, executors: map[string]manager.Executor{}, jobs: map[string]manager.Job{}}
}
func (s *JSON) Load(_ context.Context) ([]manager.Executor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, e := os.ReadFile(s.path)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var d disk
	if e = json.Unmarshal(b, &d); e != nil {
		return nil, e
	}
	for _, x := range d.Executors {
		s.executors[x.UserID] = x
	}
	for _, x := range d.Jobs {
		s.jobs[x.ID] = x
	}
	return d.Executors, nil
}
func (s *JSON) LoadJobs(_ context.Context) ([]manager.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, e := os.ReadFile(s.path)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var d disk
	if e = json.Unmarshal(b, &d); e != nil {
		return nil, e
	}
	return d.Jobs, nil
}
func (s *JSON) flush() error {
	d := disk{}
	for _, x := range s.executors {
		d.Executors = append(d.Executors, x)
	}
	for _, x := range s.jobs {
		d.Jobs = append(d.Jobs, x)
	}
	b, e := json.MarshalIndent(d, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(s.path), 0700); e != nil {
		return e
	}
	tmp := s.path + ".tmp"
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	return os.Rename(tmp, s.path)
}
func (s *JSON) SaveExecutor(_ context.Context, e manager.Executor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executors[e.UserID] = e
	return s.flush()
}
func (s *JSON) SaveJob(_ context.Context, j manager.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j
	return s.flush()
}

func (s *JSON) DeleteJobs(_ context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.jobs, id)
	}
	return s.flush()
}
