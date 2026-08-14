package control

import (
	"context"
	"path"
	"sort"
	"strings"
	"unicode"
)

const defaultMaxProjectsPerUser = 32

func ProjectWorkingDir(projectID string) string {
	return "/workspace/projects/" + projectID
}

// NormalizePreviewAppPath accepts only a project-relative POSIX path. The
// Manager, not an external caller, combines it with the opaque project root.
// Backslashes are rejected so the same value cannot be interpreted
// differently by the HTTP layer and the Linux Executor.
func NormalizePreviewAppPath(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." {
		return ".", true
	}
	if len(value) > 512 || strings.Contains(value, "\\") || path.IsAbs(value) {
		return "", false
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." || len([]byte(part)) > 255 {
			return "", false
		}
		for _, character := range part {
			if unicode.IsControl(character) {
				return "", false
			}
		}
	}
	return clean, true
}

func ProjectPreviewWorkingDir(projectID, appPath string) (string, bool) {
	if !validID(projectID) {
		return "", false
	}
	normalized, ok := NormalizePreviewAppPath(appPath)
	if !ok {
		return "", false
	}
	if normalized == "." {
		return ProjectWorkingDir(projectID), true
	}
	return ProjectWorkingDir(projectID) + "/" + normalized, true
}

func (s *Service) Projects() []Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]Project, 0, len(s.projects))
	for _, value := range s.projects {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	return values
}

func (s *Service) Project(id string) (Project, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.projects[id]
	return value, ok
}

func (s *Service) PutProject(ctx context.Context, value Project, expected int64, meta MutationMeta) (Project, error) {
	value.Name = strings.TrimSpace(value.Name)
	value.Locale = strings.TrimSpace(value.Locale)
	value.PreviewAppPath = strings.TrimSpace(value.PreviewAppPath)
	if !validID(value.ID) || !validID(value.IntegrationID) || !validID(value.IntegrationUserID) || !validID(value.OwnerUserID) || !validProjectName(value.Name) || !validProjectLocale(value.Locale) || !validProjectStatus(value.Status) {
		return Project{}, ErrInvalid
	}
	if value.WorkingDir == "" {
		value.WorkingDir = ProjectWorkingDir(value.ID)
	}
	if value.WorkingDir != ProjectWorkingDir(value.ID) || len(value.LastError) > 2000 {
		return Project{}, ErrInvalid
	}
	if value.PreviewAppPath != "" {
		normalized, ok := NormalizePreviewAppPath(value.PreviewAppPath)
		if !ok {
			return Project{}, ErrInvalid
		}
		value.PreviewAppPath = normalized
	}
	binding, bindingOK := s.IntegrationUser(value.IntegrationUserID)
	integration, integrationOK := s.Integration(value.IntegrationID)
	if !bindingOK || binding.IntegrationID != value.IntegrationID || binding.OwnerUserID != value.OwnerUserID || !integrationOK || integration.Status != "active" {
		return Project{}, ErrForbidden
	}
	quotaUnlock := s.lock("project-quota", value.IntegrationUserID)
	defer quotaUnlock()
	unlocks := s.lock(KindProject, value.ID)
	defer unlocks()
	s.mu.RLock()
	before, exists := s.projects[value.ID]
	s.mu.RUnlock()
	if exists && before.Version != expected || !exists && expected != 0 {
		return Project{}, ErrConflict
	}
	if !exists {
		active := 0
		s.mu.RLock()
		for _, candidate := range s.projects {
			if candidate.IntegrationUserID == value.IntegrationUserID && candidate.Status != "archived" {
				active++
			}
		}
		s.mu.RUnlock()
		if active >= integration.MaxProjectsPerUser {
			return Project{}, ErrQuota
		}
	}
	if exists && (before.IntegrationID != value.IntegrationID || before.IntegrationUserID != value.IntegrationUserID || before.OwnerUserID != value.OwnerUserID || before.WorkingDir != value.WorkingDir || before.Name != value.Name || before.Locale != value.Locale) {
		return Project{}, ErrForbidden
	}
	now := s.now().UTC()
	if exists {
		value.CreatedAt = before.CreatedAt
	} else {
		value.CreatedAt = now
	}
	value.UpdatedAt, value.Version = now, expected+1
	if err := s.persist(ctx, KindProject, value.ID, expected, value); err != nil {
		return Project{}, err
	}
	s.mu.Lock()
	s.projects[value.ID] = value
	s.emitLocked(KindProject, mutationAction(exists), value.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "project."+mutationAction(exists), KindProject, value.ID, before, value, true, "")
	return value, nil
}

func validProjectName(value string) bool {
	if value == "" || len([]rune(value)) > 120 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validProjectLocale(value string) bool {
	switch value {
	case "ko", "en", "ja", "zh":
		return true
	}
	return false
}

func validProjectStatus(value string) bool {
	switch value {
	case "provisioning", "ready", "failed", "archived":
		return true
	}
	return false
}
