package thirdparty

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/credential"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

var (
	ErrUnauthorized = errors.New("integration service authentication failed")
	ErrInactive     = errors.New("integration is not active")
)

type Service struct {
	Control     *control.Service
	Executors   *manager.Manager
	Credentials *credential.Store
	Random      io.Reader
	userLocks   [64]sync.Mutex
}

type Registered struct {
	Integration  control.Integration `json:"integration"`
	ServiceToken string              `json:"serviceToken"`
}

func (s *Service) Register(ctx context.Context, value control.Integration, meta control.MutationMeta) (Registered, error) {
	if s.Control == nil {
		return Registered{}, errors.New("control service is required")
	}
	created, err := s.Control.PutIntegration(ctx, value, 0, meta)
	if err != nil {
		return Registered{}, err
	}
	token, digest, err := s.newToken()
	if err != nil {
		return Registered{}, err
	}
	if _, err := s.Control.PutIntegrationSecret(ctx, control.IntegrationSecret{ID: created.ID, TokenHash: digest}, 0); err != nil {
		return Registered{}, err
	}
	return Registered{Integration: created, ServiceToken: token}, nil
}

func (s *Service) RotateToken(ctx context.Context, integrationID string) (string, error) {
	if _, ok := s.Control.Integration(integrationID); !ok {
		return "", control.ErrNotFound
	}
	token, digest, err := s.newToken()
	if err != nil {
		return "", err
	}
	secret, exists := s.Control.IntegrationSecret(integrationID)
	expected := int64(0)
	if exists {
		expected = secret.Version
	}
	_, err = s.Control.PutIntegrationSecret(ctx, control.IntegrationSecret{ID: integrationID, TokenHash: digest}, expected)
	return token, err
}

func (s *Service) Authenticate(integrationID, token string) (control.Integration, error) {
	integration, ok := s.Control.Integration(integrationID)
	if !ok || integration.Status != "active" {
		return control.Integration{}, ErrInactive
	}
	secret, ok := s.Control.IntegrationSecret(integrationID)
	if !ok || token == "" {
		return control.Integration{}, ErrUnauthorized
	}
	sum := sha256.Sum256([]byte(token))
	want, err := hex.DecodeString(secret.TokenHash)
	if err != nil || len(want) != len(sum) || subtle.ConstantTimeCompare(sum[:], want) != 1 {
		return control.Integration{}, ErrUnauthorized
	}
	return integration, nil
}

func (s *Service) Provision(ctx context.Context, integration control.Integration, externalUserID string, rawCredential json.RawMessage, meta control.MutationMeta) (control.IntegrationUser, error) {
	if s.Executors == nil || s.Credentials == nil {
		return control.IntegrationUser{}, errors.New("provisioning dependencies are unavailable")
	}
	if integration.Status != "active" {
		return control.IntegrationUser{}, ErrInactive
	}
	externalUserID = strings.TrimSpace(externalUserID)
	if externalUserID == "" || len(externalUserID) > 512 {
		return control.IntegrationUser{}, control.ErrInvalid
	}
	bindingID, ownerID := BindingID(integration.ID, externalUserID), OwnerID(integration.ID, externalUserID)
	unlock := s.lockUser(bindingID)
	defer unlock()
	binding, exists := s.Control.IntegrationUser(bindingID)
	if exists && (binding.IntegrationID != integration.ID || binding.ExternalUserID != externalUserID || binding.OwnerUserID != ownerID) {
		return control.IntegrationUser{}, control.ErrForbidden
	}
	user, userExists := s.Control.User(ownerID)
	if !userExists {
		var err error
		user, err = s.Control.PutUser(ctx, control.User{ID: ownerID, ExternalSubject: externalUserID, OrganizationID: integration.ID, Status: "active"}, 0, meta)
		if err != nil && !errors.Is(err, control.ErrConflict) {
			return control.IntegrationUser{}, err
		}
		if errors.Is(err, control.ErrConflict) {
			user, _ = s.Control.User(ownerID)
		}
	} else if user.Status != "active" {
		user.Status = "active"
		var err error
		user, err = s.Control.PutUser(ctx, user, user.Version, meta)
		if err != nil {
			return control.IntegrationUser{}, err
		}
	}
	if !exists {
		users := 0
		for _, candidate := range s.Control.IntegrationUsers() {
			if candidate.IntegrationID == integration.ID && candidate.Status != "suspended" && candidate.Status != "deleting" {
				users++
			}
		}
		if users >= integration.MaxUsers {
			return control.IntegrationUser{}, control.ErrQuota
		}
		var err error
		binding, err = s.Control.PutIntegrationUser(ctx, control.IntegrationUser{ID: bindingID, IntegrationID: integration.ID, ExternalUserID: externalUserID, OwnerUserID: ownerID, Status: "provisioning"}, 0, meta)
		if err != nil && !errors.Is(err, control.ErrConflict) {
			return control.IntegrationUser{}, err
		}
		if errors.Is(err, control.ErrConflict) {
			binding, _ = s.Control.IntegrationUser(bindingID)
		}
	} else if binding.Status == "failed" || binding.Status == "suspended" || binding.Status == "deleting" {
		binding.Status = "provisioning"
		var err error
		binding, err = s.Control.PutIntegrationUser(ctx, binding, binding.Version, meta)
		if err != nil {
			return control.IntegrationUser{}, err
		}
	}
	if len(rawCredential) > 0 {
		result, err := s.Credentials.Write(ownerID, integration.Credential.TargetPath, rawCredential, integration.Credential.MaxBytes)
		if err != nil {
			return s.failBinding(ctx, binding, meta, err)
		}
		if result.Digest != binding.CredentialDigest {
			binding.CredentialVersion++
			binding.CredentialDigest = result.Digest
			binding.CredentialUpdatedAt = time.Now().UTC()
		}
	}
	executor, err := s.Executors.EnsureWithLimits(ctx, ownerID, manager.ExecutorLimits{CPUs: user.Quota.CPUs, MemoryBytes: user.Quota.MemoryBytes, PIDsLimit: user.Quota.PIDs})
	if err != nil {
		return s.failBinding(ctx, binding, meta, err)
	}
	_, deviceExists := s.Control.Device(executor.ID)
	if !deviceExists {
		_, err = s.Control.RegisterDevice(ctx, control.Device{ID: executor.ID, OwnerUserID: ownerID, Name: integration.DisplayName + " AI Workspace", Kind: "docker", RuntimeID: executor.ID, DesiredState: "running", ObservedState: "provisioning"}, meta)
		if err != nil && !errors.Is(err, control.ErrConflict) {
			return s.failBinding(ctx, binding, meta, err)
		}
	}
	binding.Status = "ready"
	updated, err := s.Control.PutIntegrationUser(ctx, binding, binding.Version, meta)
	if err != nil {
		return binding, err
	}
	return updated, nil
}

func (s *Service) ReplaceCredential(ctx context.Context, integration control.Integration, binding control.IntegrationUser, rawCredential json.RawMessage, meta control.MutationMeta) (control.IntegrationUser, error) {
	unlock := s.lockUser(binding.ID)
	defer unlock()
	if binding.IntegrationID != integration.ID {
		return control.IntegrationUser{}, control.ErrForbidden
	}
	current, ok := s.Control.IntegrationUser(binding.ID)
	if !ok {
		return control.IntegrationUser{}, control.ErrNotFound
	}
	if current.Status != "ready" {
		return control.IntegrationUser{}, control.ErrForbidden
	}
	binding = current
	result, err := s.Credentials.Write(binding.OwnerUserID, integration.Credential.TargetPath, rawCredential, integration.Credential.MaxBytes)
	if err != nil {
		return control.IntegrationUser{}, err
	}
	if result.Digest == binding.CredentialDigest {
		return binding, nil
	}
	binding.CredentialDigest, binding.CredentialVersion, binding.CredentialUpdatedAt = result.Digest, binding.CredentialVersion+1, time.Now().UTC()
	return s.Control.PutIntegrationUser(ctx, binding, binding.Version, meta)
}

func (s *Service) CreateProject(ctx context.Context, integration control.Integration, binding control.IntegrationUser, projectID, name, locale string, meta control.MutationMeta) (control.Project, error) {
	if s.Executors == nil || s.Control == nil {
		return control.Project{}, errors.New("project provisioning dependencies are unavailable")
	}
	if integration.Status != "active" || binding.IntegrationID != integration.ID {
		return control.Project{}, ErrInactive
	}
	unlock := s.lockUser(binding.ID)
	defer unlock()
	currentBinding, ok := s.Control.IntegrationUser(binding.ID)
	if !ok {
		return control.Project{}, control.ErrNotFound
	}
	if currentBinding.IntegrationID != integration.ID || currentBinding.OwnerUserID != binding.OwnerUserID || currentBinding.Status != "ready" {
		return control.Project{}, control.ErrForbidden
	}
	name, locale = strings.TrimSpace(name), strings.TrimSpace(locale)
	if locale == "" {
		locale = "ko"
	}
	project, exists := s.Control.Project(projectID)
	if exists {
		if project.IntegrationID != integration.ID || project.IntegrationUserID != binding.ID || project.OwnerUserID != binding.OwnerUserID || project.Name != name || project.Locale != locale {
			return control.Project{}, control.ErrConflict
		}
		if project.Status == "ready" {
			return project, nil
		}
		if project.Status == "archived" {
			return control.Project{}, control.ErrForbidden
		}
		project.Status, project.LastError = "provisioning", ""
		var err error
		project, err = s.Control.PutProject(ctx, project, project.Version, meta)
		if err != nil {
			return control.Project{}, err
		}
	} else {
		var err error
		project, err = s.Control.PutProject(ctx, control.Project{
			ID: projectID, IntegrationID: integration.ID, IntegrationUserID: binding.ID,
			OwnerUserID: binding.OwnerUserID, Name: name, Locale: locale,
			WorkingDir: control.ProjectWorkingDir(projectID), Status: "provisioning",
		}, 0, meta)
		if err != nil {
			return control.Project{}, err
		}
	}
	if err := s.Executors.InitializeProject(ctx, binding.OwnerUserID, manager.ProjectSpec{ID: project.ID, Name: project.Name, Locale: project.Locale}); err != nil {
		project.Status, project.LastError = "failed", boundedError(err, 2000)
		updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		updated, updateErr := s.Control.PutProject(updateCtx, project, project.Version, meta)
		if updateErr != nil {
			return project, errors.Join(err, updateErr)
		}
		return updated, err
	}
	now := time.Now().UTC()
	project.Status, project.LastError, project.InitializedAt = "ready", "", &now
	return s.Control.PutProject(ctx, project, project.Version, meta)
}

func (s *Service) ProjectApplications(ctx context.Context, integration control.Integration, binding control.IntegrationUser, projectID string) ([]manager.ProjectApplication, error) {
	if s.Executors == nil || s.Control == nil {
		return nil, errors.New("project application discovery dependencies are unavailable")
	}
	project, ok := s.Control.Project(projectID)
	if !ok {
		return nil, control.ErrNotFound
	}
	if integration.Status != "active" || binding.Status != "ready" || project.Status != "ready" || project.IntegrationID != integration.ID || project.IntegrationUserID != binding.ID || project.OwnerUserID != binding.OwnerUserID {
		return nil, control.ErrForbidden
	}
	return s.Executors.DiscoverProjectApplications(ctx, binding.OwnerUserID, project.ID)
}

func (s *Service) SelectProjectApplication(ctx context.Context, integration control.Integration, binding control.IntegrationUser, projectID, appPath string, meta control.MutationMeta) (control.Project, error) {
	normalized, ok := control.NormalizePreviewAppPath(appPath)
	if !ok {
		return control.Project{}, control.ErrInvalid
	}
	applications, err := s.ProjectApplications(ctx, integration, binding, projectID)
	if err != nil {
		return control.Project{}, err
	}
	found := false
	for _, application := range applications {
		if application.Path == normalized {
			found = true
			break
		}
	}
	if !found {
		return control.Project{}, control.ErrInvalid
	}
	var selected control.Project
	err = s.Control.WithRecordOperation(ctx, control.KindProject, projectID, func() error {
		current, exists := s.Control.Project(projectID)
		if !exists {
			return control.ErrNotFound
		}
		if current.Status != "ready" || current.IntegrationID != integration.ID || current.IntegrationUserID != binding.ID || current.OwnerUserID != binding.OwnerUserID {
			return control.ErrForbidden
		}
		if current.PreviewAppPath == normalized {
			selected = current
			return nil
		}
		current.PreviewAppPath = normalized
		updated, updateErr := s.Control.PutProject(ctx, current, current.Version, meta)
		if updateErr == nil {
			selected = updated
		}
		return updateErr
	})
	return selected, err
}

func (s *Service) Suspend(ctx context.Context, integration control.Integration, binding control.IntegrationUser, meta control.MutationMeta) (control.IntegrationUser, error) {
	unlock := s.lockUser(binding.ID)
	defer unlock()
	if binding.IntegrationID != integration.ID {
		return control.IntegrationUser{}, control.ErrForbidden
	}
	current, ok := s.Control.IntegrationUser(binding.ID)
	if !ok {
		return control.IntegrationUser{}, control.ErrNotFound
	}
	if current.Status == "suspended" {
		return current, nil
	}
	binding = current
	// Revoke routing before touching any runtime. This closes the authorization
	// window immediately and also tells background reconcilers not to revive a
	// preview while its Executor is being removed.
	binding.Status = "deleting"
	var err error
	binding, err = s.Control.PutIntegrationUser(ctx, binding, binding.Version, meta)
	if err != nil {
		return control.IntegrationUser{}, err
	}
	for _, conversation := range s.Control.Conversations() {
		if conversation.IntegrationUserID != binding.ID || conversation.Status == "closed" || conversation.Status == "deleted" {
			continue
		}
		_ = s.Executors.StopSession(ctx, binding.OwnerUserID, conversation.SessionID)
		if session, ok := s.Control.Session(conversation.SessionID); ok && session.Status != "closed" {
			session.Status, session.LastError = "closed", "integration user suspended"
			_, _ = s.Control.PutSession(ctx, session, session.Version, meta)
		}
		conversation.Status = "closed"
		_, _ = s.Control.PutConversation(ctx, conversation, conversation.Version, meta)
	}
	if err := s.Credentials.Remove(binding.OwnerUserID, integration.Credential.TargetPath); err != nil {
		return control.IntegrationUser{}, err
	}
	if _, err := s.Executors.StopExecutor(ctx, binding.OwnerUserID); err != nil && !errors.Is(err, manager.ErrExecutorNotFound) {
		return control.IntegrationUser{}, err
	}
	if user, ok := s.Control.User(binding.OwnerUserID); ok {
		user.Status = "deleted"
		_, _ = s.Control.PutUser(ctx, user, user.Version, meta)
	}
	// Keep a tombstone so a repeated DELETE is idempotent and an explicit PUT
	// can reactivate the same stable owner later. The runtime Stop operation
	// removes the actual container; no active workload remains in this state.
	binding.Status, binding.CredentialDigest = "suspended", ""
	return s.Control.PutIntegrationUser(ctx, binding, binding.Version, meta)
}

func (s *Service) failBinding(ctx context.Context, binding control.IntegrationUser, meta control.MutationMeta, cause error) (control.IntegrationUser, error) {
	binding.Status = "failed"
	updated, updateErr := s.Control.PutIntegrationUser(ctx, binding, binding.Version, meta)
	if updateErr != nil {
		return binding, errors.Join(cause, updateErr)
	}
	return updated, cause
}

func boundedError(err error, limit int) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func BindingID(integrationID, externalUserID string) string {
	return "iu-" + stableHex(integrationID+"\x00"+externalUserID, 20)
}
func OwnerID(integrationID, externalUserID string) string {
	return "ext-" + stableHex(integrationID+"\x00"+externalUserID, 20)
}

func stableHex(value string, bytes int) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:bytes])
}

func (s *Service) newToken() (string, string, error) {
	reader := s.Random
	if reader == nil {
		reader = rand.Reader
	}
	var raw [32]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return "", "", fmt.Errorf("generate integration token: %w", err)
	}
	token := "pie_int_" + hex.EncodeToString(raw[:])
	digest := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(digest[:]), nil
}

func (s *Service) lockUser(bindingID string) func() {
	sum := sha256.Sum256([]byte(bindingID))
	lock := &s.userLocks[int(sum[0])%len(s.userLocks)]
	lock.Lock()
	return lock.Unlock
}
