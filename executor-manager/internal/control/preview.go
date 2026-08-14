package control

import (
	"context"
	"sort"
	"strings"
)

func (s *Service) Previews() []Preview {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]Preview, 0, len(s.previews))
	for _, value := range s.previews {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	return values
}

func (s *Service) Preview(id string) (Preview, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.previews[id]
	return value, ok
}

func (s *Service) PreviewByHostname(hostname string) (Preview, bool) {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := s.previewHosts[hostname]
	if id == "" {
		return Preview{}, false
	}
	value, ok := s.previews[id]
	return value, ok
}

func (s *Service) PreviewsForIntegrationUser(integrationUserID string) []Preview {
	s.mu.RLock()
	values := s.indexedPreviewsLocked(s.previewUsers[integrationUserID])
	s.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	return values
}

func (s *Service) PreviewsForProject(projectID string, limit int) []Preview {
	s.mu.RLock()
	values := s.indexedPreviewsLocked(s.previewProjects[projectID])
	s.mu.RUnlock()
	// Keep every active preview visible even when a project has a long stopped
	// history. Within each group the newest record comes first.
	sort.Slice(values, func(i, j int) bool {
		leftActive, rightActive := activePreview(values[i]), activePreview(values[j])
		if leftActive != rightActive {
			return leftActive
		}
		return values[i].CreatedAt.After(values[j].CreatedAt)
	})
	if limit > 0 && len(values) > limit {
		values = values[:limit]
	}
	return values
}

func activePreview(value Preview) bool {
	return value.Status != "stopped" && value.Status != "failed"
}

func (s *Service) indexedPreviewsLocked(ids map[string]struct{}) []Preview {
	values := make([]Preview, 0, len(ids))
	for id := range ids {
		if value, ok := s.previews[id]; ok {
			values = append(values, value)
		}
	}
	return values
}

func (s *Service) PutPreview(ctx context.Context, value Preview, expected int64, meta MutationMeta) (Preview, error) {
	var appPathOK bool
	value.AppPath, appPathOK = NormalizePreviewAppPath(value.AppPath)
	value.Hostname = strings.ToLower(strings.TrimSpace(value.Hostname))
	value.BackendHost = strings.ToLower(strings.TrimSpace(value.BackendHost))
	value.Profile = strings.TrimSpace(value.Profile)
	value.Visibility = strings.TrimSpace(value.Visibility)
	value.Status = strings.TrimSpace(value.Status)
	if !appPathOK || !validID(value.ID) || !validID(value.IntegrationID) || !validID(value.IntegrationUserID) || !validID(value.OwnerUserID) || !validID(value.ProjectID) || !validPreviewHostname(value.Hostname) || !validPreviewBackend(value.BackendHost) || value.Port < 1024 || value.Port > 65535 || !validPreviewProfile(value.Profile) || !validPreviewVisibility(value.Visibility) || value.AccessVersion < 0 || !validPreviewStatus(value.Status) || len(value.LastError) > 2000 || value.StartAttempts < 0 || value.StartAttempts > 32 {
		return Preview{}, ErrInvalid
	}
	binding, bindingOK := s.IntegrationUser(value.IntegrationUserID)
	integration, integrationOK := s.Integration(value.IntegrationID)
	project, projectOK := s.Project(value.ProjectID)
	if !bindingOK || !integrationOK || integration.Status != "active" || !projectOK || project.Status != "ready" || binding.IntegrationID != value.IntegrationID || binding.OwnerUserID != value.OwnerUserID || project.IntegrationID != value.IntegrationID || project.IntegrationUserID != value.IntegrationUserID || project.OwnerUserID != value.OwnerUserID {
		return Preview{}, ErrForbidden
	}
	quotaUnlock := s.lock("preview-quota", value.IntegrationUserID)
	defer quotaUnlock()
	objectUnlock := s.lock(KindPreview, value.ID)
	defer objectUnlock()
	s.mu.RLock()
	before, exists := s.previews[value.ID]
	s.mu.RUnlock()
	if exists && before.Version != expected || !exists && expected != 0 {
		return Preview{}, ErrConflict
	}
	beforeAppPath, _ := NormalizePreviewAppPath(before.AppPath)
	if exists && (before.IntegrationID != value.IntegrationID || before.IntegrationUserID != value.IntegrationUserID || before.OwnerUserID != value.OwnerUserID || before.ProjectID != value.ProjectID || beforeAppPath != value.AppPath || before.Hostname != value.Hostname || before.Port != value.Port || before.Profile != value.Profile) {
		return Preview{}, ErrForbidden
	}
	// Visibility is the only mutable routing property. Require a monotonic
	// access generation change with it, and reject arbitrary generation edits.
	// Legacy records start at generation zero and remain valid until changed.
	if exists {
		visibilityChanged := before.Visibility != value.Visibility
		if visibilityChanged && value.AccessVersion != before.AccessVersion+1 || !visibilityChanged && value.AccessVersion != before.AccessVersion {
			return Preview{}, ErrForbidden
		}
	}
	active := 0
	s.mu.RLock()
	if existingID := s.previewHosts[value.Hostname]; existingID != "" && existingID != value.ID {
		s.mu.RUnlock()
		return Preview{}, ErrConflict
	}
	for candidateID := range s.previewUsers[value.IntegrationUserID] {
		candidate := s.previews[candidateID]
		if candidate.ID == value.ID {
			continue
		}
		if candidate.Status != "stopped" && candidate.Status != "failed" {
			active++
			if candidate.Port == value.Port {
				s.mu.RUnlock()
				return Preview{}, ErrConflict
			}
		}
	}
	s.mu.RUnlock()
	if !exists && active >= integration.MaxPreviewsPerUser {
		return Preview{}, ErrQuota
	}
	now := s.now().UTC()
	if exists {
		value.CreatedAt = before.CreatedAt
	} else {
		value.CreatedAt = now
	}
	value.UpdatedAt, value.Version = now, expected+1
	if err := s.persist(ctx, KindPreview, value.ID, expected, value); err != nil {
		return Preview{}, err
	}
	s.mu.Lock()
	s.previews[value.ID] = value
	s.indexPreviewLocked(value)
	s.emitLocked(KindPreview, mutationAction(exists), value.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "preview."+mutationAction(exists), KindPreview, value.ID, before, value, true, "")
	return value, nil
}

// DeletePreview removes only the operational record and its hot indexes. The
// caller must stop the runtime first; the deletion audit remains as a separate
// immutable audit record.
func (s *Service) DeletePreview(ctx context.Context, id string, expected int64, meta MutationMeta) error {
	if !validID(id) || expected < 1 {
		return ErrInvalid
	}
	unlock := s.lock(KindPreview, id)
	defer unlock()
	s.mu.RLock()
	before, ok := s.previews[id]
	s.mu.RUnlock()
	if !ok {
		return ErrNotFound
	}
	if before.Version != expected {
		return ErrConflict
	}
	if err := s.store.Delete(ctx, KindPreview, id, expected); err != nil {
		return err
	}
	s.mu.Lock()
	s.unindexPreviewLocked(before)
	delete(s.previews, id)
	s.emitLocked(KindPreview, "deleted", id)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "preview.deleted", KindPreview, id, before, nil, true, "")
	return nil
}

func (s *Service) indexPreviewLocked(value Preview) {
	if s.previewHosts == nil {
		s.previewHosts = map[string]string{}
	}
	if s.previewUsers == nil {
		s.previewUsers = map[string]map[string]struct{}{}
	}
	if s.previewProjects == nil {
		s.previewProjects = map[string]map[string]struct{}{}
	}
	s.previewHosts[value.Hostname] = value.ID
	if s.previewUsers[value.IntegrationUserID] == nil {
		s.previewUsers[value.IntegrationUserID] = map[string]struct{}{}
	}
	s.previewUsers[value.IntegrationUserID][value.ID] = struct{}{}
	if s.previewProjects[value.ProjectID] == nil {
		s.previewProjects[value.ProjectID] = map[string]struct{}{}
	}
	s.previewProjects[value.ProjectID][value.ID] = struct{}{}
}

func (s *Service) unindexPreviewLocked(value Preview) {
	if s.previewHosts[value.Hostname] == value.ID {
		delete(s.previewHosts, value.Hostname)
	}
	removePreviewIndexValue(s.previewUsers, value.IntegrationUserID, value.ID)
	removePreviewIndexValue(s.previewProjects, value.ProjectID, value.ID)
}

func removePreviewIndexValue(index map[string]map[string]struct{}, key, id string) {
	values := index[key]
	delete(values, id)
	if len(values) == 0 {
		delete(index, key)
	}
}

func validPreviewHostname(value string) bool {
	if len(value) < 4 || len(value) > 253 || strings.Contains(value, "..") {
		return false
	}
	labels := strings.Split(value, ".")
	if len(labels) < 3 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func validPreviewBackend(value string) bool {
	if value == "" || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validPreviewProfile(value string) bool {
	switch value {
	case "auto", "next", "vite", "npm":
		return true
	}
	return false
}

func validPreviewVisibility(value string) bool {
	return value == "private" || value == "public"
}

func validPreviewStatus(value string) bool {
	switch value {
	case "starting", "ready", "stopping", "stopped", "failed":
		return true
	}
	return false
}
