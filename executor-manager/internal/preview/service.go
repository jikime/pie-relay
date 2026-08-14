package preview

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/previewtoken"
)

type Options struct {
	Domain           string
	PublicScheme     string
	PublicPort       int
	GatewayContainer string
	AccessSecret     []byte
	ReconcileEvery   time.Duration
	DefaultTTL       time.Duration
	MaxTTL           time.Duration
	LaunchTTL        time.Duration
	Random           io.Reader
}

type Service struct {
	control   *control.Service
	manager   *manager.Manager
	options   Options
	ctx       context.Context
	stop      context.CancelFunc
	wg        sync.WaitGroup
	runMu     sync.Mutex
	closed    bool
	closeOnce sync.Once
}

type CreateRequest struct {
	ID          string
	Integration control.Integration
	Binding     control.IntegrationUser
	Project     control.Project
	AppPath     string
	Profile     string
	Visibility  string
	TTL         time.Duration
	Meta        control.MutationMeta
}

type Launch struct {
	Preview   control.Preview `json:"preview"`
	URL       string          `json:"url"`
	AccessURL string          `json:"accessUrl,omitempty"`
}

type Route struct {
	ID            string `json:"id"`
	Hostname      string `json:"hostname"`
	Backend       string `json:"backend"`
	Port          int    `json:"port"`
	Visibility    string `json:"visibility"`
	AccessVersion int64  `json:"accessVersion,omitempty"`
}

func New(ctx context.Context, controlService *control.Service, executorManager *manager.Manager, options Options) (*Service, error) {
	options.Domain = strings.ToLower(strings.TrimSpace(options.Domain))
	options.PublicScheme = strings.ToLower(strings.TrimSpace(options.PublicScheme))
	if controlService == nil || executorManager == nil {
		return nil, errors.New("preview control and executor manager are required")
	}
	if options.Domain == "" || strings.ContainsAny(options.Domain, "/:") || strings.TrimSpace(options.GatewayContainer) == "" || len(options.AccessSecret) < 32 {
		return nil, errors.New("preview domain, gateway container, and 32-byte access secret are required")
	}
	if options.PublicScheme == "" {
		options.PublicScheme = "https"
	}
	if options.PublicScheme != "http" && options.PublicScheme != "https" {
		return nil, errors.New("preview public scheme must be http or https")
	}
	if options.PublicPort < 0 || options.PublicPort > 65535 {
		return nil, errors.New("preview public port is invalid")
	}
	if options.ReconcileEvery <= 0 {
		options.ReconcileEvery = 2 * time.Second
	}
	if options.DefaultTTL <= 0 {
		options.DefaultTTL = 4 * time.Hour
	}
	if options.MaxTTL <= 0 {
		options.MaxTTL = 24 * time.Hour
	}
	if options.LaunchTTL <= 0 {
		options.LaunchTTL = 2 * time.Minute
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	workerCtx, cancel := context.WithCancel(ctx)
	service := &Service{control: controlService, manager: executorManager, options: options, ctx: workerCtx, stop: cancel}
	service.wg.Add(1)
	go service.loop(workerCtx)
	return service, nil
}

func (s *Service) Close() {
	if s == nil || s.stop == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.runMu.Lock()
		s.closed = true
		s.stop()
		s.runMu.Unlock()
	})
	s.wg.Wait()
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Launch, bool, error) {
	if !manager.ValidUserID(request.ID) || request.Integration.ID == "" || request.Binding.ID == "" || request.Project.ID == "" || request.Project.Status != "ready" || request.Project.IntegrationID != request.Integration.ID || request.Project.IntegrationUserID != request.Binding.ID || request.Project.OwnerUserID != request.Binding.OwnerUserID {
		return Launch{}, false, control.ErrInvalid
	}
	profile := strings.TrimSpace(request.Profile)
	if profile == "" {
		profile = "auto"
	}
	visibility := strings.TrimSpace(request.Visibility)
	if visibility == "" {
		visibility = "private"
	}
	ttl := request.TTL
	if ttl <= 0 {
		ttl = s.options.DefaultTTL
	}
	requestedAppPath := request.AppPath
	if strings.TrimSpace(requestedAppPath) == "" && request.Project.PreviewAppPath != "" {
		requestedAppPath = request.Project.PreviewAppPath
	}
	// Preserve idempotency for integrations created before Project stored a
	// preview app selection: the original record remains the source of truth.
	if strings.TrimSpace(requestedAppPath) == "" {
		if existing, ok := s.control.Preview(request.ID); ok && existing.IntegrationID == request.Integration.ID && existing.IntegrationUserID == request.Binding.ID && existing.ProjectID == request.Project.ID && existing.OwnerUserID == request.Binding.OwnerUserID {
			requestedAppPath = existing.AppPath
		}
	}
	appPath, appPathOK := control.NormalizePreviewAppPath(requestedAppPath)
	if !appPathOK || !validProfile(profile) || !validVisibility(visibility) || ttl > s.options.MaxTTL {
		return Launch{}, false, control.ErrInvalid
	}
	request.AppPath, request.Profile, request.Visibility, request.TTL = appPath, profile, visibility, ttl
	var launch Launch
	var duplicate bool
	// The slot lock, rather than the caller's idempotency-derived preview ID,
	// makes creation a singleton operation across browser tabs, integrations,
	// and Manager replicas for one project application.
	err := s.control.WithRecordOperation(ctx, "preview-slot", previewSlotID(request), func() error {
		var operationErr error
		launch, duplicate, operationErr = s.create(ctx, request)
		return operationErr
	})
	if err == nil {
		s.cleanupDuplicatePreviews(ctx, launch.Preview, request.Meta)
	}
	return launch, duplicate, err
}

func (s *Service) create(ctx context.Context, request CreateRequest) (Launch, bool, error) {
	if existing, ok := s.control.Preview(request.ID); ok {
		if existing.IntegrationID != request.Integration.ID || existing.IntegrationUserID != request.Binding.ID || existing.ProjectID != request.Project.ID || existing.OwnerUserID != request.Binding.OwnerUserID {
			return Launch{}, false, control.ErrForbidden
		}
		existingAppPath, _ := control.NormalizePreviewAppPath(existing.AppPath)
		if existingAppPath != request.AppPath {
			return Launch{}, false, control.ErrConflict
		}
	}
	// PreviewsForProject orders active records before historical ones and the
	// newest record first. Reuse that canonical record even when a different
	// client idempotency key was supplied, preserving its hostname and port.
	for _, existing := range s.control.PreviewsForProject(request.Project.ID, 0) {
		existingAppPath, _ := control.NormalizePreviewAppPath(existing.AppPath)
		if existing.IntegrationID == request.Integration.ID && existing.IntegrationUserID == request.Binding.ID && existing.OwnerUserID == request.Binding.OwnerUserID && existingAppPath == request.AppPath {
			launch, err := s.Launch(existing)
			return launch, true, err
		}
	}
	hostname, err := s.newHostname()
	if err != nil {
		return Launch{}, false, err
	}
	backend, err := s.manager.EnsurePreviewNetwork(ctx, request.Binding.OwnerUserID, s.options.GatewayContainer)
	if err != nil {
		return Launch{}, false, err
	}
	expires := time.Now().UTC().Add(request.TTL)
	var created control.Preview
	for attempt := 0; attempt < 16; attempt++ {
		port, allocateErr := s.allocatePort(request.Binding.ID)
		if allocateErr != nil {
			return Launch{}, false, allocateErr
		}
		created, err = s.control.PutPreview(ctx, control.Preview{
			ID: request.ID, IntegrationID: request.Integration.ID, IntegrationUserID: request.Binding.ID,
			OwnerUserID: request.Binding.OwnerUserID, ProjectID: request.Project.ID, AppPath: request.AppPath, Hostname: hostname,
			BackendHost: backend, Port: port, Profile: request.Profile, Visibility: request.Visibility, AccessVersion: 1, Status: "starting", ExpiresAt: &expires,
		}, 0, request.Meta)
		if err == nil {
			break
		}
		if !errors.Is(err, control.ErrConflict) {
			return Launch{}, false, err
		}
		if refreshErr := s.control.Refresh(ctx); refreshErr != nil {
			return Launch{}, false, errors.Join(err, refreshErr)
		}
		if existing, ok := s.control.Preview(request.ID); ok {
			if existing.IntegrationID != request.Integration.ID || existing.IntegrationUserID != request.Binding.ID || existing.ProjectID != request.Project.ID || existing.OwnerUserID != request.Binding.OwnerUserID {
				return Launch{}, false, control.ErrForbidden
			}
			existingAppPath, _ := control.NormalizePreviewAppPath(existing.AppPath)
			if existingAppPath != request.AppPath {
				return Launch{}, false, control.ErrConflict
			}
			launch, launchErr := s.Launch(existing)
			return launch, true, launchErr
		}
	}
	if err != nil {
		return Launch{}, false, err
	}
	s.reconcileAsync(created.ID)
	launch, err := s.Launch(created)
	return launch, false, err
}

// SetVisibility updates the access policy without replacing the preview
// process, hostname, or port. The access generation revokes launch/session
// credentials issued under an older policy.
func (s *Service) SetVisibility(ctx context.Context, value control.Preview, visibility string, meta control.MutationMeta) (Launch, error) {
	visibility = strings.TrimSpace(visibility)
	if !validVisibility(visibility) {
		return Launch{}, control.ErrInvalid
	}
	var launch Launch
	err := s.control.WithRecordOperation(ctx, control.KindPreview, value.ID, func() error {
		updated, updateErr := s.updatePreview(ctx, value.ID, meta, func(current *control.Preview) (bool, error) {
			if current.Visibility == visibility {
				return false, nil
			}
			current.Visibility = visibility
			current.AccessVersion++
			return true, nil
		})
		if updateErr != nil {
			return updateErr
		}
		launch, updateErr = s.Launch(updated)
		return updateErr
	})
	if err == nil {
		s.cleanupDuplicatePreviews(ctx, launch.Preview, meta)
	}
	return launch, err
}

func (s *Service) Stop(ctx context.Context, value control.Preview, meta control.MutationMeta) (control.Preview, error) {
	var result control.Preview
	err := s.control.WithRecordOperation(ctx, control.KindPreview, value.ID, func() error {
		var operationErr error
		result, operationErr = s.stopPreview(ctx, value, meta)
		return operationErr
	})
	return result, err
}

// Delete stops any remaining runtime before permanently removing the Preview
// control record. Audit records are intentionally retained by Control.
func (s *Service) Delete(ctx context.Context, value control.Preview, meta control.MutationMeta) (control.Preview, error) {
	var deleted control.Preview
	err := s.control.WithRecordOperation(ctx, control.KindPreview, value.ID, func() error {
		current, ok := s.control.Preview(value.ID)
		if !ok {
			return control.ErrNotFound
		}
		var operationErr error
		if current.Status == "stopped" || current.Status == "failed" {
			operationErr = s.manager.StopPreview(ctx, current.OwnerUserID, current.ID)
			if errors.Is(operationErr, manager.ErrExecutorNotFound) {
				operationErr = nil
			}
		} else {
			current, operationErr = s.stopPreview(ctx, current, meta)
		}
		if operationErr != nil {
			return operationErr
		}
		if err := s.control.DeletePreview(ctx, current.ID, current.Version, meta); err != nil {
			return err
		}
		deleted = current
		return nil
	})
	return deleted, err
}

func (s *Service) stopPreview(ctx context.Context, value control.Preview, meta control.MutationMeta) (control.Preview, error) {
	current, ok := s.control.Preview(value.ID)
	if !ok {
		return control.Preview{}, control.ErrNotFound
	}
	if current.Status == "stopped" {
		return current, nil
	}
	updated, err := s.updatePreview(ctx, current.ID, meta, func(value *control.Preview) (bool, error) {
		if value.Status == "stopped" {
			return false, nil
		}
		value.Status, value.LastError = "stopping", ""
		return true, nil
	})
	if err != nil {
		return control.Preview{}, err
	}
	if updated.Status == "stopped" {
		return updated, nil
	}
	stopErr := s.manager.StopPreview(ctx, updated.OwnerUserID, updated.ID)
	if errors.Is(stopErr, manager.ErrExecutorNotFound) {
		stopErr = nil
	}
	result, persistErr := s.updatePreview(ctx, updated.ID, meta, func(value *control.Preview) (bool, error) {
		value.Status, value.LastError = "stopped", ""
		if stopErr != nil {
			value.Status, value.LastError = "failed", bounded(stopErr.Error(), 2000)
		}
		return true, nil
	})
	return result, errors.Join(stopErr, persistErr)
}

func (s *Service) Restart(ctx context.Context, value control.Preview, meta control.MutationMeta) (Launch, error) {
	var launch Launch
	err := s.control.WithRecordOperation(ctx, control.KindPreview, value.ID, func() error {
		var operationErr error
		launch, operationErr = s.restart(ctx, value, meta)
		return operationErr
	})
	return launch, err
}

func (s *Service) restart(ctx context.Context, value control.Preview, meta control.MutationMeta) (Launch, error) {
	current, ok := s.control.Preview(value.ID)
	if !ok {
		return Launch{}, control.ErrNotFound
	}
	if err := s.manager.StopPreview(ctx, current.OwnerUserID, current.ID); err != nil && !errors.Is(err, manager.ErrExecutorNotFound) {
		return Launch{}, err
	}
	updated, err := s.updatePreview(ctx, current.ID, meta, func(value *control.Preview) (bool, error) {
		value.Status, value.LastError, value.StartAttempts, value.NextRetryAt = "starting", "", 0, nil
		expires := time.Now().UTC().Add(s.options.DefaultTTL)
		value.ExpiresAt = &expires
		return true, nil
	})
	if err != nil {
		return Launch{}, err
	}
	s.reconcileAsync(updated.ID)
	return s.Launch(updated)
}

func (s *Service) Logs(ctx context.Context, value control.Preview) ([]byte, error) {
	return s.manager.PreviewLogs(ctx, value.OwnerUserID, value.ID)
}

func (s *Service) Route(hostname string) (Route, bool) {
	value, ok := s.control.PreviewByHostname(strings.ToLower(strings.TrimSpace(hostname)))
	if !ok || value.Status != "ready" || value.BackendHost == "" || value.ExpiresAt != nil && !value.ExpiresAt.After(time.Now().UTC()) {
		return Route{}, false
	}
	integration, integrationOK := s.control.Integration(value.IntegrationID)
	binding, bindingOK := s.control.IntegrationUser(value.IntegrationUserID)
	project, projectOK := s.control.Project(value.ProjectID)
	if !integrationOK || integration.Status != "active" || !bindingOK || binding.Status != "ready" || !projectOK || project.Status != "ready" || binding.OwnerUserID != value.OwnerUserID || project.OwnerUserID != value.OwnerUserID {
		return Route{}, false
	}
	return Route{ID: value.ID, Hostname: value.Hostname, Backend: value.BackendHost, Port: value.Port, Visibility: value.Visibility, AccessVersion: value.AccessVersion}, true
}

func (s *Service) Launch(value control.Preview) (Launch, error) {
	host := value.Hostname
	if s.options.PublicPort > 0 && !(s.options.PublicScheme == "https" && s.options.PublicPort == 443) && !(s.options.PublicScheme == "http" && s.options.PublicPort == 80) {
		host = fmt.Sprintf("%s:%d", host, s.options.PublicPort)
	}
	base := s.options.PublicScheme + "://" + host + "/"
	launch := Launch{Preview: value, URL: base}
	if value.Visibility == "public" {
		return launch, nil
	}
	token, err := previewtoken.Issue(s.options.AccessSecret, previewtoken.Claims{Type: "launch", PreviewID: value.ID, Hostname: value.Hostname, AccessVersion: value.AccessVersion}, s.options.LaunchTTL)
	if err != nil {
		return Launch{}, err
	}
	parsed, _ := url.Parse(base)
	query := parsed.Query()
	query.Set("__pie_token", token)
	parsed.RawQuery = query.Encode()
	launch.AccessURL = parsed.String()
	return launch, nil
}

func (s *Service) loop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.options.ReconcileEvery)
	defer ticker.Stop()
	s.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcile(ctx)
		}
	}
}

func (s *Service) reconcile(ctx context.Context) {
	canonicalSlots := map[string]string{}
	for _, value := range s.control.Previews() {
		if value.Status == "stopped" || value.Status == "failed" {
			continue
		}
		appPath, _ := control.NormalizePreviewAppPath(value.AppPath)
		slot := value.ProjectID + "\x00" + appPath
		if canonicalID := canonicalSlots[slot]; canonicalID != "" && canonicalID != value.ID {
			stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, err := s.Stop(stopCtx, value, control.MutationMeta{ActorUserID: "preview-controller"})
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Printf("stop duplicate preview %s (canonical %s): %v", value.ID, canonicalID, err)
			}
			continue
		}
		canonicalSlots[slot] = value.ID
		if value.ExpiresAt != nil && !value.ExpiresAt.After(time.Now().UTC()) {
			stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, err := s.Stop(stopCtx, value, control.MutationMeta{ActorUserID: "preview-controller"})
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Printf("stop expired preview %s: %v", value.ID, err)
			}
			continue
		}
		s.reconcileOne(ctx, value.ID)
	}
}

func (s *Service) reconcileOne(ctx context.Context, previewID string) {
	err := s.control.WithLocalRecordOperation(ctx, control.KindPreview, previewID, func() error {
		s.reconcileOneLocked(ctx, previewID)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		log.Printf("reconcile preview %s lock: %v", previewID, err)
	}
}

func (s *Service) reconcileAsync(previewID string) {
	s.runMu.Lock()
	if s.closed {
		s.runMu.Unlock()
		return
	}
	s.wg.Add(1)
	s.runMu.Unlock()
	go func() {
		defer s.wg.Done()
		s.reconcileOne(s.ctx, previewID)
	}()
}

func (s *Service) reconcileOneLocked(ctx context.Context, previewID string) {
	value, ok := s.control.Preview(previewID)
	if !ok || value.Status == "stopping" || value.Status == "stopped" || value.Status == "failed" {
		return
	}
	if !s.active(value) {
		stopErr := s.manager.StopPreview(ctx, value.OwnerUserID, value.ID)
		lastError := "preview owner or project is inactive"
		if stopErr != nil && !errors.Is(stopErr, manager.ErrExecutorNotFound) {
			lastError = bounded(lastError+"; process cleanup: "+stopErr.Error(), 2000)
		}
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = s.updatePreview(persistCtx, value.ID, control.MutationMeta{ActorUserID: "preview-controller"}, func(current *control.Preview) (bool, error) {
			if current.Status == "stopped" && current.LastError == lastError {
				return false, nil
			}
			current.Status, current.NextRetryAt, current.LastError = "stopped", nil, lastError
			return true, nil
		})
		return
	}
	if value.NextRetryAt != nil && value.NextRetryAt.After(time.Now().UTC()) {
		return
	}
	operationCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	workingDir, workingDirOK := control.ProjectPreviewWorkingDir(value.ProjectID, value.AppPath)
	if !workingDirOK {
		s.persistFailure(value, errors.New("invalid preview application path"))
		return
	}
	backend, err := s.manager.EnsurePreviewNetwork(operationCtx, value.OwnerUserID, s.options.GatewayContainer)
	if err == nil {
		value.BackendHost = backend
		err = s.manager.StartPreview(operationCtx, value.OwnerUserID, manager.PreviewSpec{ID: value.ID, ProjectID: value.ProjectID, WorkingDir: workingDir, Hostname: value.Hostname, Profile: value.Profile, Port: value.Port})
	}
	if err != nil {
		s.persistRetry(value, err)
		return
	}
	observed, err := s.manager.ObservePreviews(operationCtx, value.OwnerUserID)
	if err != nil {
		s.persistRetry(value, err)
		return
	}
	for _, observation := range observed {
		if observation.ID != value.ID {
			continue
		}
		if observation.State == "failed" {
			s.persistFailure(value, errors.New(observation.LastError))
			return
		}
		status := "starting"
		if observation.Ready && observation.State == "running" {
			status = "ready"
			now := time.Now().UTC()
			value.LastReadyAt = &now
		}
		lastError := bounded(observation.LastError, 2000)
		_, _ = s.updatePreview(context.WithoutCancel(operationCtx), value.ID, control.MutationMeta{ActorUserID: "preview-controller"}, func(current *control.Preview) (bool, error) {
			if current.Status == "stopping" || current.Status == "stopped" || current.Status == "failed" {
				return false, nil
			}
			if current.Status == status && current.BackendHost == backend && current.LastError == lastError && current.StartAttempts == 0 && current.NextRetryAt == nil {
				return false, nil
			}
			current.Status, current.LastError, current.BackendHost, current.StartAttempts, current.NextRetryAt = status, lastError, backend, 0, nil
			if status == "ready" {
				current.LastReadyAt = value.LastReadyAt
			}
			return true, nil
		})
		return
	}
	s.persistRetry(value, errors.New("preview process did not appear in executor state"))
}

func (s *Service) active(value control.Preview) bool {
	integration, integrationOK := s.control.Integration(value.IntegrationID)
	binding, bindingOK := s.control.IntegrationUser(value.IntegrationUserID)
	project, projectOK := s.control.Project(value.ProjectID)
	return integrationOK && integration.Status == "active" && bindingOK && binding.Status == "ready" && projectOK && project.Status == "ready" && binding.OwnerUserID == value.OwnerUserID && project.OwnerUserID == value.OwnerUserID
}

func (s *Service) persistRetry(value control.Preview, cause error) {
	if cause == nil {
		cause = errors.New("preview start failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.updatePreview(ctx, value.ID, control.MutationMeta{ActorUserID: "preview-controller"}, func(current *control.Preview) (bool, error) {
		if current.Status == "stopping" || current.Status == "stopped" || current.Status == "failed" {
			return false, nil
		}
		current.StartAttempts++
		current.LastError = bounded(cause.Error(), 2000)
		current.Status = "starting"
		if current.StartAttempts >= 8 {
			current.Status = "failed"
			current.NextRetryAt = nil
		} else {
			delay := time.Second << min(current.StartAttempts-1, 6)
			next := time.Now().UTC().Add(delay)
			current.NextRetryAt = &next
		}
		return true, nil
	})
}

func (s *Service) persistFailure(value control.Preview, cause error) {
	if cause == nil {
		cause = errors.New("preview process failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.updatePreview(ctx, value.ID, control.MutationMeta{ActorUserID: "preview-controller"}, func(current *control.Preview) (bool, error) {
		if current.Status == "stopping" || current.Status == "stopped" {
			return false, nil
		}
		current.Status, current.LastError, current.NextRetryAt = "failed", bounded(cause.Error(), 2000), nil
		return true, nil
	})
}

func (s *Service) allocatePort(bindingID string) (int, error) {
	used := map[int]bool{}
	for _, value := range s.control.PreviewsForIntegrationUser(bindingID) {
		// Historical stopped/failed records remain available for audit, but no
		// longer own a process. Reuse those ports so a long-lived user cannot
		// exhaust the bounded container-local range by repeated create/stop use.
		if value.IntegrationUserID == bindingID && value.Status != "stopped" && value.Status != "failed" {
			used[value.Port] = true
		}
	}
	for port := 20000; port <= 29999; port++ {
		if !used[port] {
			return port, nil
		}
	}
	return 0, control.ErrQuota
}

func (s *Service) newHostname() (string, error) {
	for range 8 {
		var raw [16]byte
		if _, err := io.ReadFull(s.options.Random, raw[:]); err != nil {
			return "", err
		}
		label := "p-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]))
		hostname := label + "." + s.options.Domain
		if _, exists := s.control.PreviewByHostname(hostname); !exists {
			return hostname, nil
		}
	}
	return "", fmt.Errorf("could not allocate unique preview hostname")
}

func previewSlotID(request CreateRequest) string {
	sum := sha256.Sum256([]byte(request.Integration.ID + "\x00" + request.Binding.ID + "\x00" + request.Project.ID + "\x00" + request.AppPath))
	return "slot-" + hex.EncodeToString(sum[:16])
}

func validProfile(value string) bool {
	switch value {
	case "auto", "next", "vite", "npm":
		return true
	default:
		return false
	}
}

func validVisibility(value string) bool { return value == "private" || value == "public" }

func (s *Service) cleanupDuplicatePreviews(ctx context.Context, canonical control.Preview, meta control.MutationMeta) {
	canonicalAppPath, _ := control.NormalizePreviewAppPath(canonical.AppPath)
	for _, candidate := range s.control.PreviewsForProject(canonical.ProjectID, 0) {
		candidateAppPath, _ := control.NormalizePreviewAppPath(candidate.AppPath)
		if candidate.ID == canonical.ID || candidateAppPath != canonicalAppPath || candidate.Status == "stopped" || candidate.Status == "failed" {
			continue
		}
		if _, err := s.Stop(ctx, candidate, meta); err != nil && ctx.Err() == nil {
			log.Printf("stop duplicate preview %s (canonical %s): %v", candidate.ID, canonical.ID, err)
		}
	}
}

func (s *Service) updatePreview(ctx context.Context, id string, meta control.MutationMeta, mutate func(*control.Preview) (bool, error)) (control.Preview, error) {
	var conflict error
	for range 4 {
		current, ok := s.control.Preview(id)
		if !ok {
			return control.Preview{}, control.ErrNotFound
		}
		next := current
		changed, err := mutate(&next)
		if err != nil {
			return control.Preview{}, err
		}
		if !changed {
			return current, nil
		}
		updated, err := s.control.PutPreview(ctx, next, current.Version, meta)
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, control.ErrConflict) {
			return control.Preview{}, err
		}
		conflict = err
		if refreshErr := s.control.Refresh(ctx); refreshErr != nil {
			return control.Preview{}, errors.Join(err, refreshErr)
		}
	}
	return control.Preview{}, conflict
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
