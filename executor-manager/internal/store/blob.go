package store

import (
	"crypto/rand"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
)

var safeBlobUser = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
var ErrBlobQuotaExceeded = errors.New("user blob quota exceeded")

type BlobStore struct {
	root       string
	maxFile    int64
	maxUser    int64
	locks      [64]sync.Mutex
	usageMu    sync.Mutex
	usage      map[string]int64
	usageKnown map[string]bool
}

func NewBlobStore(root string, maxFile, maxUser int64) *BlobStore {
	return &BlobStore{root: root, maxFile: maxFile, maxUser: maxUser, usage: map[string]int64{}, usageKnown: map[string]bool{}}
}

func (s *BlobStore) Save(user, name string, src io.Reader) (string, int64, error) {
	if s == nil || s.maxFile < 1 || s.maxUser < 1 {
		return "", 0, errors.New("blob store is not configured")
	}
	lock := s.userLock(user)
	lock.Lock()
	defer lock.Unlock()
	used, err := s.userUsage(user)
	if err != nil {
		return "", 0, err
	}
	remaining := s.maxUser - used
	if remaining <= 0 {
		return "", 0, ErrBlobQuotaExceeded
	}
	limit := min(s.maxFile, remaining)
	ref, n, err := SaveBlob(s.root, user, name, src, limit)
	if err != nil {
		if limit < s.maxFile && strings.Contains(err.Error(), "upload too large") {
			return "", n, ErrBlobQuotaExceeded
		}
		return "", n, err
	}
	s.usageMu.Lock()
	s.usage[user] = used + n
	s.usageKnown[user] = true
	s.usageMu.Unlock()
	return ref, n, nil
}

func (s *BlobStore) ValidateRefs(user string, refs []string) error {
	for _, ref := range refs {
		path, err := s.pathForRef(user, ref)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("blob not found: %s", ref)
		}
	}
	return nil
}

func (s *BlobStore) Delete(user, ref string) error {
	path, err := s.pathForRef(user, ref)
	if err != nil {
		return err
	}
	lock := s.userLock(user)
	lock.Lock()
	defer lock.Unlock()
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("invalid blob")
	}
	used, err := s.userUsage(user)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	s.usageMu.Lock()
	s.usage[user] = max(0, used-info.Size())
	s.usageKnown[user] = true
	s.usageMu.Unlock()
	return nil
}

func (s *BlobStore) pathForRef(user, ref string) (string, error) {
	if s == nil || !safeBlobUser.MatchString(user) || user == "." || user == ".." || !strings.HasPrefix(ref, user+"/") {
		return "", errors.New("invalid blob reference")
	}
	name := strings.TrimPrefix(ref, user+"/")
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "/\\") {
		return "", errors.New("invalid blob reference")
	}
	return filepath.Join(s.root, user, name), nil
}

func (s *BlobStore) userUsage(user string) (int64, error) {
	s.usageMu.Lock()
	if s.usageKnown[user] {
		used := s.usage[user]
		s.usageMu.Unlock()
		return used, nil
	}
	s.usageMu.Unlock()
	dir := filepath.Join(s.root, user)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			return 0, fmt.Errorf("unexpected non-file in blob directory: %s", entry.Name())
		}
		total += info.Size()
	}
	s.usageMu.Lock()
	s.usage[user] = total
	s.usageKnown[user] = true
	s.usageMu.Unlock()
	return total, nil
}

func (s *BlobStore) userLock(user string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(user))
	return &s.locks[h.Sum32()%uint32(len(s.locks))]
}

func SaveBlob(root, user, name string, src io.Reader, max int64) (string, int64, error) {
	if !safeBlobUser.MatchString(user) || user == "." || user == ".." || name == "" || filepath.Base(name) != name {
		return "", 0, fmt.Errorf("invalid upload")
	}
	dir := filepath.Join(root, user)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", 0, err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", 0, err
	}
	ref := fmt.Sprintf("%x-%s", b[:], name)
	path := filepath.Join(dir, ref)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", 0, err
	}
	// Executor bind directories are owned by their fixed non-root uid. The
	// Manager commonly runs as root for Docker socket access, so a newly created
	// 0600 upload would otherwise be unreadable inside the user's container.
	// Inherit the already-provisioned directory owner without loosening mode bits.
	if dirInfo, statErr := os.Stat(dir); statErr == nil {
		if stat, ok := dirInfo.Sys().(*syscall.Stat_t); ok {
			fileInfo, fileStatErr := f.Stat()
			fileStat, fileStatOK := fileInfo.Sys().(*syscall.Stat_t)
			needsOwner := fileStatErr == nil && fileStatOK && (fileStat.Uid != stat.Uid || fileStat.Gid != stat.Gid)
			if needsOwner {
				if chownErr := f.Chown(int(stat.Uid), int(stat.Gid)); chownErr != nil {
					_ = f.Close()
					_ = os.Remove(path)
					return "", 0, chownErr
				}
			}
		}
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(path)
		}
	}()
	n, e := io.Copy(f, io.LimitReader(src, max+1))
	ce := f.Close()
	if e != nil {
		return "", n, e
	}
	if ce != nil {
		return "", n, ce
	}
	if n > max {
		return "", n, fmt.Errorf("upload too large")
	}
	complete = true
	return filepath.ToSlash(filepath.Join(user, ref)), n, nil
}
