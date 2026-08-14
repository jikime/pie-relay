package control

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultCredentialMaxBytes int64 = 64 << 10

func (s *Service) Integrations() []Integration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]Integration, 0, len(s.integrations))
	for _, value := range s.integrations {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	return values
}

func (s *Service) Integration(id string) (Integration, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.integrations[id]
	return value, ok
}

func (s *Service) PutIntegration(ctx context.Context, value Integration, expected int64, meta MutationMeta) (Integration, error) {
	normalizeIntegration(&value)
	value.ID = strings.TrimSpace(value.ID)
	value.DisplayName = strings.TrimSpace(value.DisplayName)
	if !validID(value.ID) || value.DisplayName == "" || len(value.DisplayName) > 120 {
		return Integration{}, ErrInvalid
	}
	if value.Status == "" {
		value.Status = "active"
	}
	if value.Status != "active" && value.Status != "disabled" && value.Status != "revoked" {
		return Integration{}, ErrInvalid
	}
	if value.Credential.Format != "json" || !validCredentialPath(value.Credential.TargetPath) {
		return Integration{}, ErrInvalid
	}
	if value.Credential.MaxBytes < 2 || value.Credential.MaxBytes > 1<<20 {
		return Integration{}, ErrInvalid
	}
	if value.MaxUsers < 1 || value.MaxUsers > 1_000_000 || value.MaxProjectsPerUser < 1 || value.MaxProjectsPerUser > 256 || value.MaxPreviewsPerUser < 1 || value.MaxPreviewsPerUser > 32 || value.MaxConversationsPerUser < 1 || value.MaxConversationsPerUser > 128 {
		return Integration{}, ErrInvalid
	}
	unlock := s.lock(KindIntegration, value.ID)
	defer unlock()
	s.mu.RLock()
	before, exists := s.integrations[value.ID]
	s.mu.RUnlock()
	if exists && before.Version != expected || !exists && expected != 0 {
		return Integration{}, ErrConflict
	}
	now := s.now().UTC()
	if exists {
		value.CreatedAt = before.CreatedAt
	} else {
		value.CreatedAt = now
	}
	value.UpdatedAt, value.Version = now, expected+1
	if err := s.persist(ctx, KindIntegration, value.ID, expected, value); err != nil {
		return Integration{}, err
	}
	s.mu.Lock()
	s.integrations[value.ID] = value
	s.emitLocked(KindIntegration, mutationAction(exists), value.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "integration."+mutationAction(exists), KindIntegration, value.ID, before, value, true, "")
	return value, nil
}

func normalizeIntegration(value *Integration) {
	if value.Credential.Format == "" {
		value.Credential.Format = "json"
	}
	if value.Credential.MaxBytes == 0 {
		value.Credential.MaxBytes = defaultCredentialMaxBytes
	}
	if value.MaxUsers == 0 {
		value.MaxUsers = 10_000
	}
	if value.MaxProjectsPerUser == 0 {
		value.MaxProjectsPerUser = defaultMaxProjectsPerUser
	}
	if value.MaxPreviewsPerUser == 0 {
		value.MaxPreviewsPerUser = 4
	}
	if value.MaxConversationsPerUser == 0 {
		value.MaxConversationsPerUser = 4
	}
}

func (s *Service) IntegrationSecret(id string) (IntegrationSecret, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.integrationSecrets[id]
	return value, ok
}

func (s *Service) PutIntegrationSecret(ctx context.Context, value IntegrationSecret, expected int64) (IntegrationSecret, error) {
	if !validID(value.ID) || len(value.TokenHash) != 64 {
		return IntegrationSecret{}, ErrInvalid
	}
	if _, ok := s.Integration(value.ID); !ok {
		return IntegrationSecret{}, ErrNotFound
	}
	unlock := s.lock(KindIntegrationSecret, value.ID)
	defer unlock()
	s.mu.RLock()
	before, exists := s.integrationSecrets[value.ID]
	s.mu.RUnlock()
	if exists && before.Version != expected || !exists && expected != 0 {
		return IntegrationSecret{}, ErrConflict
	}
	now := s.now().UTC()
	if exists {
		value.CreatedAt = before.CreatedAt
	} else {
		value.CreatedAt = now
	}
	value.UpdatedAt, value.Version = now, expected+1
	if err := s.persist(ctx, KindIntegrationSecret, value.ID, expected, value); err != nil {
		return IntegrationSecret{}, err
	}
	s.mu.Lock()
	s.integrationSecrets[value.ID] = value
	s.emitLocked(KindIntegrationSecret, mutationAction(exists), value.ID)
	s.mu.Unlock()
	return value, nil
}

func (s *Service) IntegrationUsers() []IntegrationUser {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]IntegrationUser, 0, len(s.integrationUsers))
	for _, value := range s.integrationUsers {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	return values
}

func (s *Service) IntegrationUser(id string) (IntegrationUser, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.integrationUsers[id]
	return value, ok
}

func (s *Service) PutIntegrationUser(ctx context.Context, value IntegrationUser, expected int64, meta MutationMeta) (IntegrationUser, error) {
	if !validID(value.ID) || !validID(value.IntegrationID) || !validID(value.OwnerUserID) || strings.TrimSpace(value.ExternalUserID) == "" || len(value.ExternalUserID) > 512 {
		return IntegrationUser{}, ErrInvalid
	}
	if value.Status == "" {
		value.Status = "provisioning"
	}
	if !validIntegrationUserStatus(value.Status) {
		return IntegrationUser{}, ErrInvalid
	}
	integration, integrationOK := s.Integration(value.IntegrationID)
	_, ownerOK := s.User(value.OwnerUserID)
	if !integrationOK || integration.Status != "active" || !ownerOK {
		return IntegrationUser{}, ErrForbidden
	}
	quotaUnlock := s.lock("integration-user-quota", value.IntegrationID)
	defer quotaUnlock()
	unlock := s.lock(KindIntegrationUser, value.ID)
	defer unlock()
	s.mu.RLock()
	before, exists := s.integrationUsers[value.ID]
	s.mu.RUnlock()
	if exists && before.Version != expected || !exists && expected != 0 {
		return IntegrationUser{}, ErrConflict
	}
	if !exists {
		active := 0
		s.mu.RLock()
		for _, candidate := range s.integrationUsers {
			if candidate.IntegrationID == value.IntegrationID && candidate.Status != "suspended" && candidate.Status != "deleting" {
				active++
			}
		}
		s.mu.RUnlock()
		if active >= integration.MaxUsers {
			return IntegrationUser{}, ErrQuota
		}
	}
	if exists && (before.IntegrationID != value.IntegrationID || before.ExternalUserID != value.ExternalUserID || before.OwnerUserID != value.OwnerUserID) {
		return IntegrationUser{}, ErrForbidden
	}
	now := s.now().UTC()
	if exists {
		value.CreatedAt = before.CreatedAt
	} else {
		value.CreatedAt = now
	}
	value.UpdatedAt, value.Version = now, expected+1
	if err := s.persist(ctx, KindIntegrationUser, value.ID, expected, value); err != nil {
		return IntegrationUser{}, err
	}
	s.mu.Lock()
	s.integrationUsers[value.ID] = value
	s.emitLocked(KindIntegrationUser, mutationAction(exists), value.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "integration-user."+mutationAction(exists), KindIntegrationUser, value.ID, before, value, true, "")
	return value, nil
}

func (s *Service) DeleteIntegrationUser(ctx context.Context, id string, expected int64, meta MutationMeta) error {
	unlock := s.lock(KindIntegrationUser, id)
	defer unlock()
	s.mu.RLock()
	before, ok := s.integrationUsers[id]
	s.mu.RUnlock()
	if !ok {
		return ErrNotFound
	}
	if before.Version != expected {
		return ErrConflict
	}
	for _, conversation := range s.Conversations() {
		if conversation.IntegrationUserID == id && conversation.Status != "closed" && conversation.Status != "deleted" {
			return errors.New("integration user has active conversations")
		}
	}
	if err := s.store.Delete(ctx, KindIntegrationUser, id, expected); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.integrationUsers, id)
	s.emitLocked(KindIntegrationUser, "deleted", id)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "integration-user.deleted", KindIntegrationUser, id, before, nil, true, "")
	return nil
}

func (s *Service) Conversations() []Conversation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]Conversation, 0, len(s.conversations))
	for _, value := range s.conversations {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	return values
}

func (s *Service) Conversation(id string) (Conversation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.conversations[id]
	return value, ok
}

func (s *Service) PutConversation(ctx context.Context, value Conversation, expected int64, meta MutationMeta) (Conversation, error) {
	if !validID(value.ID) || !validID(value.IntegrationID) || !validID(value.IntegrationUserID) || !validID(value.OwnerUserID) || !validID(value.DeviceID) || !validID(value.SessionID) || !validConversationStatus(value.Status) {
		return Conversation{}, ErrInvalid
	}
	binding, bindingOK := s.IntegrationUser(value.IntegrationUserID)
	device, deviceOK := s.Device(value.DeviceID)
	session, sessionOK := s.Session(value.SessionID)
	project, projectOK := s.Project(value.ProjectID)
	if !bindingOK || binding.IntegrationID != value.IntegrationID || binding.OwnerUserID != value.OwnerUserID || !projectOK || project.Status != "ready" || project.IntegrationID != value.IntegrationID || project.IntegrationUserID != value.IntegrationUserID || project.OwnerUserID != value.OwnerUserID || !deviceOK || device.Kind != "docker" || device.OwnerUserID != value.OwnerUserID || !sessionOK || session.ExecutionTarget != "docker" || session.OwnerUserID != value.OwnerUserID || session.DeviceID != value.DeviceID || session.ProjectID != value.ProjectID {
		return Conversation{}, ErrForbidden
	}
	integration, integrationOK := s.Integration(value.IntegrationID)
	if !integrationOK || integration.Status != "active" {
		return Conversation{}, ErrForbidden
	}
	quotaUnlock := s.lock("conversation-quota", value.IntegrationUserID)
	defer quotaUnlock()
	unlock := s.lock(KindConversation, value.ID)
	defer unlock()
	s.mu.RLock()
	before, exists := s.conversations[value.ID]
	s.mu.RUnlock()
	if exists && before.Version != expected || !exists && expected != 0 {
		return Conversation{}, ErrConflict
	}
	if !exists {
		active := 0
		s.mu.RLock()
		for _, candidate := range s.conversations {
			if candidate.IntegrationUserID == value.IntegrationUserID && candidate.Status != "closed" && candidate.Status != "deleted" {
				active++
			}
		}
		s.mu.RUnlock()
		if active >= integration.MaxConversationsPerUser {
			return Conversation{}, ErrQuota
		}
	}
	if exists && (before.IntegrationID != value.IntegrationID || before.IntegrationUserID != value.IntegrationUserID || before.OwnerUserID != value.OwnerUserID || before.DeviceID != value.DeviceID || before.ProjectID != value.ProjectID || before.SessionID != value.SessionID) {
		return Conversation{}, ErrForbidden
	}
	now := s.now().UTC()
	if exists {
		value.CreatedAt = before.CreatedAt
		if value.LastActivityAt.IsZero() {
			value.LastActivityAt = before.LastActivityAt
		}
	} else {
		value.CreatedAt = now
		if value.LastActivityAt.IsZero() {
			value.LastActivityAt = now
		}
	}
	value.UpdatedAt, value.Version = now, expected+1
	if err := s.persist(ctx, KindConversation, value.ID, expected, value); err != nil {
		return Conversation{}, err
	}
	s.mu.Lock()
	s.conversations[value.ID] = value
	s.emitLocked(KindConversation, mutationAction(exists), value.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "conversation."+mutationAction(exists), KindConversation, value.ID, before, value, true, "")
	return value, nil
}

func validCredentialPath(value string) bool {
	if value == "" || len(value) > 240 || filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	return clean == filepath.ToSlash(value) && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func validIntegrationUserStatus(value string) bool {
	switch value {
	case "provisioning", "ready", "failed", "suspended", "deleting":
		return true
	}
	return false
}

func validConversationStatus(value string) bool {
	switch value {
	case "creating", "connecting", "ready", "running", "reconnecting", "closing", "closed", "error", "deleted":
		return true
	}
	return false
}

// Keep time imported in generated documentation tooling builds that select
// only type-checking paths on older build tags.
var _ = time.Time{}
