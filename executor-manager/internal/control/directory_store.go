package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type DirectoryStore struct {
	root string
	mu   sync.Mutex
}

func NewDirectoryStore(root string) *DirectoryStore { return &DirectoryStore{root: root} }

func (s *DirectoryStore) Load(ctx context.Context) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, kindEntry := range entries {
		if !kindEntry.IsDir() || !validRecordPart(kindEntry.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.root, kindEntry.Name()))
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(s.root, kindEntry.Name(), file.Name()))
			if err != nil {
				return nil, err
			}
			var record Record
			if err := json.Unmarshal(data, &record); err != nil {
				return nil, fmt.Errorf("decode control record %s/%s: %w", kindEntry.Name(), file.Name(), err)
			}
			if record.Kind != kindEntry.Name() || record.ID == "" || record.Version < 1 {
				return nil, fmt.Errorf("invalid control record %s/%s", kindEntry.Name(), file.Name())
			}
			out = append(out, record)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind == out[j].Kind {
			return out[i].ID < out[j].ID
		}
		return out[i].Kind < out[j].Kind
	})
	return out, nil
}

func (s *DirectoryStore) Put(ctx context.Context, record Record, expectedVersion int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validRecordPart(record.Kind) || !validRecordPart(record.ID) || record.Version != expectedVersion+1 || len(record.Data) == 0 {
		return errors.New("invalid control record")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.root, record.Kind)
	path := filepath.Join(dir, record.ID+".json")
	current, err := readRecordVersion(path)
	if err != nil {
		return err
	}
	if current != expectedVersion {
		return ErrConflict
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".control-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return syncDir(dir)
}

func (s *DirectoryStore) Delete(ctx context.Context, kind, id string, expectedVersion int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validRecordPart(kind) || !validRecordPart(id) || expectedVersion < 1 {
		return errors.New("invalid control record")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.root, kind)
	path := filepath.Join(dir, id+".json")
	current, err := readRecordVersion(path)
	if err != nil {
		return err
	}
	if current != expectedVersion {
		return ErrConflict
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDir(dir)
}

func (s *DirectoryStore) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.MkdirAll(s.root, 0700)
}

func (s *DirectoryStore) Close() error { return nil }

func readRecordVersion(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var current Record
	if err := json.Unmarshal(data, &current); err != nil {
		return 0, err
	}
	return current.Version, nil
}

func validRecordPart(value string) bool {
	if value == "" || len(value) > 180 || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':' {
			continue
		}
		return false
	}
	return true
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
