package runtime

import (
	"crypto/sha256"
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
)

const (
	krootCommonMarkerName   = ".pie-kroot-common.json"
	krootCommonMarkerSchema = 1
)

type krootCommonMarker struct {
	SchemaVersion int      `json:"schemaVersion"`
	BundleVersion string   `json:"bundleVersion"`
	Digest        string   `json:"digest"`
	ManagedRoots  []string `json:"managedRoots"`
	FileCount     int      `json:"fileCount"`
	AppliedAt     string   `json:"appliedAt"`
}

type krootCommonBundle struct {
	root         string
	digest       string
	managedRoots []string
	fileCount    int
}

var krootCommonStateLocks sync.Map
var krootCommonBundleCache sync.Map

// syncKrootCommonBundle installs only Kroot-owned common skill directories and
// the complete common agents tree into a persistent Executor HOME. Unrelated
// user skills, credentials, settings, history and project data remain untouched.
//
// A content digest makes reconciliation cheap. On change, every managed root is
// staged on the same filesystem and exchanged with rollback backups before the
// marker is committed. Existing Executors therefore receive upgrades without
// requiring their HOME volume to be recreated.
func syncKrootCommonBundle(stateRoot, bundleRoot, configuredVersion string) error {
	if strings.TrimSpace(bundleRoot) == "" {
		return nil
	}
	if err := ensureRealDirectory(stateRoot); err != nil {
		return err
	}
	lockValue, _ := krootCommonStateLocks.LoadOrStore(filepath.Clean(stateRoot), &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	bundle, err := inspectKrootCommonBundle(bundleRoot, configuredVersion)
	if err != nil {
		return err
	}
	version := strings.TrimSpace(configuredVersion)
	if version == "" {
		version = bundle.digest
	}
	markerPath := filepath.Join(stateRoot, krootCommonMarkerName)
	previous, exists, err := readKrootCommonMarker(markerPath)
	if err != nil {
		return err
	}
	next := krootCommonMarker{
		SchemaVersion: krootCommonMarkerSchema,
		BundleVersion: version,
		Digest:        bundle.digest,
		ManagedRoots:  append([]string(nil), bundle.managedRoots...),
		FileCount:     bundle.fileCount,
		AppliedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	installedRootsAvailable := false
	if exists && previous.Digest == next.Digest && stringSlicesEqual(previous.ManagedRoots, next.ManagedRoots) {
		installedRootsAvailable, err = krootManagedRootsAvailable(stateRoot, next.ManagedRoots)
		if err != nil {
			return err
		}
	}
	if installedRootsAvailable {
		if previous.BundleVersion == next.BundleVersion && previous.FileCount == next.FileCount {
			return nil
		}
		return writeKrootCommonMarker(markerPath, next)
	}

	if err := ensureKrootCommonParents(stateRoot); err != nil {
		return err
	}
	stageRoot, err := os.MkdirTemp(stateRoot, ".pie-kroot-common-stage-*")
	if err != nil {
		return fmt.Errorf("create Kroot common staging directory: %w", err)
	}
	defer os.RemoveAll(stageRoot)
	if err := os.Chmod(stageRoot, 0700); err != nil {
		return err
	}
	payloadRoot := filepath.Join(stageRoot, "payload")
	if err := os.Mkdir(payloadRoot, 0700); err != nil {
		return err
	}
	for _, relative := range bundle.managedRoots {
		if err := copyKrootCommonTree(filepath.Join(bundle.root, filepath.FromSlash(relative)), filepath.Join(payloadRoot, filepath.FromSlash(relative))); err != nil {
			return fmt.Errorf("stage Kroot common root %s: %w", relative, err)
		}
	}

	impacted := append([]string(nil), bundle.managedRoots...)
	if exists {
		impacted = append(impacted, previous.ManagedRoots...)
	}
	impacted = uniqueSortedStrings(impacted)
	desired := make(map[string]bool, len(bundle.managedRoots))
	for _, relative := range bundle.managedRoots {
		desired[relative] = true
	}
	changes := make([]krootCommonChange, 0, len(impacted))
	for index, relative := range impacted {
		if !validKrootManagedRoot(relative) {
			rollbackKrootCommonChanges(stateRoot, changes)
			return fmt.Errorf("invalid managed Kroot root %q", relative)
		}
		target := filepath.Join(stateRoot, filepath.FromSlash(relative))
		change := krootCommonChange{relative: relative}
		if info, statErr := os.Lstat(target); statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				rollbackKrootCommonChanges(stateRoot, changes)
				return fmt.Errorf("managed Kroot target is not a real directory: %s", relative)
			}
			backup := filepath.Join(stageRoot, "backup", fmt.Sprintf("%04d", index))
			if err := os.MkdirAll(filepath.Dir(backup), 0700); err != nil {
				rollbackKrootCommonChanges(stateRoot, changes)
				return err
			}
			if err := os.Rename(target, backup); err != nil {
				rollbackKrootCommonChanges(stateRoot, changes)
				return fmt.Errorf("back up managed Kroot root %s: %w", relative, err)
			}
			change.backup = backup
		}
		if desired[relative] {
			staged := filepath.Join(payloadRoot, filepath.FromSlash(relative))
			if err := os.Rename(staged, target); err != nil {
				if change.backup != "" {
					_ = os.Rename(change.backup, target)
				}
				rollbackKrootCommonChanges(stateRoot, changes)
				return fmt.Errorf("activate managed Kroot root %s: %w", relative, err)
			}
			change.installed = true
		}
		changes = append(changes, change)
	}
	for _, path := range []string{
		filepath.Join(stateRoot, ".claude", "skills"),
		filepath.Join(stateRoot, ".claude", "agents"),
	} {
		if err := syncDirectory(path); err != nil {
			rollbackKrootCommonChanges(stateRoot, changes)
			return err
		}
	}
	if err := writeKrootCommonMarker(markerPath, next); err != nil {
		rollbackKrootCommonChanges(stateRoot, changes)
		return err
	}
	return nil
}

type krootCommonChange struct {
	relative  string
	backup    string
	installed bool
}

func rollbackKrootCommonChanges(stateRoot string, changes []krootCommonChange) {
	for index := len(changes) - 1; index >= 0; index-- {
		change := changes[index]
		if !validKrootManagedRoot(change.relative) {
			continue
		}
		target := filepath.Join(stateRoot, filepath.FromSlash(change.relative))
		if change.installed {
			_ = os.RemoveAll(target)
		}
		if change.backup != "" {
			_ = os.Rename(change.backup, target)
		}
	}
}

func inspectKrootCommonBundle(root, configuredVersion string) (krootCommonBundle, error) {
	resolved, err := filepath.EvalSymlinks(strings.TrimSpace(root))
	if err != nil {
		return krootCommonBundle{}, fmt.Errorf("resolve Kroot common bundle: %w", err)
	}
	// Releases produced by prepare-kroot-common-bundle.sh are immutable and the
	// `current` symlink changes to a new resolved path on upgrade. Cache the
	// expensive 500+ file validation by resolved release plus operator version,
	// rather than repeating it for every user reconciliation.
	cacheKey := resolved + "\x00" + strings.TrimSpace(configuredVersion)
	if cached, ok := krootCommonBundleCache.Load(cacheKey); ok {
		return cached.(krootCommonBundle), nil
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return krootCommonBundle{}, fmt.Errorf("inspect Kroot common bundle: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return krootCommonBundle{}, errors.New("Kroot common bundle root must resolve to a real directory")
	}
	skillsRoot := filepath.Join(resolved, ".claude", "skills")
	agentsRoot := filepath.Join(resolved, ".claude", "agents")
	if err := requireRealDirectory(skillsRoot, "Kroot common skills"); err != nil {
		return krootCommonBundle{}, err
	}
	if err := requireRealDirectory(agentsRoot, "Kroot common agents"); err != nil {
		return krootCommonBundle{}, err
	}
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		return krootCommonBundle{}, err
	}
	managedRoots := []string{".claude/agents"}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return krootCommonBundle{}, fmt.Errorf("Kroot common skills may contain only real skill directories: %s", entry.Name())
		}
		managedRoots = append(managedRoots, ".claude/skills/"+entry.Name())
	}
	if len(managedRoots) == 1 {
		return krootCommonBundle{}, errors.New("Kroot common bundle contains no skill directories")
	}
	sort.Strings(managedRoots)
	hash := sha256.New()
	fileCount := 0
	for _, relative := range managedRoots {
		if !validKrootManagedRoot(relative) {
			return krootCommonBundle{}, fmt.Errorf("invalid Kroot common source path %q", relative)
		}
		rootPath := filepath.Join(resolved, filepath.FromSlash(relative))
		before := fileCount
		if _, err := hashKrootCommonTree(hash, resolved, rootPath, &fileCount); err != nil {
			return krootCommonBundle{}, err
		}
		if relative == ".claude/agents" && fileCount == before {
			return krootCommonBundle{}, errors.New("Kroot common bundle contains no agent files")
		}
	}
	if fileCount == 0 {
		return krootCommonBundle{}, errors.New("Kroot common bundle contains no files")
	}
	bundle := krootCommonBundle{
		root:         resolved,
		digest:       "sha256:" + hex.EncodeToString(hash.Sum(nil)),
		managedRoots: managedRoots,
		fileCount:    fileCount,
	}
	actual, _ := krootCommonBundleCache.LoadOrStore(cacheKey, bundle)
	return actual.(krootCommonBundle), nil
}

func hashKrootCommonTree(hash io.Writer, bundleRoot, treeRoot string, fileCount *int) (int, error) {
	directories := 0
	err := filepath.WalkDir(treeRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(bundleRoot, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("Kroot common bundle contains symlink: %s", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories++
			_, err = fmt.Fprintf(hash, "D\x00%s\x00", relative)
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("Kroot common bundle contains unsupported file: %s", relative)
		}
		if _, err := fmt.Fprintf(hash, "F\x00%s\x00%03o\x00", relative, normalizedKrootCommonMode(info.Mode())); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		(*fileCount)++
		return nil
	})
	return directories, err
}

func copyKrootCommonTree(sourceRoot, targetRoot string) error {
	if err := os.MkdirAll(filepath.Dir(targetRoot), 0700); err != nil {
		return err
	}
	return filepath.WalkDir(sourceRoot, func(source string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(sourceRoot, source)
		if err != nil {
			return err
		}
		target := targetRoot
		if relative != "." {
			target = filepath.Join(targetRoot, relative)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("Kroot common source contains symlink: %s", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Mkdir(target, 0700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("Kroot common source contains unsupported file: %s", relative)
		}
		return copyKrootCommonFile(source, target, normalizedKrootCommonMode(info.Mode()))
	})
}

func copyKrootCommonFile(source, target string, mode os.FileMode) (result error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); result == nil {
			result = closeErr
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func normalizedKrootCommonMode(mode os.FileMode) os.FileMode {
	if mode.Perm()&0111 != 0 {
		return 0555
	}
	return 0444
}

func ensureKrootCommonParents(stateRoot string) error {
	for _, relative := range []string{".claude", ".claude/skills", ".claude/agents"} {
		if err := ensureRealDirectory(filepath.Join(stateRoot, filepath.FromSlash(relative))); err != nil {
			return err
		}
	}
	return nil
}

func requireRealDirectory(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s must be a real directory", label)
	}
	return nil
}

func validKrootManagedRoot(relative string) bool {
	relative = filepath.ToSlash(filepath.Clean(relative))
	// agents/kroot remains accepted for a safe one-time migration from the
	// previous marker schema. New bundles always own the complete agents tree.
	if relative == ".claude/agents" || relative == ".claude/agents/kroot" {
		return true
	}
	const prefix = ".claude/skills/"
	if !strings.HasPrefix(relative, prefix) {
		return false
	}
	name := strings.TrimPrefix(relative, prefix)
	return name != "" && name != "." && name != ".." && !strings.Contains(name, "/")
}

func krootManagedRootsAvailable(stateRoot string, roots []string) (bool, error) {
	for _, relative := range roots {
		if !validKrootManagedRoot(relative) {
			return false, fmt.Errorf("invalid managed Kroot root %q", relative)
		}
		info, err := os.Lstat(filepath.Join(stateRoot, filepath.FromSlash(relative)))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, fmt.Errorf("managed Kroot target is not a real directory: %s", relative)
		}
	}
	return true, nil
}

func readKrootCommonMarker(path string) (krootCommonMarker, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return krootCommonMarker{}, false, nil
	}
	if err != nil {
		return krootCommonMarker{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return krootCommonMarker{}, false, errors.New("Kroot common marker is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return krootCommonMarker{}, false, err
	}
	var marker krootCommonMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return krootCommonMarker{}, false, fmt.Errorf("decode Kroot common marker: %w", err)
	}
	if marker.SchemaVersion != krootCommonMarkerSchema || marker.Digest == "" {
		return krootCommonMarker{}, false, errors.New("unsupported Kroot common marker")
	}
	for _, relative := range marker.ManagedRoots {
		if !validKrootManagedRoot(relative) {
			return krootCommonMarker{}, false, fmt.Errorf("invalid managed root in Kroot common marker: %q", relative)
		}
	}
	marker.ManagedRoots = uniqueSortedStrings(marker.ManagedRoots)
	return marker, true, nil
}

func writeKrootCommonMarker(path string, marker krootCommonMarker) (result error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("Kroot common marker is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pie-kroot-common-marker-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}()
	if err := temporary.Chmod(0600); err != nil {
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(marker); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
