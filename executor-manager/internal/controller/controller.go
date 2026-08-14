package controller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/capability"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

type Options struct {
	NodeID                string
	Image                 string
	ReconcileInterval     time.Duration
	ReconcileConcurrency  int
	HeartbeatTimeout      time.Duration
	RelayHeartbeatTimeout time.Duration
	OperationConcurrency  int
	OperationTimeout      time.Duration
	RepairBaseDelay       time.Duration
	RepairMaxDelay        time.Duration
	MaintenanceInterval   time.Duration
	OperationRetention    time.Duration
	RelayURL              string
	RelayControlURL       string
	RelayControl          RelayControl
	Issuer                capability.Issuer
	ClaudeOAuth           ClaudeOAuthProvider
}

// ClaudeOAuthProvider is implemented by the Manager-owned credential broker.
// The secret never crosses an admin HTTP API; it is consumed only while the
// controller starts a Docker chat session.
type ClaudeOAuthProvider interface {
	CurrentOAuthToken(context.Context) (token, version string, err error)
}

type Controller struct {
	manager  *manager.Manager
	control  *control.Service
	options  Options
	stop     context.CancelFunc
	wg       sync.WaitGroup
	slots    chan struct{}
	repairMu sync.Mutex
	repairs  map[string]repairState
}

type repairState struct {
	failures int
	next     time.Time
}

func New(ctx context.Context, executorManager *manager.Manager, controlService *control.Service, options Options) (*Controller, error) {
	if executorManager == nil || controlService == nil {
		return nil, errors.New("manager and control service are required")
	}
	if options.NodeID == "" {
		options.NodeID = "default"
	}
	if options.ReconcileInterval <= 0 {
		options.ReconcileInterval = 10 * time.Second
	}
	if options.ReconcileConcurrency <= 0 {
		options.ReconcileConcurrency = 8
	}
	if options.HeartbeatTimeout <= 0 {
		options.HeartbeatTimeout = 45 * time.Second
	}
	if options.RelayHeartbeatTimeout <= 0 {
		options.RelayHeartbeatTimeout = 90 * time.Second
	}
	if options.OperationConcurrency <= 0 {
		options.OperationConcurrency = 4
	}
	if options.OperationTimeout <= 0 {
		options.OperationTimeout = 2 * time.Minute
	}
	if options.RepairBaseDelay <= 0 {
		options.RepairBaseDelay = 5 * time.Second
	}
	if options.RepairMaxDelay <= 0 {
		options.RepairMaxDelay = 5 * time.Minute
	}
	if options.MaintenanceInterval <= 0 {
		options.MaintenanceInterval = time.Hour
	}
	if options.OperationRetention <= 0 {
		options.OperationRetention = 7 * 24 * time.Hour
	}
	workerCtx, cancel := context.WithCancel(ctx)
	c := &Controller{manager: executorManager, control: controlService, options: options, stop: cancel, slots: make(chan struct{}, options.OperationConcurrency), repairs: map[string]repairState{}}
	if err := c.reconcile(workerCtx); err != nil {
		log.Printf("initial Pie Control reconciliation: %v", err)
	}
	c.wg.Add(1)
	go c.loop(workerCtx)
	return c, nil
}

func (c *Controller) Close() {
	if c == nil || c.stop == nil {
		return
	}
	c.stop()
	c.wg.Wait()
}

func (c *Controller) loop(ctx context.Context) {
	defer c.wg.Done()
	reconcileTicker := time.NewTicker(c.options.ReconcileInterval)
	operationTicker := time.NewTicker(500 * time.Millisecond)
	sessionTicker := time.NewTicker(time.Second)
	maintenanceTicker := time.NewTicker(c.options.MaintenanceInterval)
	defer reconcileTicker.Stop()
	defer operationTicker.Stop()
	defer sessionTicker.Stop()
	defer maintenanceTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-reconcileTicker.C:
			if err := c.reconcile(ctx); err != nil {
				log.Printf("Pie Control reconciliation: %v", err)
			}
		case <-operationTicker.C:
			c.dispatchOperations(ctx)
		case <-sessionTicker.C:
			c.startDockerSessions(ctx)
		case <-maintenanceTicker.C:
			removed, err := c.control.PruneTerminalOperations(ctx, time.Now().Add(-c.options.OperationRetention), 1000)
			if err != nil {
				log.Printf("prune completed operations: %v", err)
			} else if removed > 0 {
				log.Printf("pruned %d completed Pie Control operations", removed)
			}
		}
	}
}

func (c *Controller) reconcile(ctx context.Context) error {
	if err := c.control.Refresh(ctx); err != nil {
		return err
	}
	_, relayMigrationErr := c.control.MigrateDefaultRelaySessions(ctx, control.MutationMeta{ActorUserID: "controller"})
	_, relayRecoveryErr := c.control.RecoverStaleRelaySessions(ctx, c.options.RelayHeartbeatTimeout, control.MutationMeta{ActorUserID: "controller"})
	if err := c.reconcileNode(ctx); err != nil {
		return errors.Join(relayMigrationErr, relayRecoveryErr, err)
	}
	errs := c.reconcileExecutors(ctx, c.manager.Executors())
	if relayRecoveryErr != nil {
		errs = append(errs, relayRecoveryErr)
	}
	if relayMigrationErr != nil {
		errs = append(errs, relayMigrationErr)
	}
	c.expireLocalDevices(ctx)
	c.reconcileDockerSessions(ctx)
	return errors.Join(errs...)
}

func (c *Controller) reconcileExecutors(ctx context.Context, executors []manager.Executor) []error {
	if len(executors) == 0 {
		return nil
	}
	workers := min(c.options.ReconcileConcurrency, len(executors))
	jobs := make(chan manager.Executor)
	errCh := make(chan error, len(executors))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for executor := range jobs {
				if err := c.reconcileExecutor(ctx, executor); err != nil {
					errCh <- fmt.Errorf("%s: %w", executor.UserID, err)
				}
			}
		}()
	}
	sendCanceled := false
	for _, executor := range executors {
		select {
		case jobs <- executor:
		case <-ctx.Done():
			sendCanceled = true
		}
		if sendCanceled {
			break
		}
	}
	close(jobs)
	wg.Wait()
	close(errCh)
	errs := make([]error, 0, len(errCh)+1)
	for err := range errCh {
		errs = append(errs, err)
	}
	if sendCanceled {
		errs = append(errs, ctx.Err())
	}
	return errs
}

func (c *Controller) reconcileDockerSessions(ctx context.Context) {
	c.startDockerSessions(ctx)
	c.reconcileActiveDockerSessions(ctx)
}

func (c *Controller) startDockerSessions(ctx context.Context) {
	candidates := make([]control.Session, 0)
	for _, session := range c.control.Sessions() {
		if session.ExecutionTarget != "docker" {
			continue
		}
		retryProvisioning := session.Status == "provisioning" && time.Since(session.UpdatedAt) > c.options.OperationTimeout
		retryReconnect := session.Status == "reconnecting" && (session.StartAttempts == 0 || time.Since(session.UpdatedAt) >= dockerSessionRetryDelay(session.StartAttempts))
		retryManagedChat := session.Status == "error" && c.hasRecoverableConversation(session.ID) && time.Since(session.UpdatedAt) >= dockerSessionRetryDelay(session.StartAttempts)
		if session.Status != "starting" && !retryReconnect && !retryProvisioning && !retryManagedChat {
			continue
		}
		candidates = append(candidates, session)
	}
	if len(candidates) == 0 {
		return
	}
	workers := min(c.options.ReconcileConcurrency, len(candidates))
	jobs := make(chan control.Session)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for session := range jobs {
				c.startDockerSessionCandidate(ctx, session)
			}
		}()
	}
	for _, session := range candidates {
		select {
		case jobs <- session:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

func (c *Controller) startDockerSessionCandidate(ctx context.Context, session control.Session) {
	if err := c.startDockerSession(ctx, session); err != nil {
		if ctx.Err() != nil {
			return
		}
		current, ok := c.control.Session(session.ID)
		if !ok || current.Status != "provisioning" {
			return
		}
		// A Relay outage is recoverable. Keep the session in reconnecting so
		// the bounded retry loop can replace the fenced credential as soon as a
		// healthy node returns. Other failures remain explicit errors (managed
		// chat sessions can still recover through their own retry policy).
		if session.Status == "reconnecting" || errors.Is(err, control.ErrNoRelayCapacity) {
			current.Status = "reconnecting"
		} else {
			current.Status = "error"
		}
		current.LastError = err.Error()
		persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = c.control.PutSession(persistCtx, current, current.Version, control.MutationMeta{ActorUserID: "controller"})
		log.Printf("start Docker session %s: %v", session.ID, err)
	}
}

type ownerSessions struct {
	owner    string
	sessions []control.Session
}

func (c *Controller) reconcileActiveDockerSessions(ctx context.Context) {
	grouped := map[string][]control.Session{}
	for _, session := range c.control.Sessions() {
		if session.ExecutionTarget != "docker" || (session.Status != "ready" && session.Status != "active") {
			continue
		}
		grouped[session.OwnerUserID] = append(grouped[session.OwnerUserID], session)
	}
	if len(grouped) == 0 {
		return
	}
	workers := min(c.options.ReconcileConcurrency, len(grouped))
	jobs := make(chan ownerSessions)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				c.reconcileActiveDockerOwner(ctx, job.owner, job.sessions)
			}
		}()
	}
	for owner, sessions := range grouped {
		select {
		case jobs <- ownerSessions{owner: owner, sessions: sessions}:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

func (c *Controller) reconcileActiveDockerOwner(ctx context.Context, owner string, sessions []control.Session) {
	values, err := c.manager.ObserveSessions(ctx, owner)
	if err != nil {
		log.Printf("observe Docker sessions for %s: %v", owner, err)
		return
	}
	observed := make(map[string]manager.SessionObservation, len(values))
	for _, value := range values {
		observed[value.ID] = value
	}
	for _, session := range sessions {
		value, exists := observed[session.ID]
		if exists && (value.State == "running" || value.State == "starting") {
			continue
		}
		current, ok := c.control.Session(session.ID)
		if !ok || (current.Status != "ready" && current.Status != "active") {
			continue
		}
		current.Status = "starting"
		if !exists {
			current.LastError = "recovering session missing from Executor"
		} else if value.LastError != "" {
			current.LastError = "recovering Executor session: " + value.LastError
		} else {
			current.LastError = "recovering Executor session stopped unexpectedly"
		}
		_, _ = c.control.PutSession(ctx, current, current.Version, control.MutationMeta{ActorUserID: "controller"})
	}
}

func (c *Controller) hasRecoverableConversation(sessionID string) bool {
	for _, conversation := range c.control.Conversations() {
		if conversation.SessionID != sessionID {
			continue
		}
		switch conversation.Status {
		case "creating", "connecting", "ready", "running", "reconnecting":
			return true
		}
	}
	return false
}

func dockerSessionRetryDelay(attempts int) time.Duration {
	delay := time.Second
	for attempt := 0; attempt < min(max(attempts, 0), 5); attempt++ {
		delay *= 2
	}
	return min(delay, 30*time.Second)
}

func (c *Controller) startDockerSession(ctx context.Context, session control.Session) error {
	current, ok := c.control.Session(session.ID)
	if !ok {
		return control.ErrNotFound
	}
	// The periodic reconciler and an explicit restart operation may observe the
	// same transition concurrently. If another worker has already completed it,
	// a stale caller must not demote ready/active back to provisioning or start a
	// second Executor PTY.
	switch current.Status {
	case "starting", "reconnecting", "provisioning", "error":
	default:
		return nil
	}
	recoveringRelay := current.Status == "reconnecting"
	current.Status, current.LastError, current.StartAttempts = "provisioning", "", current.StartAttempts+1
	claimed, err := c.control.PutSession(ctx, current, current.Version, control.MutationMeta{ActorUserID: "controller"})
	if err != nil {
		if errors.Is(err, control.ErrConflict) {
			return nil
		}
		return err
	}
	session = claimed
	relayURL := c.options.RelayURL
	if session.ApplicationID != "" || session.PoolID != "" {
		assigned, node, assignErr := c.control.EnsureSessionRelayNode(ctx, session.ID, control.MutationMeta{ActorUserID: "controller"})
		if errors.Is(assignErr, control.ErrNoRelayCapacity) {
			// A product/profile split may leave an open Integration chat session
			// pinned to a pool that no longer has a Relay node. Only that narrowly
			// defined managed-chat case may cross the otherwise immutable routing
			// boundary, and only when the configured default pool is healthy.
			rebound, changed, rebindErr := c.control.RebindManagedChatSessionToDefaultRelayContext(ctx, session.ID, control.MutationMeta{ActorUserID: "controller"})
			if rebindErr != nil && !errors.Is(rebindErr, control.ErrForbidden) && !errors.Is(rebindErr, control.ErrNoRelayCapacity) {
				return rebindErr
			}
			if changed {
				recoveringRelay = true
				assigned, node, assignErr = c.control.EnsureSessionRelayNode(ctx, rebound.ID, control.MutationMeta{ActorUserID: "controller"})
			}
		}
		if assignErr != nil {
			return assignErr
		}
		session = assigned
		// 사용자 Executor는 Relay의 내부 Docker network에 붙이지 않는다.
		// 따라서 Manager 전용 ControlAddress(예: http://relay:13412)를
		// 넘기면 컨테이너에서 이름을 해석할 수 없다. 호스트 WebSocket도
		// 참가자와 같은 노드의 공개 TLS 주소를 사용해야 한다.
		relayURL, err = relayAgentURL(node.Address)
		if err != nil {
			return err
		}
	}
	if relayURL == "" {
		return errors.New("PIE_RELAY_URL is required for Docker sessions")
	}
	if !c.options.Issuer.Enabled() {
		return errors.New("PIE_RELAY_JWT_SECRET is required for Docker sessions")
	}
	device, ok := c.control.Device(session.DeviceID)
	if !ok || device.OwnerUserID != session.OwnerUserID || device.Kind != "docker" {
		return errors.New("Docker session target is unavailable")
	}
	user, ok := c.control.User(session.OwnerUserID)
	if !ok || user.Status != "active" {
		return errors.New("Docker session owner is not active")
	}
	if _, err := c.manager.EnsureWithLimits(ctx, session.OwnerUserID, limitsFromQuota(user.Quota)); err != nil {
		return err
	}
	if recoveringRelay {
		// The Session Manager treats an existing ID as idempotent and does not
		// replace its credential. Remove the old fenced connection first so the
		// recovered session starts with its newly assigned node/generation.
		if err := c.manager.StopSession(ctx, session.OwnerUserID, session.ID); err != nil && !errors.Is(err, manager.ErrExecutorNotFound) {
			return fmt.Errorf("stop fenced Docker session: %w", err)
		}
	}
	minted, err := c.options.Issuer.MintSession(capability.SessionCredential{
		Subject: session.OwnerUserID, Room: session.OwnerUserID, DeviceID: session.DeviceID,
		SessionID: session.ID, ApplicationID: session.ApplicationID, PoolID: session.PoolID,
		TenantID: session.TenantID, ResourceType: session.ResourceType, ResourceID: session.ResourceID,
		AgentID: session.AgentID, Protocol: session.Protocol, ExecutionTarget: "docker", Role: "host",
		Access: "control", RelayNode: session.RelayNodeID, RelayGeneration: session.RelayGeneration,
		TTL: 24 * time.Hour, AllowInvite: session.AccessMode == "shared",
	})
	if err != nil {
		return err
	}
	workingDir := "/workspace"
	if session.ProjectID != "" {
		project, ok := c.control.Project(session.ProjectID)
		if !ok || project.Status != "ready" || project.OwnerUserID != session.OwnerUserID || project.WorkingDir != control.ProjectWorkingDir(project.ID) {
			return errors.New("Docker session project is unavailable")
		}
		workingDir = project.WorkingDir
	}
	var claudeOAuthToken, claudeAuthVersion string
	if session.AgentMode == "chat" && c.options.ClaudeOAuth != nil {
		claudeOAuthToken, claudeAuthVersion, err = c.options.ClaudeOAuth.CurrentOAuthToken(ctx)
		if err != nil {
			return errors.New("Claude subscription authentication is unavailable")
		}
	}
	if err := c.manager.StartSession(ctx, session.OwnerUserID, manager.SessionSpec{
		ID: session.ID, AgentID: session.AgentID, AgentMode: session.AgentMode,
		RelayURL: relayURL, Token: minted.Token, StreamID: session.AgentMode,
		WorkingDir: workingDir, InitialDriver: session.DriverUserID,
		ClaudeOAuthToken: claudeOAuthToken, ClaudeAuthVersion: claudeAuthVersion,
	}); err != nil {
		return err
	}
	current, ok = c.control.Session(session.ID)
	if !ok {
		return control.ErrNotFound
	}
	// Relay presence may promote provisioning -> active before StartSession's
	// HTTP response returns. Never demote that newer observed state back to
	// ready; only complete the state we claimed ourselves.
	if current.Status == "provisioning" {
		current.Status = "ready"
	}
	current.SelectedTransport, current.LastActivityAt, current.LastError = "relay", time.Now().UTC(), ""
	if _, err := c.control.PutSession(ctx, current, current.Version, control.MutationMeta{ActorUserID: "controller"}); err != nil && !errors.Is(err, control.ErrConflict) {
		return err
	}
	return nil
}

func relayAgentURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", errors.New("assigned Relay node URL is invalid")
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", errors.New("assigned Relay node must use http(s) or ws(s)")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if path == "" {
		path = "/ws/agent"
	} else if !strings.HasSuffix(path, "/ws/agent") {
		path += "/ws/agent"
	}
	parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = path, "", "", ""
	return parsed.String(), nil
}

func (c *Controller) reconcileNode(ctx context.Context) error {
	node, exists := c.control.Node(c.options.NodeID)
	if !exists {
		node = control.Node{ID: c.options.NodeID, Kind: "docker", Status: "ready"}
	}
	node.Kind, node.Status, node.LastHeartbeat = "docker", "ready", time.Now().UTC()
	node.Usage.Instances = len(c.manager.Executors())
	_, err := c.control.PutNode(ctx, node, node.Version, control.MutationMeta{ActorUserID: "controller", SkipAudit: true})
	if errors.Is(err, control.ErrConflict) {
		return nil
	}
	return err
}

func (c *Controller) reconcileExecutor(ctx context.Context, executor manager.Executor) error {
	user, exists := c.control.User(executor.UserID)
	if !exists {
		var err error
		user, err = c.control.PutUser(ctx, control.User{ID: executor.UserID, ExternalSubject: executor.UserID, Status: "active"}, 0, control.MutationMeta{ActorUserID: "controller"})
		if err != nil && !errors.Is(err, control.ErrConflict) {
			return err
		}
		if errors.Is(err, control.ErrConflict) {
			user, _ = c.control.User(executor.UserID)
		}
	}
	if user.Status != "active" && executor.Status != "stopped" {
		stopped, err := c.manager.StopExecutor(ctx, executor.UserID)
		if err != nil && !errors.Is(err, manager.ErrExecutorNotFound) {
			return fmt.Errorf("stop executor for %s user: %w", user.Status, err)
		}
		if err == nil {
			executor = stopped
		}
	}
	if user.Status == "active" && executor.Status != "stopped" && (executor.Status != "ready" || !executorMatchesQuota(executor, user.Quota)) {
		updated, err := c.manager.EnsureWithLimits(ctx, executor.UserID, limitsFromQuota(user.Quota))
		if err != nil {
			return fmt.Errorf("apply executor resource quota: %w", err)
		}
		executor = updated
	}
	deviceID := executor.ID
	device, deviceExists := c.control.Device(deviceID)
	if !deviceExists {
		device = control.Device{ID: deviceID, OwnerUserID: executor.UserID, Name: "Docker Workspace", Kind: "docker", RuntimeID: executor.ID, DesiredState: desiredFromExecutor(executor), ObservedState: "provisioning"}
		created, err := c.control.RegisterDevice(ctx, device, control.MutationMeta{ActorUserID: "controller"})
		if err != nil && !errors.Is(err, control.ErrConflict) {
			return err
		}
		if err == nil {
			device = created
		}
	} else if device.RuntimeID == "" {
		device.RuntimeID = executor.ID
		updated, err := c.control.RegisterDevice(ctx, device, control.MutationMeta{ActorUserID: "controller"})
		if err == nil {
			device = updated
		}
	}
	runtime, runtimeExists := c.control.Runtime(executor.ID)
	if !runtimeExists {
		runtime = control.RuntimeInstance{ID: executor.ID, DeviceID: deviceID, OwnerUserID: executor.UserID, NodeID: c.options.NodeID, Image: c.options.Image, DesiredState: desiredFromExecutor(executor), ObservedState: "provisioning", Quota: user.Quota}
		created, err := c.control.PutRuntime(ctx, runtime, 0, control.MutationMeta{ActorUserID: "controller"})
		if err != nil && !errors.Is(err, control.ErrConflict) {
			return err
		}
		if err == nil {
			runtime = created
		}
	}
	observation, observeErr := c.manager.Observe(ctx, executor.UserID)
	if errors.Is(observeErr, manager.ErrExecutorNotFound) && executor.Status != "stopped" && c.repairDue(executor.UserID) {
		if repaired, repairErr := c.manager.EnsureWithLimits(ctx, executor.UserID, limitsFromQuota(user.Quota)); repairErr != nil {
			c.recordRepairFailure(executor.UserID)
			observeErr = fmt.Errorf("automatic repair failed: %w", repairErr)
		} else {
			executor = repaired
			observation, observeErr = c.manager.Observe(ctx, executor.UserID)
			if observeErr == nil {
				c.clearRepair(executor.UserID)
			} else {
				c.recordRepairFailure(executor.UserID)
			}
		}
	} else if observeErr == nil {
		c.clearRepair(executor.UserID)
	}
	now := time.Now().UTC()
	previousRuntime := runtime
	runtime.DesiredState = desiredFromExecutor(executor)
	runtime.Quota = user.Quota
	if observeErr == nil {
		runtime.ContainerID = observation.RuntimeID
		if observation.Image != "" {
			runtime.Image = observation.Image
		}
		runtime.ObservedState = observation.Status
		if observation.Running {
			runtime.ObservedState = "running"
		}
		runtime.Health, runtime.LastError = observation.Health, ""
	} else if errors.Is(observeErr, manager.ErrExecutorNotFound) {
		runtime.ContainerID, runtime.Health = "", "missing"
		if executor.Status == "stopped" {
			runtime.ObservedState = "stopped"
		} else {
			runtime.ObservedState = "error"
		}
		runtime.LastError = observeErr.Error()
	} else {
		runtime.ObservedState, runtime.LastError = "error", observeErr.Error()
	}
	if runtimeObservationChanged(previousRuntime, runtime) || previousRuntime.LastReconciled.IsZero() || now.Sub(previousRuntime.LastReconciled) >= time.Minute {
		runtime.LastReconciled = now
		updatedRuntime, err := c.control.PutRuntime(ctx, runtime, runtime.Version, control.MutationMeta{ActorUserID: "controller", SkipAudit: true})
		if err != nil && !errors.Is(err, control.ErrConflict) {
			return err
		}
		if err == nil {
			runtime = updatedRuntime
		}
	}
	observedState := runtimeDeviceState(runtime)
	if runtime.ObservedState == "running" {
		// Let Control Plane compose runtime health with the latest clientd/Relay
		// presence. Pinning this to "degraded" made the periodic Docker poll
		// overwrite a legitimately online host every reconciliation cycle.
		observedState = ""
	}
	heartbeat := control.DeviceHeartbeat{
		ObservedState: observedState, RuntimeRunning: runtime.ObservedState == "running",
		RuntimeHealthy:  runtime.Health == "healthy" || runtime.Health == "running",
		ClientConnected: device.ClientConnected, RelayRegistered: device.RelayRegistered,
		RelayNodeID: device.RelayNodeID, ActiveSessions: device.ActiveSessions, Metadata: device.Metadata,
	}
	if !deviceHeartbeatChanged(device, heartbeat) && !device.LastHeartbeat.IsZero() && now.Sub(device.LastHeartbeat) < time.Minute {
		return nil
	}
	_, err := c.control.HeartbeatDevice(ctx, deviceID, executor.UserID, heartbeat, control.MutationMeta{ActorUserID: "controller", SkipAudit: true})
	if errors.Is(err, control.ErrConflict) {
		return nil
	}
	return err
}

func runtimeObservationChanged(before, after control.RuntimeInstance) bool {
	return before.ContainerID != after.ContainerID || before.Image != after.Image || before.DesiredState != after.DesiredState ||
		before.ObservedState != after.ObservedState || before.Health != after.Health || before.LastError != after.LastError || before.Quota != after.Quota
}

func limitsFromQuota(quota control.ResourceQuota) manager.ExecutorLimits {
	return manager.ExecutorLimits{CPUs: quota.CPUs, MemoryBytes: quota.MemoryBytes, PIDsLimit: quota.PIDs}
}

func executorMatchesQuota(executor manager.Executor, quota control.ResourceQuota) bool {
	return executor.CPUs == quota.CPUs && executor.MemoryBytes == quota.MemoryBytes && executor.PIDsLimit == quota.PIDs
}

func deviceHeartbeatChanged(device control.Device, heartbeat control.DeviceHeartbeat) bool {
	observed := heartbeat.ObservedState
	if observed == "" {
		switch {
		case heartbeat.RuntimeRunning && !heartbeat.RuntimeHealthy:
			observed = "degraded"
		case heartbeat.ClientConnected && (heartbeat.RelayRegistered || heartbeat.RuntimeRunning):
			observed = "online"
		case heartbeat.RuntimeRunning || heartbeat.ClientConnected:
			observed = "degraded"
		default:
			observed = "offline"
		}
	}
	return device.ObservedState != observed || device.RuntimeRunning != heartbeat.RuntimeRunning ||
		device.RuntimeHealthy != heartbeat.RuntimeHealthy || device.ClientConnected != heartbeat.ClientConnected ||
		device.RelayRegistered != heartbeat.RelayRegistered || device.RelayNodeID != heartbeat.RelayNodeID ||
		device.ActiveSessions != heartbeat.ActiveSessions
}

func (c *Controller) expireLocalDevices(ctx context.Context) {
	cutoff := time.Now().Add(-c.options.HeartbeatTimeout)
	for _, device := range c.control.Devices() {
		if device.Kind != "local" || device.LastHeartbeat.IsZero() || device.LastHeartbeat.After(cutoff) || device.ObservedState == "offline" {
			continue
		}
		_, _ = c.control.HeartbeatDevice(ctx, device.ID, device.OwnerUserID, control.DeviceHeartbeat{ObservedState: "offline", Metadata: device.Metadata}, control.MutationMeta{ActorUserID: "controller"})
	}
}

func (c *Controller) dispatchOperations(ctx context.Context) {
	available := cap(c.slots) - len(c.slots)
	for _, operation := range c.control.QueuedOperations(available) {
		select {
		case c.slots <- struct{}{}:
			c.wg.Add(1)
			go func(id string) { defer c.wg.Done(); defer func() { <-c.slots }(); c.runOperation(ctx, id) }(operation.ID)
		default:
			return
		}
	}
}

func (c *Controller) runOperation(parent context.Context, id string) {
	operation, err := c.control.UpdateOperation(parent, id, "running", 5, nil, "", control.MutationMeta{ActorUserID: "controller"})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, c.options.OperationTimeout)
	defer cancel()
	result, runErr := c.executeOperation(ctx, operation)
	if runErr != nil {
		_, _ = c.control.UpdateOperation(context.Background(), id, "failed", 100, nil, runErr.Error(), control.MutationMeta{ActorUserID: "controller"})
		return
	}
	_, _ = c.control.UpdateOperation(context.Background(), id, "succeeded", 100, result, "", control.MutationMeta{ActorUserID: "controller"})
}

func (c *Controller) executeOperation(ctx context.Context, operation control.Operation) (map[string]any, error) {
	switch operation.Type {
	case "runtime.start", "runtime.stop", "runtime.recreate":
		runtime, ok := c.control.Runtime(operation.TargetID)
		if !ok {
			return nil, control.ErrNotFound
		}
		quota := runtime.Quota
		if operation.Type != "runtime.stop" {
			user, exists := c.control.User(runtime.OwnerUserID)
			if !exists || user.Status != "active" {
				return nil, errors.New("runtime owner is not active")
			}
			quota = user.Quota
		}
		switch operation.Type {
		case "runtime.start":
			if _, err := c.manager.EnsureWithLimits(ctx, runtime.OwnerUserID, limitsFromQuota(quota)); err != nil {
				return nil, err
			}
		case "runtime.stop":
			if _, err := c.manager.StopExecutor(ctx, runtime.OwnerUserID); err != nil {
				return nil, err
			}
		case "runtime.recreate":
			if _, err := c.manager.StopExecutor(ctx, runtime.OwnerUserID); err != nil && !errors.Is(err, manager.ErrExecutorNotFound) {
				return nil, err
			}
			if _, err := c.manager.EnsureWithLimits(ctx, runtime.OwnerUserID, limitsFromQuota(quota)); err != nil {
				return nil, err
			}
		}
		executor, ok := c.manager.Executor(runtime.OwnerUserID)
		if ok {
			_ = c.reconcileExecutor(ctx, executor)
		}
		return map[string]any{"runtimeId": runtime.ID, "action": operation.Type}, nil
	case "session.close", "session.restart":
		session, ok := c.control.Session(operation.TargetID)
		if !ok {
			return nil, control.ErrNotFound
		}
		if session.ExecutionTarget == "docker" {
			if err := c.manager.StopSession(ctx, session.OwnerUserID, session.ID); err != nil && !errors.Is(err, manager.ErrExecutorNotFound) {
				return nil, err
			}
		}
		if operation.Type == "session.restart" {
			if session.ExecutionTarget != "docker" {
				return nil, errors.New("local sessions must be restarted by their owning clientd")
			}
			updated, err := c.transitionSessionToStarting(ctx, session.ID, operation.ActorUserID)
			if err != nil {
				return nil, err
			}
			if err := c.startDockerSession(ctx, updated); err != nil {
				return nil, err
			}
			return map[string]any{"sessionId": updated.ID, "action": operation.Type}, nil
		}
		// Stopping the PTY closes its Relay host connection, and that presence
		// event can update the session version before this operation persists its
		// final state. Always refetch and retry instead of writing the stale value
		// captured before StopSession.
		if err := c.closeControlSession(ctx, session.ID, operation.ActorUserID); err != nil {
			return nil, err
		}
		return map[string]any{"sessionId": session.ID}, nil
	case "participant.disconnect":
		participant, ok := c.control.Participant(operation.TargetID)
		if !ok {
			return nil, control.ErrNotFound
		}
		session, ok := c.control.Session(participant.SessionID)
		if !ok {
			return nil, control.ErrNotFound
		}
		address, err := c.relayControlAddress(session)
		if err != nil {
			return nil, err
		}
		if c.options.RelayControl == nil {
			return nil, errors.New("Relay control is not configured")
		}
		if err := c.options.RelayControl.DisconnectConnection(ctx, address, participant.ConnectionID); err != nil {
			return nil, err
		}
		return map[string]any{"participantId": participant.ID, "connectionId": participant.ConnectionID}, nil
	case "session.driver.set":
		session, ok := c.control.Session(operation.TargetID)
		if !ok {
			return nil, control.ErrNotFound
		}
		if c.options.RelayControl == nil {
			return nil, errors.New("Relay control is not configured")
		}
		address, err := c.relayControlAddress(session)
		if err != nil {
			return nil, err
		}
		userID, _ := operation.Request["userId"].(string)
		room, err := c.relayRoutingRoom(session)
		if err != nil {
			return nil, err
		}
		result, err := c.options.RelayControl.SetDriver(ctx, address, room, session.DeviceID, session.ID, userID, session.RelayGeneration)
		if err != nil {
			return nil, err
		}
		current, ok := c.control.Session(session.ID)
		if !ok {
			return nil, control.ErrNotFound
		}
		current.DriverUserID = result.UserID
		if result.ExpiresAt.IsZero() {
			current.DriverLeaseExpiresAt = nil
		} else {
			expiresAt := result.ExpiresAt
			current.DriverLeaseExpiresAt = &expiresAt
		}
		if _, err := c.control.PutSession(ctx, current, current.Version, control.MutationMeta{ActorUserID: operation.ActorUserID}); err != nil {
			return nil, err
		}
		return map[string]any{"sessionId": session.ID, "driverUserId": result.UserID, "generation": result.Generation}, nil
	case "device.drain":
		device, ok := c.control.Device(operation.TargetID)
		if !ok {
			return nil, control.ErrNotFound
		}
		device.DesiredState, device.ObservedState = "stopped", "draining"
		if updated, err := c.control.RegisterDevice(ctx, device, control.MutationMeta{ActorUserID: operation.ActorUserID}); err != nil {
			return nil, err
		} else {
			device = updated
		}
		closedSessions, disconnected := 0, 0
		for _, session := range c.control.Sessions() {
			if session.DeviceID != device.ID || session.Status == "closed" {
				continue
			}
			if session.HostConnectionID != "" {
				if err := c.disconnectRelayConnection(ctx, session, session.HostConnectionID); err != nil {
					return nil, fmt.Errorf("disconnect session host %s: %w", session.ID, err)
				}
				disconnected++
			}
			for _, participant := range c.control.Participants() {
				if participant.SessionID != session.ID {
					continue
				}
				if participant.ConnectionID != "" {
					if err := c.disconnectRelayConnection(ctx, session, participant.ConnectionID); err != nil {
						return nil, fmt.Errorf("disconnect participant %s: %w", participant.ID, err)
					}
				}
				if err := c.control.DeleteParticipant(ctx, participant.ID, control.MutationMeta{ActorUserID: operation.ActorUserID}); err != nil && !errors.Is(err, control.ErrNotFound) {
					return nil, err
				}
				disconnected++
			}
			if session.ExecutionTarget == "docker" {
				if err := c.manager.StopSession(ctx, session.OwnerUserID, session.ID); err != nil && !errors.Is(err, manager.ErrExecutorNotFound) {
					return nil, err
				}
			}
			if err := c.closeControlSession(ctx, session.ID, operation.ActorUserID); err != nil {
				return nil, err
			}
			closedSessions++
		}
		if device.Kind == "docker" {
			if _, err := c.manager.StopExecutor(ctx, device.OwnerUserID); err != nil && !errors.Is(err, manager.ErrExecutorNotFound) {
				return nil, err
			}
			if runtime, ok := c.control.Runtime(device.RuntimeID); ok {
				runtime.DesiredState, runtime.ObservedState, runtime.Health = "stopped", "stopped", "stopped"
				runtime.ContainerID, runtime.LastError, runtime.LastReconciled = "", "", time.Now().UTC()
				if _, err := c.control.PutRuntime(ctx, runtime, runtime.Version, control.MutationMeta{ActorUserID: operation.ActorUserID}); err != nil {
					return nil, err
				}
			}
		}
		device, _ = c.control.Device(device.ID)
		device.ObservedState, device.ActiveSessions = "stopped", 0
		device.RuntimeRunning, device.RuntimeHealthy, device.ClientConnected, device.RelayRegistered = false, false, false, false
		if _, err := c.control.RegisterDevice(ctx, device, control.MutationMeta{ActorUserID: operation.ActorUserID}); err != nil {
			return nil, err
		}
		return map[string]any{"deviceId": device.ID, "closedSessions": closedSessions, "disconnectedParticipants": disconnected}, nil
	default:
		return nil, fmt.Errorf("unsupported operation type %q", operation.Type)
	}
}

// relayRoutingRoom must mirror capability.Issuer.MintSession. Resource-scoped
// credentials deliberately replace the owner ID with an opaque HMAC room, so
// Relay control operations must address that same room instead of leaking or
// routing by the human owner identifier.
func (c *Controller) relayRoutingRoom(session control.Session) (string, error) {
	if session.ApplicationID == "" && session.TenantID == "" && session.ResourceType == "" && session.ResourceID == "" {
		return session.OwnerUserID, nil
	}
	room, err := c.options.Issuer.OpaqueRoom(session.ApplicationID, session.TenantID, session.ResourceType, session.ResourceID, session.ID)
	if err != nil {
		return "", fmt.Errorf("derive Relay routing room: %w", err)
	}
	return room, nil
}

func (c *Controller) disconnectRelayConnection(ctx context.Context, session control.Session, connectionID string) error {
	if c.options.RelayControl == nil {
		return errors.New("Relay control is not configured")
	}
	address, err := c.relayControlAddress(session)
	if err != nil {
		return err
	}
	return c.options.RelayControl.DisconnectConnection(ctx, address, connectionID)
}

func (c *Controller) closeControlSession(ctx context.Context, sessionID, actor string) error {
	for range 8 {
		if err := ctx.Err(); err != nil {
			return err
		}
		session, ok := c.control.Session(sessionID)
		if !ok {
			return control.ErrNotFound
		}
		if session.Status == "closed" {
			return nil
		}
		session.Status, session.HostConnectionID, session.DriverUserID, session.DriverLeaseExpiresAt = "closed", "", "", nil
		if _, err := c.control.PutSession(ctx, session, session.Version, control.MutationMeta{ActorUserID: actor}); err == nil {
			return nil
		} else if !errors.Is(err, control.ErrConflict) {
			return err
		}
	}
	return control.ErrConflict
}

func (c *Controller) transitionSessionToStarting(ctx context.Context, sessionID, actor string) (control.Session, error) {
	for range 8 {
		if err := ctx.Err(); err != nil {
			return control.Session{}, err
		}
		session, ok := c.control.Session(sessionID)
		if !ok {
			return control.Session{}, control.ErrNotFound
		}
		session.Status, session.LastError, session.HostConnectionID = "starting", "", ""
		session.DriverUserID, session.DriverLeaseExpiresAt = "", nil
		updated, err := c.control.PutSession(ctx, session, session.Version, control.MutationMeta{ActorUserID: actor})
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, control.ErrConflict) {
			return control.Session{}, err
		}
	}
	return control.Session{}, control.ErrConflict
}

func (c *Controller) repairDue(userID string) bool {
	c.repairMu.Lock()
	defer c.repairMu.Unlock()
	state, ok := c.repairs[userID]
	return !ok || !time.Now().Before(state.next)
}

func (c *Controller) recordRepairFailure(userID string) {
	c.repairMu.Lock()
	defer c.repairMu.Unlock()
	state := c.repairs[userID]
	state.failures++
	delay := c.options.RepairBaseDelay
	for attempt := 1; attempt < state.failures && delay < c.options.RepairMaxDelay; attempt++ {
		delay *= 2
	}
	if delay > c.options.RepairMaxDelay {
		delay = c.options.RepairMaxDelay
	}
	state.next = time.Now().Add(delay)
	c.repairs[userID] = state
}

func (c *Controller) clearRepair(userID string) {
	c.repairMu.Lock()
	delete(c.repairs, userID)
	c.repairMu.Unlock()
}

func (c *Controller) relayControlAddress(session control.Session) (string, error) {
	nodeID := session.RelayNodeID
	if nodeID == "" {
		if device, ok := c.control.Device(session.DeviceID); ok {
			nodeID = device.RelayNodeID
		}
	}
	if nodeID != "" {
		if node, ok := c.control.Node(nodeID); ok {
			if address := relayNodeControlAddress(node); address != "" {
				return address, nil
			}
		}
	}
	if c.options.RelayControlURL != "" {
		return c.options.RelayControlURL, nil
	}
	return "", errors.New("Relay node control address is unavailable")
}

func relayNodeControlAddress(node control.Node) string {
	if node.ControlAddress != "" {
		return node.ControlAddress
	}
	return node.Address
}

func desiredFromExecutor(executor manager.Executor) string {
	if executor.Status == "stopped" {
		return "stopped"
	}
	return "running"
}

func runtimeDeviceState(runtime control.RuntimeInstance) string {
	if runtime.ObservedState == "running" {
		if runtime.Health == "unhealthy" {
			return "degraded"
		}
		return "degraded" // runtime is alive; clientd/Relay heartbeat promotes it to online.
	}
	if runtime.ObservedState == "stopped" {
		return "stopped"
	}
	if runtime.ObservedState == "created" || runtime.ObservedState == "restarting" {
		return "starting"
	}
	return "error"
}
