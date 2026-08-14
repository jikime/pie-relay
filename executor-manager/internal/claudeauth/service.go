package claudeauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/credential"
)

const (
	setupTokenCandidate    = "setup-token"
	encryptedTokenFile     = "oauth-token.enc"
	masterKeyFile          = "master.key"
	authMode               = "subscription-oauth"
	maxTokenBytes          = 32 << 10
	legacyCredentialTarget = ".claude/.credentials.json"
	legacyVersionTokenFile = ".credentials.json"
)

var (
	ErrNotConfigured    = errors.New("Claude authentication is not configured")
	ErrExpired          = errors.New("Claude subscription authentication has expired")
	ErrMigrationPending = errors.New("legacy Claude authentication requires subscription OAuth migration")
)

type Service struct {
	Root     string
	LoginDir string
	Targets  *credential.Store
	Required bool
	Now      func() time.Time
	key      []byte
	mu       sync.Mutex
}

type Version struct {
	ID          string    `json:"id"`
	Label       string    `json:"label,omitempty"`
	Fingerprint string    `json:"fingerprint"`
	Bytes       int64     `json:"bytes"`
	CreatedAt   time.Time `json:"createdAt"`
	Mode        string    `json:"mode"`
	RotateAfter time.Time `json:"rotateAfter"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type State struct {
	ActiveVersion   string    `json:"activeVersion,omitempty"`
	PreviousVersion string    `json:"previousVersion,omitempty"`
	UpdatedAt       time.Time `json:"updatedAt,omitempty"`
}

type Deployment struct {
	UserID    string    `json:"userId"`
	Version   string    `json:"version,omitempty"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	LastError string    `json:"lastError,omitempty"`
}

type Status struct {
	Configured       bool         `json:"configured"`
	Available        bool         `json:"available"`
	Required         bool         `json:"required"`
	Expired          bool         `json:"expired"`
	RotationDue      bool         `json:"rotationDue"`
	MigrationPending bool         `json:"migrationPending"`
	State            State        `json:"state"`
	Active           *Version     `json:"active,omitempty"`
	Versions         []Version    `json:"versions"`
	Targets          TargetCounts `json:"targets"`
	Deployments      []Deployment `json:"deployments"`
}

type TargetCounts struct {
	Total    int `json:"total"`
	Current  int `json:"current"`
	Verified int `json:"verified"`
	Outdated int `json:"outdated"`
	Missing  int `json:"missing"`
	Failed   int `json:"failed"`
}

type Rollout struct {
	Version   string       `json:"version"`
	Total     int          `json:"total"`
	Succeeded int          `json:"succeeded"`
	Failed    int          `json:"failed"`
	Items     []Deployment `json:"items"`
}

type encryptedToken struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

func Open(root, loginDir string, targets *credential.Store, required bool) (*Service, error) {
	if targets == nil {
		return nil, errors.New("Claude authentication target store is required")
	}
	var err error
	if root, err = cleanAbsolute(root); err != nil {
		return nil, fmt.Errorf("Claude authentication root: %w", err)
	}
	if loginDir == "" {
		loginDir = filepath.Join(root, "login")
	}
	if loginDir, err = cleanAbsolute(loginDir); err != nil {
		return nil, fmt.Errorf("Claude login directory: %w", err)
	}
	for _, path := range []string{root, loginDir, filepath.Join(root, "versions")} {
		if err := ensurePrivateDirectory(path); err != nil {
			return nil, err
		}
	}
	key, err := loadOrCreateMasterKey(filepath.Join(root, masterKeyFile))
	if err != nil {
		return nil, fmt.Errorf("Claude authentication master key: %w", err)
	}
	return &Service{Root: root, LoginDir: loginDir, Targets: targets, Required: required, key: key}, nil
}

func (s *Service) PublishFromLogin(_ context.Context, label string) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := filepath.Join(s.LoginDir, setupTokenCandidate)
	payload, err := readSetupToken(candidate)
	if err != nil {
		return Version{}, fmt.Errorf("read Event Manager Claude setup token: %w", err)
	}
	version, err := s.publishLocked(payload, label)
	zero(payload)
	if err == nil {
		// The candidate is deliberately one-shot. The durable copy is encrypted
		// under the Manager-owned master key below versions/.
		_ = os.Remove(candidate)
	}
	return version, err
}

func (s *Service) BootstrapFrom(path, label string) (Version, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readStateLocked()
	if err != nil {
		return Version{}, false, err
	}
	if state.ActiveVersion != "" {
		version, err := s.readVersionLocked(state.ActiveVersion)
		if err == nil {
			return version, false, nil
		}
		legacy, legacyErr := s.legacyVersionLocked(state.ActiveVersion)
		if legacyErr != nil {
			return Version{}, false, err
		}
		if !legacy {
			return Version{}, false, err
		}
	}
	if strings.TrimSpace(path) == "" {
		return Version{}, false, nil
	}
	payload, err := readSetupToken(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Version{}, false, nil
	}
	if err != nil {
		return Version{}, false, err
	}
	version, err := s.publishLocked(payload, label)
	zero(payload)
	return version, err == nil, err
}

func (s *Service) Rollback(_ context.Context) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readStateLocked()
	if err != nil {
		return Version{}, err
	}
	if state.PreviousVersion == "" {
		return Version{}, errors.New("previous Claude authentication version is unavailable")
	}
	version, err := s.readVersionLocked(state.PreviousVersion)
	if err != nil {
		return Version{}, err
	}
	state.ActiveVersion, state.PreviousVersion = state.PreviousVersion, state.ActiveVersion
	state.UpdatedAt = s.now()
	if err := writeJSONAtomic(filepath.Join(s.Root, "active.json"), state, 0600); err != nil {
		return Version{}, err
	}
	return version, nil
}

func (s *Service) EnsureUser(ctx context.Context, userID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	_, _, payload, err := s.activeLocked()
	s.mu.Unlock()
	zero(payload)
	if err != nil {
		if errors.Is(err, ErrNotConfigured) && !s.Required {
			return nil
		}
		return err
	}
	// Subscription OAuth is never copied into a user's persistent HOME. The
	// controller obtains it only when starting a chat session and sends it over
	// the container-local stdin control channel.
	return s.Targets.Remove(userID, legacyCredentialTarget)
}

func (s *Service) Rollout(ctx context.Context, userIDs []string, concurrency int) (Rollout, error) {
	s.mu.Lock()
	_, version, payload, err := s.activeLocked()
	s.mu.Unlock()
	zero(payload)
	if err != nil {
		return Rollout{}, err
	}
	userIDs = uniqueIDs(userIDs)
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 32 {
		concurrency = 32
	}
	result := Rollout{Version: version.ID, Total: len(userIDs), Items: make([]Deployment, len(userIDs))}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for index, userID := range userIDs {
		wg.Add(1)
		go func(index int, userID string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				result.Items[index] = Deployment{UserID: userID, Version: version.ID, Status: "failed", LastError: "rollout canceled"}
				return
			}
			item := Deployment{UserID: userID, Version: version.ID, Status: "available", UpdatedAt: s.now()}
			if removeErr := s.Targets.Remove(userID, legacyCredentialTarget); removeErr != nil {
				item.Status, item.LastError = "failed", boundedError(removeErr)
			}
			result.Items[index] = item
		}(index, userID)
	}
	wg.Wait()
	for _, item := range result.Items {
		if item.Status == "failed" {
			result.Failed++
		} else {
			result.Succeeded++
		}
	}
	if result.Failed > 0 {
		return result, fmt.Errorf("Claude authentication rollout failed for %d of %d targets", result.Failed, result.Total)
	}
	return result, nil
}

func (s *Service) RecordVerification(userID, version string, verifyErr error) error {
	// Kept for API compatibility while rolling upgrades are in flight. There
	// is no per-user credential to verify in subscription OAuth mode; a real
	// canary turn is the meaningful verification boundary.
	return verifyErr
}

func (s *Service) Status(userIDs []string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readStateLocked()
	if err != nil {
		return Status{}, err
	}
	versions, err := s.versionsLocked()
	if err != nil {
		return Status{}, err
	}
	result := Status{Required: s.Required, State: state, Versions: versions, Deployments: []Deployment{}}
	if state.ActiveVersion != "" {
		active, err := s.readVersionLocked(state.ActiveVersion)
		if err != nil {
			legacy, legacyErr := s.legacyVersionLocked(state.ActiveVersion)
			if legacyErr != nil || !legacy {
				return Status{}, err
			}
			result.MigrationPending = true
		} else {
			result.Configured = true
			result.Active = &active
			result.Expired = !s.now().Before(active.ExpiresAt)
			result.RotationDue = !s.now().Before(active.RotateAfter)
			result.Available = !result.Expired
		}
	}
	for _, userID := range uniqueIDs(userIDs) {
		item := Deployment{UserID: userID, Status: "missing"}
		if result.Active != nil && !result.Expired {
			item.Version = result.Active.ID
			item.Status = "available"
			item.UpdatedAt = state.UpdatedAt
		} else if result.Active != nil {
			item.Version = result.Active.ID
			item.Status = "failed"
			item.UpdatedAt = state.UpdatedAt
			item.LastError = "subscription OAuth expired"
		}
		result.Deployments = append(result.Deployments, item)
		result.Targets.Total++
		switch item.Status {
		case "available":
			result.Targets.Current++
		case "outdated":
			result.Targets.Outdated++
		case "failed":
			result.Targets.Failed++
		default:
			result.Targets.Missing++
		}
	}
	return result, nil
}

func (s *Service) LoginDirectory() string { return s.LoginDir }

// CurrentOAuthToken returns the active subscription credential to the trusted
// in-process session controller. It is intentionally absent from every HTTP
// response type and is never written into a user Executor state directory.
func (s *Service) CurrentOAuthToken(ctx context.Context) (token, version string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	_, active, payload, err := s.activeLocked()
	s.mu.Unlock()
	if err != nil {
		if errors.Is(err, ErrNotConfigured) && !s.Required {
			return "", "", nil
		}
		return "", "", err
	}
	token = string(payload)
	zero(payload)
	return token, active.ID, nil
}

func (s *Service) publishLocked(payload []byte, label string) (Version, error) {
	digest := sha256.Sum256(payload)
	fingerprint := hex.EncodeToString(digest[:])
	state, err := s.readStateLocked()
	if err != nil {
		return Version{}, err
	}
	previousVersion := ""
	if state.ActiveVersion != "" {
		active, activeErr := s.readVersionLocked(state.ActiveVersion)
		if activeErr == nil && active.Fingerprint == fingerprint {
			return active, nil
		}
		if activeErr == nil {
			previousVersion = state.ActiveVersion
		}
	}
	now := s.now()
	id := "v-" + now.Format("20060102T150405.000000000Z") + "-" + fingerprint[:12]
	version := Version{
		ID: id, Label: strings.TrimSpace(label), Fingerprint: fingerprint,
		Bytes: int64(len(payload)), CreatedAt: now, Mode: authMode,
		RotateAfter: now.Add(330 * 24 * time.Hour), ExpiresAt: now.Add(365 * 24 * time.Hour),
	}
	directory := filepath.Join(s.Root, "versions", id)
	if err := os.Mkdir(directory, 0700); err != nil {
		return Version{}, err
	}
	sealed, err := s.encrypt(payload)
	if err != nil {
		_ = os.RemoveAll(directory)
		return Version{}, err
	}
	if err := writeJSONAtomic(filepath.Join(directory, encryptedTokenFile), sealed, 0600); err != nil {
		_ = os.RemoveAll(directory)
		return Version{}, err
	}
	if err := writeJSONAtomic(filepath.Join(directory, "metadata.json"), version, 0600); err != nil {
		_ = os.RemoveAll(directory)
		return Version{}, err
	}
	// Only a valid subscription-OAuth version may become a rollback target.
	// Legacy per-container credential versions use a different on-disk format
	// and must never be reactivated by the central broker.
	state.PreviousVersion, state.ActiveVersion, state.UpdatedAt = previousVersion, id, now
	if err := writeJSONAtomic(filepath.Join(s.Root, "active.json"), state, 0600); err != nil {
		return Version{}, err
	}
	return version, nil
}

func (s *Service) activeLocked() (State, Version, []byte, error) {
	state, err := s.readStateLocked()
	if err != nil {
		return State{}, Version{}, nil, err
	}
	if state.ActiveVersion == "" {
		return state, Version{}, nil, ErrNotConfigured
	}
	version, err := s.readVersionLocked(state.ActiveVersion)
	if err != nil {
		if legacy, legacyErr := s.legacyVersionLocked(state.ActiveVersion); legacyErr == nil && legacy {
			return state, Version{}, nil, ErrMigrationPending
		}
		return state, Version{}, nil, err
	}
	if !s.now().Before(version.ExpiresAt) {
		return state, version, nil, ErrExpired
	}
	var sealed encryptedToken
	if err := readJSON(filepath.Join(s.Root, "versions", version.ID, encryptedTokenFile), &sealed, maxTokenBytes*2); err != nil {
		return state, version, nil, err
	}
	payload, err := s.decrypt(sealed)
	if err != nil {
		return state, version, nil, err
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != version.Fingerprint {
		zero(payload)
		return state, version, nil, errors.New("Claude OAuth token integrity check failed")
	}
	return state, version, payload, nil
}

func (s *Service) readStateLocked() (State, error) {
	var state State
	err := readJSON(filepath.Join(s.Root, "active.json"), &state, 32<<10)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	return state, err
}

func (s *Service) readVersionLocked(id string) (Version, error) {
	if !safeVersionID(id) {
		return Version{}, errors.New("invalid Claude authentication version")
	}
	var version Version
	if err := readJSON(filepath.Join(s.Root, "versions", id, "metadata.json"), &version, 32<<10); err != nil {
		return Version{}, err
	}
	if version.ID != id || len(version.Fingerprint) != 64 || version.Mode != authMode {
		return Version{}, errors.New("invalid Claude authentication metadata")
	}
	return version, nil
}

func (s *Service) legacyVersionLocked(id string) (bool, error) {
	if !safeVersionID(id) {
		return false, errors.New("invalid Claude authentication version")
	}
	var version Version
	if err := readJSON(filepath.Join(s.Root, "versions", id, "metadata.json"), &version, 32<<10); err != nil {
		return false, err
	}
	if version.ID != id || len(version.Fingerprint) != 64 || version.Mode != "" {
		return false, nil
	}
	info, err := os.Lstat(filepath.Join(s.Root, "versions", id, legacyVersionTokenFile))
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0, nil
}

func (s *Service) versionsLocked() ([]Version, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "versions"))
	if err != nil {
		return nil, err
	}
	versions := make([]Version, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !safeVersionID(entry.Name()) {
			continue
		}
		version, err := s.readVersionLocked(entry.Name())
		if err != nil {
			// Keep legacy per-container credential versions on disk for rollback
			// forensics, but never expose or activate them as subscription OAuth.
			continue
		}
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].CreatedAt.After(versions[j].CreatedAt) })
	if len(versions) > 20 {
		versions = versions[:20]
	}
	return versions, nil
}

func readSetupToken(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("Claude setup token must be a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Claude setup token permissions must be 0600 or stricter")
	}
	if info.Size() <= 0 || info.Size() > maxTokenBytes {
		return nil, errors.New("Claude setup token size is invalid")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	payload = []byte(strings.TrimSpace(string(payload)))
	if len(payload) < 20 {
		return nil, errors.New("Claude setup token is too short")
	}
	for _, value := range payload {
		if value <= 0x20 || value == 0x7f {
			return nil, errors.New("Claude setup token must not contain whitespace or control characters")
		}
	}
	return payload, nil
}

func (s *Service) encrypt(plaintext []byte) (encryptedToken, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return encryptedToken{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return encryptedToken{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return encryptedToken{}, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, []byte(authMode))
	return encryptedToken{
		Version:    1,
		Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
	}, nil
}

func (s *Service) decrypt(value encryptedToken) ([]byte, error) {
	if value.Version != 1 {
		return nil, errors.New("unsupported Claude OAuth token envelope")
	}
	nonce, err := base64.RawStdEncoding.DecodeString(value.Nonce)
	if err != nil {
		return nil, errors.New("invalid Claude OAuth token nonce")
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(value.Ciphertext)
	if err != nil {
		return nil, errors.New("invalid Claude OAuth token ciphertext")
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, errors.New("invalid Claude OAuth token nonce length")
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(authMode))
	if err != nil {
		return nil, errors.New("decrypt Claude OAuth token")
	}
	if len(plaintext) < 20 || len(plaintext) > maxTokenBytes {
		zero(plaintext)
		return nil, errors.New("decrypted Claude OAuth token size is invalid")
	}
	return plaintext, nil
}

func loadOrCreateMasterKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, err
		}
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if createErr == nil {
			writeErr := file.Chmod(0600)
			if writeErr == nil {
				_, writeErr = file.Write(key)
			}
			if closeErr := file.Close(); writeErr == nil {
				writeErr = closeErr
			}
			if writeErr != nil {
				_ = os.Remove(path)
				zero(key)
				return nil, writeErr
			}
			return key, nil
		}
		zero(key)
		if !errors.Is(createErr, fs.ErrExist) {
			return nil, createErr
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != 32 {
		return nil, errors.New("master key must be a 32-byte regular file with mode 0600 or stricter")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		zero(key)
		return nil, errors.New("invalid master key length")
	}
	return key, nil
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func readJSON(path string, target any, maximum int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maximum))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSONAtomic(path string, value any, mode fs.FileMode) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeBytesAtomic(path, append(payload, '\n'), mode)
}

func writeBytesAtomic(path string, payload []byte, mode fs.FileMode) error {
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pie-claude-auth-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() { _ = temporary.Close(); _ = os.Remove(name) }()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("path is not a private directory: %s", path)
	}
	return os.Chmod(path, 0700)
}

func cleanAbsolute(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is required")
	}
	return filepath.Abs(filepath.Clean(path))
}

func uniqueIDs(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func safeVersionID(value string) bool {
	if !strings.HasPrefix(value, "v-") || len(value) > 96 {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' {
			continue
		}
		return false
	}
	return true
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 240 {
		message = message[:240]
	}
	return message
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
