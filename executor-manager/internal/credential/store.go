package credential

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Store struct {
	Root string
	UID  int
	GID  int
}

type Result struct {
	Digest string
	Bytes  int64
}

func New(root, containerUser string) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("credential state root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	uid, gid, err := numericIdentity(containerUser)
	if err != nil {
		return nil, err
	}
	return &Store{Root: absolute, UID: uid, GID: gid}, nil
}

func (s *Store) Write(userID, targetPath string, payload []byte, maxBytes int64) (Result, error) {
	if !safeID(userID) {
		return Result{}, errors.New("invalid credential owner")
	}
	if !safeRelativePath(targetPath) {
		return Result{}, errors.New("invalid credential target path")
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	if int64(len(payload)) > maxBytes {
		return Result{}, errors.New("credential exceeds integration limit")
	}
	if !json.Valid(payload) {
		return Result{}, errors.New("credential must be valid JSON")
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return Result{}, errors.New("credential must be valid JSON")
	}
	if _, ok := value.(map[string]any); !ok {
		return Result{}, errors.New("credential must be a JSON object")
	}
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return Result{}, err
	}
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	if err := ensureDir(root, userID, s.UID, s.GID); err != nil {
		return Result{}, err
	}
	userRoot, err := root.OpenRoot(userID)
	if err != nil {
		return Result{}, err
	}
	defer userRoot.Close()
	parent := filepath.Dir(targetPath)
	if parent != "." {
		current := ""
		for _, part := range strings.Split(filepath.ToSlash(parent), "/") {
			if current == "" {
				current = part
			} else {
				current += "/" + part
			}
			if err := ensureDir(userRoot, current, s.UID, s.GID); err != nil {
				return Result{}, err
			}
		}
	}
	if info, statErr := userRoot.Lstat(targetPath); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return Result{}, errors.New("credential target cannot be a symlink")
	} else if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return Result{}, statErr
	}
	tempName, err := randomTempName(parent)
	if err != nil {
		return Result{}, err
	}
	file, err := userRoot.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Result{}, err
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = userRoot.Remove(tempName)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return Result{}, err
	}
	if s.UID >= 0 && s.GID >= 0 {
		if err := file.Chown(s.UID, s.GID); err != nil {
			return Result{}, err
		}
	}
	if _, err := file.Write(payload); err != nil {
		return Result{}, err
	}
	if err := file.Sync(); err != nil {
		return Result{}, err
	}
	if err := file.Close(); err != nil {
		return Result{}, err
	}
	if err := userRoot.Rename(tempName, targetPath); err != nil {
		return Result{}, err
	}
	cleanup = false
	if parentRoot, err := userRoot.Open(parent); err == nil {
		_ = parentRoot.Sync()
		_ = parentRoot.Close()
	}
	digest := sha256.Sum256(payload)
	return Result{Digest: hex.EncodeToString(digest[:]), Bytes: int64(len(payload))}, nil
}

func (s *Store) Remove(userID, targetPath string) error {
	if !safeID(userID) || !safeRelativePath(targetPath) {
		return errors.New("invalid credential path")
	}
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer root.Close()
	userRoot, err := root.OpenRoot(userID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer userRoot.Close()
	err = userRoot.Remove(targetPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func ensureDir(root *os.Root, path string, uid, gid int) error {
	info, err := root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := root.Mkdir(path, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err = root.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("credential directory %s is not a real directory", path)
	}
	if err := root.Chmod(path, 0700); err != nil {
		return err
	}
	if uid >= 0 && gid >= 0 {
		if err := root.Chown(path, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

func randomTempName(parent string) (string, error) {
	var raw [12]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", err
	}
	name := ".pie-credential-" + hex.EncodeToString(raw[:])
	if parent == "." {
		return name, nil
	}
	return filepath.ToSlash(filepath.Join(parent, name)), nil
}

func numericIdentity(value string) (int, int, error) {
	if value == "" {
		return os.Geteuid(), os.Getegid(), nil
	}
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, 0, errors.New("credential provisioning requires numeric PIE_EXECUTOR_CONTAINER_USER=uid:gid")
	}
	uid, err1 := strconv.Atoi(parts[0])
	gid, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || uid < 0 || gid < 0 {
		return 0, 0, errors.New("invalid executor uid:gid")
	}
	return uid, gid, nil
}

func safeID(value string) bool {
	if value == "" || len(value) > 160 || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}

func safeRelativePath(value string) bool {
	if value == "" || len(value) > 240 || filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	return clean == filepath.ToSlash(value) && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}
