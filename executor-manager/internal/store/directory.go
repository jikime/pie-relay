package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

// Directory persists one JSON document per executor/job. Unlike the legacy
// JSON store, a job update does not rewrite every other record.
type Directory struct {
	root string
}

func NewDirectory(root string) *Directory { return &Directory{root: root} }

func (s *Directory) Load(ctx context.Context) ([]manager.Executor, error) {
	return loadRecords[manager.Executor](ctx, filepath.Join(s.root, "executors"))
}

func (s *Directory) LoadJobs(ctx context.Context) ([]manager.Job, error) {
	return loadRecords[manager.Job](ctx, filepath.Join(s.root, "jobs"))
}

func (s *Directory) SaveExecutor(ctx context.Context, executor manager.Executor) error {
	if !manager.ValidUserID(executor.UserID) {
		return manager.ErrInvalidUserID
	}
	return writeRecord(ctx, filepath.Join(s.root, "executors"), executor.UserID+".json", executor)
}

func (s *Directory) SaveJob(ctx context.Context, job manager.Job) error {
	if !safeRecordID(job.ID) {
		return errors.New("invalid job id")
	}
	return writeRecord(ctx, filepath.Join(s.root, "jobs"), job.ID+".json", job)
}

func (s *Directory) DeleteJobs(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !safeRecordID(id) {
			return errors.New("invalid job id")
		}
		err := os.Remove(filepath.Join(s.root, "jobs", id+".json"))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return syncDirectory(filepath.Join(s.root, "jobs"))
}

func loadRecords[T any](ctx context.Context, dir string) ([]T, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var value T
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, fmt.Errorf("decode %s: %w", entry.Name(), err)
		}
		out = append(out, value)
	}
	return out, nil
}

func writeRecord(ctx context.Context, dir, name string, value any) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func safeRecordID(id string) bool {
	if len(id) < 1 || len(id) > 160 || id == "." || id == ".." {
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
