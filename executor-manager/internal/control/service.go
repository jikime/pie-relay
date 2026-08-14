package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNotFound        = errors.New("control object not found")
	ErrInvalid         = errors.New("invalid control object")
	ErrForbidden       = errors.New("control object access forbidden")
	ErrQuota           = errors.New("control quota exceeded")
	ErrNoRelayCapacity = errors.New("no relay node capacity available")
)

type MutationMeta struct {
	ActorUserID string
	RequestID   string
	SourceIP    string
	SkipAudit   bool
	Trusted     bool
}

type DeviceHeartbeat struct {
	ObservedState   string            `json:"observedState"`
	RuntimeRunning  bool              `json:"runtimeRunning"`
	RuntimeHealthy  bool              `json:"runtimeHealthy"`
	ClientConnected bool              `json:"clientConnected"`
	RelayRegistered bool              `json:"relayRegistered"`
	RelayNodeID     string            `json:"relayNodeId,omitempty"`
	ActiveSessions  int               `json:"activeSessions"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

type Service struct {
	store Store
	now   func() time.Time

	mu                 sync.RWMutex
	users              map[string]User
	nodes              map[string]Node
	devices            map[string]Device
	runtimes           map[string]RuntimeInstance
	sessions           map[string]Session
	participants       map[string]Participant
	grants             map[string]AccessGrant
	operations         map[string]Operation
	audit              map[string]AuditEvent
	integrations       map[string]Integration
	integrationSecrets map[string]IntegrationSecret
	integrationUsers   map[string]IntegrationUser
	projects           map[string]Project
	previews           map[string]Preview
	previewHosts       map[string]string
	previewUsers       map[string]map[string]struct{}
	previewProjects    map[string]map[string]struct{}
	conversations      map[string]Conversation
	idempotency        map[string]string
	subscribers        map[chan Event]struct{}
	presenceSeen       map[string]struct{}
	presenceOrder      []string
	sequence           atomic.Uint64
	locks              sync.Map
	fingerprint        [32]byte
	changeCursor       int64
	defaultQuota       ResourceQuota
	defaultApplication string
	defaultRelayPool   string
}

func NewService(ctx context.Context, store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("control store is required")
	}
	s := &Service{
		store: store, now: time.Now,
		users: map[string]User{}, nodes: map[string]Node{}, devices: map[string]Device{},
		runtimes: map[string]RuntimeInstance{}, sessions: map[string]Session{}, participants: map[string]Participant{},
		grants: map[string]AccessGrant{}, operations: map[string]Operation{}, audit: map[string]AuditEvent{},
		integrations: map[string]Integration{}, integrationSecrets: map[string]IntegrationSecret{}, integrationUsers: map[string]IntegrationUser{}, projects: map[string]Project{}, previews: map[string]Preview{},
		previewHosts: map[string]string{}, previewUsers: map[string]map[string]struct{}{}, previewProjects: map[string]map[string]struct{}{}, conversations: map[string]Conversation{},
		idempotency: map[string]string{}, subscribers: map[chan Event]struct{}{}, presenceSeen: map[string]struct{}{},
	}
	var changeCursor int64
	if incremental, ok := store.(IncrementalStore); ok {
		var err error
		// Read the cursor before the snapshot. A concurrent mutation is then
		// either already present in the snapshot or replayed on Refresh; it can
		// never be skipped between these two reads.
		changeCursor, err = incremental.CurrentSequence(ctx)
		if err != nil {
			return nil, err
		}
	}
	records, err := loadCurrentRecords(ctx, store)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if err := s.loadRecord(record); err != nil {
			return nil, fmt.Errorf("load %s/%s: %w", record.Kind, record.ID, err)
		}
	}
	s.fingerprint = fingerprintRecords(records)
	s.changeCursor = changeCursor
	s.pruneAuditMemory(2000)
	return s, nil
}

// Refresh reloads durable state without replacing subscribers or per-record
// locks. Multi-Manager PostgreSQL deployments call this from reconciliation so
// changes committed by another process become visible locally.
func (s *Service) Refresh(ctx context.Context) error {
	shared, ok := s.store.(SharedStore)
	if !ok || !shared.Shared() {
		return nil
	}
	if incremental, ok := s.store.(IncrementalStore); ok {
		return s.refreshIncremental(ctx, incremental)
	}
	records, err := loadCurrentRecords(ctx, s.store)
	if err != nil {
		return err
	}
	fingerprint := fingerprintRecords(records)
	s.mu.RLock()
	unchanged := fingerprint == s.fingerprint
	s.mu.RUnlock()
	if unchanged {
		return nil
	}
	temp := &Service{
		users: map[string]User{}, nodes: map[string]Node{}, devices: map[string]Device{},
		runtimes: map[string]RuntimeInstance{}, sessions: map[string]Session{}, participants: map[string]Participant{},
		grants: map[string]AccessGrant{}, operations: map[string]Operation{}, audit: map[string]AuditEvent{},
		integrations: map[string]Integration{}, integrationSecrets: map[string]IntegrationSecret{}, integrationUsers: map[string]IntegrationUser{}, projects: map[string]Project{}, previews: map[string]Preview{},
		previewHosts: map[string]string{}, previewUsers: map[string]map[string]struct{}{}, previewProjects: map[string]map[string]struct{}{}, conversations: map[string]Conversation{},
		idempotency: map[string]string{},
	}
	for _, record := range records {
		if err := temp.loadRecord(record); err != nil {
			return fmt.Errorf("refresh %s/%s: %w", record.Kind, record.ID, err)
		}
	}
	s.mu.Lock()
	s.users, s.nodes, s.devices, s.runtimes = temp.users, temp.nodes, temp.devices, temp.runtimes
	s.sessions, s.participants, s.grants = temp.sessions, temp.participants, temp.grants
	s.operations, s.audit, s.idempotency = temp.operations, temp.audit, temp.idempotency
	s.integrations, s.integrationSecrets, s.integrationUsers, s.projects, s.previews, s.conversations = temp.integrations, temp.integrationSecrets, temp.integrationUsers, temp.projects, temp.previews, temp.conversations
	s.previewHosts, s.previewUsers, s.previewProjects = temp.previewHosts, temp.previewUsers, temp.previewProjects
	s.fingerprint = fingerprint
	s.emitLocked("registry", "refreshed", "all")
	s.mu.Unlock()
	return nil
}

func (s *Service) refreshIncremental(ctx context.Context, store IncrementalStore) error {
	const batchSize = 1000
	changed := false
	for {
		s.mu.RLock()
		cursor := s.changeCursor
		s.mu.RUnlock()
		changes, err := store.LoadChanges(ctx, cursor, batchSize)
		if err != nil {
			return err
		}
		if len(changes) == 0 {
			break
		}
		s.mu.Lock()
		for _, change := range changes {
			if change.Sequence <= s.changeCursor {
				continue
			}
			if change.Deleted {
				changed = s.deleteRecordLocked(change.Record.Kind, change.Record.ID) || changed
			} else if change.Record.Version > s.recordVersionLocked(change.Record.Kind, change.Record.ID) {
				if err := s.loadRecord(change.Record); err != nil {
					s.mu.Unlock()
					return fmt.Errorf("refresh change %d %s/%s: %w", change.Sequence, change.Record.Kind, change.Record.ID, err)
				}
				changed = true
			}
			s.changeCursor = change.Sequence
		}
		s.pruneAuditMemory(2000)
		s.mu.Unlock()
		if len(changes) < batchSize {
			break
		}
	}
	if changed {
		s.mu.Lock()
		s.emitLocked("registry", "refreshed", "incremental")
		s.mu.Unlock()
	}
	return nil
}

func (s *Service) recordVersionLocked(kind, id string) int64 {
	switch kind {
	case KindUser:
		return s.users[id].Version
	case KindNode:
		return s.nodes[id].Version
	case KindDevice:
		return s.devices[id].Version
	case KindRuntime:
		return s.runtimes[id].Version
	case KindSession:
		return s.sessions[id].Version
	case KindParticipant:
		return s.participants[id].Version
	case KindGrant:
		return s.grants[id].Version
	case KindOperation:
		return s.operations[id].Version
	case KindAudit:
		return s.audit[id].Version
	case KindIntegration:
		return s.integrations[id].Version
	case KindIntegrationSecret:
		return s.integrationSecrets[id].Version
	case KindIntegrationUser:
		return s.integrationUsers[id].Version
	case KindProject:
		return s.projects[id].Version
	case KindPreview:
		return s.previews[id].Version
	case KindConversation:
		return s.conversations[id].Version
	default:
		return 0
	}
}

func (s *Service) deleteRecordLocked(kind, id string) bool {
	switch kind {
	case KindUser:
		if _, ok := s.users[id]; !ok {
			return false
		}
		delete(s.users, id)
	case KindNode:
		if _, ok := s.nodes[id]; !ok {
			return false
		}
		delete(s.nodes, id)
	case KindDevice:
		if _, ok := s.devices[id]; !ok {
			return false
		}
		delete(s.devices, id)
	case KindRuntime:
		if _, ok := s.runtimes[id]; !ok {
			return false
		}
		delete(s.runtimes, id)
	case KindSession:
		if _, ok := s.sessions[id]; !ok {
			return false
		}
		delete(s.sessions, id)
	case KindParticipant:
		if _, ok := s.participants[id]; !ok {
			return false
		}
		delete(s.participants, id)
	case KindGrant:
		if _, ok := s.grants[id]; !ok {
			return false
		}
		delete(s.grants, id)
	case KindOperation:
		operation, ok := s.operations[id]
		if !ok {
			return false
		}
		delete(s.operations, id)
		if operation.IdempotencyKey != "" {
			delete(s.idempotency, idempotencyIndex(operation.ActorUserID, operation.IdempotencyKey))
		}
	case KindAudit:
		if _, ok := s.audit[id]; !ok {
			return false
		}
		delete(s.audit, id)
	case KindIntegration:
		if _, ok := s.integrations[id]; !ok {
			return false
		}
		delete(s.integrations, id)
	case KindIntegrationSecret:
		if _, ok := s.integrationSecrets[id]; !ok {
			return false
		}
		delete(s.integrationSecrets, id)
	case KindIntegrationUser:
		if _, ok := s.integrationUsers[id]; !ok {
			return false
		}
		delete(s.integrationUsers, id)
	case KindProject:
		if _, ok := s.projects[id]; !ok {
			return false
		}
		delete(s.projects, id)
	case KindPreview:
		value, ok := s.previews[id]
		if !ok {
			return false
		}
		s.unindexPreviewLocked(value)
		delete(s.previews, id)
	case KindConversation:
		if _, ok := s.conversations[id]; !ok {
			return false
		}
		delete(s.conversations, id)
	default:
		return false
	}
	return true
}

func (s *Service) Close() error                   { return s.store.Close() }
func (s *Service) Ping(ctx context.Context) error { return s.store.Ping(ctx) }

func (s *Service) SetDefaultQuota(quota ResourceQuota) {
	s.mu.Lock()
	s.defaultQuota = quota
	s.mu.Unlock()
}

// SetDefaultRelayContext scopes sessions created by callers that do not know
// the deployment's application/pool identifiers. Explicit caller context is
// preserved; partial context still fails validation instead of being guessed.
func (s *Service) SetDefaultRelayContext(applicationID, poolID string) error {
	applicationID = strings.TrimSpace(applicationID)
	poolID = strings.TrimSpace(poolID)
	if (applicationID == "") != (poolID == "") || applicationID != "" && (!validID(applicationID) || !validID(poolID)) {
		return ErrInvalid
	}
	s.mu.Lock()
	s.defaultApplication = applicationID
	s.defaultRelayPool = poolID
	s.mu.Unlock()
	return nil
}

// MigrateDefaultRelaySessions upgrades durable sessions created before pool
// scoping became the default. The normal immutable-context rule remains in
// force after this one-way migration.
func (s *Service) MigrateDefaultRelaySessions(ctx context.Context, meta MutationMeta) (int, error) {
	s.mu.RLock()
	enabled := s.defaultApplication != "" && s.defaultRelayPool != ""
	s.mu.RUnlock()
	if !enabled {
		return 0, nil
	}
	migrated := 0
	var results []error
	for _, candidate := range s.Sessions() {
		if candidate.TransportMode == "lan" || candidate.Status == "closing" || candidate.Status == "closed" || !sessionRelayContextEmpty(candidate) {
			continue
		}
		updated, err := s.putSession(ctx, candidate, candidate.Version, meta, false)
		if err != nil {
			if !errors.Is(err, ErrConflict) {
				results = append(results, fmt.Errorf("migrate Relay session %s: %w", candidate.ID, err))
			}
			continue
		}
		if updated.ApplicationID != "" {
			migrated++
		}
	}
	return migrated, errors.Join(results...)
}

// RebindManagedChatSessionToDefaultRelayContext repairs an Integration-owned
// Docker chat session after a deployment profile changes its application/pool
// identifiers. Session routing context is otherwise immutable, so this narrow
// recovery path deliberately excludes active sessions, Host OS sessions, and
// sessions that are not owned by a live Integration conversation.
func (s *Service) RebindManagedChatSessionToDefaultRelayContext(ctx context.Context, sessionID string, meta MutationMeta) (Session, bool, error) {
	if !validID(sessionID) {
		return Session{}, false, ErrInvalid
	}
	unlock := s.lock(KindSession, sessionID)
	defer unlock()

	s.mu.RLock()
	before, exists := s.sessions[sessionID]
	user, userOK := s.users[before.OwnerUserID]
	defaultApplication, defaultRelayPool := s.defaultApplication, s.defaultRelayPool
	managed := false
	for _, conversation := range s.conversations {
		if conversation.SessionID == before.ID && conversation.OwnerUserID == before.OwnerUserID &&
			conversation.DeviceID == before.DeviceID && conversation.ProjectID == before.ProjectID &&
			conversation.Status != "closed" && conversation.Status != "deleted" {
			managed = true
			break
		}
	}
	s.mu.RUnlock()

	if !exists {
		return Session{}, false, ErrNotFound
	}
	if defaultApplication == "" || defaultRelayPool == "" {
		return before, false, ErrNoRelayCapacity
	}
	if before.ApplicationID == defaultApplication && before.PoolID == defaultRelayPool {
		return before, false, nil
	}
	if !userOK || !managed || before.ExecutionTarget != "docker" || before.TransportMode != "relay" ||
		(before.AgentMode != "chat" && before.AgentMode != "acp") || before.Status == "active" ||
		before.Status == "closing" || before.Status == "closed" {
		return before, false, ErrForbidden
	}
	if before.RelayNodeID != "" {
		if node, ok := s.Node(before.RelayNodeID); ok && relayNodeCompatible(node, before.ApplicationID, before.PoolID, s.now()) {
			return before, false, nil
		}
	}
	// Never rewrite a session merely because the old pool is unavailable. A
	// healthy node in the newly configured default pool must already exist.
	if _, ok := s.SelectRelayNode(defaultApplication, defaultRelayPool); !ok {
		return before, false, ErrNoRelayCapacity
	}

	updated := before
	updated.ApplicationID = defaultApplication
	updated.PoolID = defaultRelayPool
	updated.TenantID = user.OrganizationID
	if updated.TenantID == "" {
		updated.TenantID = user.ID
	}
	updated.ResourceType = "device"
	updated.ResourceID = updated.DeviceID
	updated.Protocol = "terminal"
	if updated.AgentMode == "acp" {
		updated.Protocol = "acp"
	}
	updated.RelayNodeID = ""
	updated.HostConnectionID = ""
	updated.SelectedTransport = ""
	updated.DriverUserID = ""
	updated.DriverLeaseExpiresAt = nil
	updated.RelayGeneration++
	if updated.RelayGeneration < 1 {
		updated.RelayGeneration = 1
	}
	updated.LastError = ""
	updated.UpdatedAt = s.now().UTC()
	updated.Version = before.Version + 1
	if err := s.persist(ctx, KindSession, updated.ID, before.Version, updated); err != nil {
		return Session{}, false, err
	}
	s.mu.Lock()
	s.sessions[updated.ID] = updated
	s.emitLocked(KindSession, "updated", updated.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "session.relay_context_rebound", KindSession, updated.ID, before, updated, true, "")
	return updated, true, nil
}

func (s *Service) loadRecord(record Record) error {
	decode := func(dst any) error {
		if err := json.Unmarshal(record.Data, dst); err != nil {
			return err
		}
		return nil
	}
	switch record.Kind {
	case KindUser:
		var value User
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.users[value.ID] = value
	case KindNode:
		var value Node
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.nodes[value.ID] = value
	case KindDevice:
		var value Device
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.devices[value.ID] = value
	case KindRuntime:
		var value RuntimeInstance
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.runtimes[value.ID] = value
	case KindSession:
		var value Session
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.sessions[value.ID] = value
	case KindParticipant:
		var value Participant
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.participants[value.ID] = value
	case KindGrant:
		var value AccessGrant
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.grants[value.ID] = value
	case KindOperation:
		var value Operation
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.operations[value.ID] = value
		if value.IdempotencyKey != "" {
			s.idempotency[idempotencyIndex(value.ActorUserID, value.IdempotencyKey)] = value.ID
		}
	case KindAudit:
		var value AuditEvent
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.audit[value.ID] = value
	case KindIntegration:
		var value Integration
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		normalizeIntegration(&value)
		s.integrations[value.ID] = value
	case KindIntegrationSecret:
		var value IntegrationSecret
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.integrationSecrets[value.ID] = value
	case KindIntegrationUser:
		var value IntegrationUser
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.integrationUsers[value.ID] = value
	case KindProject:
		var value Project
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.projects[value.ID] = value
	case KindPreview:
		var value Preview
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		if previous, ok := s.previews[value.ID]; ok {
			s.unindexPreviewLocked(previous)
		}
		s.previews[value.ID] = value
		s.indexPreviewLocked(value)
	case KindConversation:
		var value Conversation
		if err := decode(&value); err != nil {
			return err
		}
		if value.ID != record.ID || value.Version != record.Version {
			return ErrInvalid
		}
		s.conversations[value.ID] = value
	default:
		return fmt.Errorf("unknown record kind %q", record.Kind)
	}
	return nil
}

func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := Snapshot{}
	for _, value := range s.users {
		out.Users = append(out.Users, value)
	}
	for _, value := range s.nodes {
		out.Nodes = append(out.Nodes, cloneNode(value))
	}
	for _, value := range s.devices {
		out.Devices = append(out.Devices, cloneDevice(value))
	}
	for _, value := range s.runtimes {
		out.Runtimes = append(out.Runtimes, value)
	}
	for _, value := range s.sessions {
		out.Sessions = append(out.Sessions, value)
	}
	for _, value := range s.participants {
		out.Participants = append(out.Participants, value)
	}
	for _, value := range s.grants {
		out.Grants = append(out.Grants, cloneGrant(value))
	}
	for _, value := range s.operations {
		out.Operations = append(out.Operations, cloneOperation(value))
	}
	for _, value := range s.audit {
		out.Audit = append(out.Audit, cloneAudit(value))
	}
	for _, value := range s.integrations {
		out.Integrations = append(out.Integrations, value)
	}
	for _, value := range s.integrationUsers {
		out.IntegrationUsers = append(out.IntegrationUsers, value)
	}
	for _, value := range s.projects {
		out.Projects = append(out.Projects, value)
	}
	for _, value := range s.previews {
		out.Previews = append(out.Previews, value)
	}
	for _, value := range s.conversations {
		out.Conversations = append(out.Conversations, value)
	}
	sort.Slice(out.Users, func(i, j int) bool { return out.Users[i].ID < out.Users[j].ID })
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	sort.Slice(out.Devices, func(i, j int) bool { return out.Devices[i].ID < out.Devices[j].ID })
	sort.Slice(out.Runtimes, func(i, j int) bool { return out.Runtimes[i].ID < out.Runtimes[j].ID })
	sort.Slice(out.Sessions, func(i, j int) bool { return out.Sessions[i].CreatedAt.After(out.Sessions[j].CreatedAt) })
	sort.Slice(out.Participants, func(i, j int) bool { return out.Participants[i].ConnectedAt.After(out.Participants[j].ConnectedAt) })
	sort.Slice(out.Grants, func(i, j int) bool { return out.Grants[i].CreatedAt.After(out.Grants[j].CreatedAt) })
	sort.Slice(out.Operations, func(i, j int) bool { return out.Operations[i].CreatedAt.After(out.Operations[j].CreatedAt) })
	sort.Slice(out.Audit, func(i, j int) bool { return out.Audit[i].CreatedAt.After(out.Audit[j].CreatedAt) })
	sort.Slice(out.Integrations, func(i, j int) bool { return out.Integrations[i].ID < out.Integrations[j].ID })
	sort.Slice(out.IntegrationUsers, func(i, j int) bool { return out.IntegrationUsers[i].CreatedAt.After(out.IntegrationUsers[j].CreatedAt) })
	sort.Slice(out.Projects, func(i, j int) bool { return out.Projects[i].CreatedAt.After(out.Projects[j].CreatedAt) })
	sort.Slice(out.Previews, func(i, j int) bool { return out.Previews[i].CreatedAt.After(out.Previews[j].CreatedAt) })
	sort.Slice(out.Conversations, func(i, j int) bool { return out.Conversations[i].CreatedAt.After(out.Conversations[j].CreatedAt) })
	return out
}

// The collection accessors intentionally avoid Snapshot's full-copy and sort
// cost. Controllers call these methods on short intervals and only need one
// resource kind at a time.
func (s *Service) Devices() []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Device, 0, len(s.devices))
	for _, value := range s.devices {
		out = append(out, cloneDevice(value))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Service) Users() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, 0, len(s.users))
	for _, value := range s.users {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Service) Nodes() []Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Node, 0, len(s.nodes))
	for _, value := range s.nodes {
		out = append(out, cloneNode(value))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Service) Runtimes() []RuntimeInstance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RuntimeInstance, 0, len(s.runtimes))
	for _, value := range s.runtimes {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Service) Sessions() []Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Session, 0, len(s.sessions))
	for _, value := range s.sessions {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *Service) Participants() []Participant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Participant, 0, len(s.participants))
	for _, value := range s.participants {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectedAt.After(out[j].ConnectedAt) })
	return out
}

func (s *Service) Grants() []AccessGrant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AccessGrant, 0, len(s.grants))
	for _, value := range s.grants {
		out = append(out, cloneGrant(value))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *Service) Operations() []Operation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Operation, 0, len(s.operations))
	for _, value := range s.operations {
		out = append(out, cloneOperation(value))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *Service) QueuedOperations(limit int) []Operation {
	if limit < 1 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Operation, 0)
	for _, value := range s.operations {
		if value.Status != "queued" {
			continue
		}
		out = append(out, cloneOperation(value))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (s *Service) PruneTerminalOperations(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	s.mu.RLock()
	candidates := make([]Operation, 0)
	for _, value := range s.operations {
		if terminalOperation(value.Status) && value.UpdatedAt.Before(before) {
			candidates = append(candidates, value)
		}
	}
	s.mu.RUnlock()
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].UpdatedAt.Before(candidates[j].UpdatedAt) })
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	removed := 0
	var errs []error
	for _, candidate := range candidates {
		unlock := s.lock(KindOperation, candidate.ID)
		s.mu.RLock()
		current, exists := s.operations[candidate.ID]
		s.mu.RUnlock()
		if !exists || current.Version != candidate.Version || !terminalOperation(current.Status) || !current.UpdatedAt.Before(before) {
			unlock()
			continue
		}
		if err := s.store.Delete(ctx, KindOperation, current.ID, current.Version); err != nil {
			unlock()
			if !errors.Is(err, ErrConflict) {
				errs = append(errs, err)
			}
			continue
		}
		s.mu.Lock()
		delete(s.operations, current.ID)
		if current.IdempotencyKey != "" {
			delete(s.idempotency, idempotencyIndex(current.ActorUserID, current.IdempotencyKey))
		}
		s.emitLocked(KindOperation, "pruned", current.ID)
		s.mu.Unlock()
		unlock()
		removed++
	}
	return removed, errors.Join(errs...)
}

func (s *Service) AuditEvents() []AuditEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AuditEvent, 0, len(s.audit))
	for _, value := range s.audit {
		out = append(out, cloneAudit(value))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *Service) Overview() Overview {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o := Overview{
		Users: len(s.users), Participants: len(s.participants),
		Integrations: len(s.integrations), IntegrationUsers: len(s.integrationUsers), Projects: len(s.projects), Previews: len(s.previews),
		IntegrationUsersByState: map[string]int{}, ProjectsByState: map[string]int{}, PreviewsByState: map[string]int{}, ConversationsByState: map[string]int{},
		OperationsByState: map[string]int{}, DevicesByState: map[string]int{}, SessionsByState: map[string]int{},
		GeneratedAt: s.now().UTC(),
	}
	for _, device := range s.devices {
		o.DevicesByState[device.ObservedState]++
		if device.ObservedState == "online" {
			o.OnlineDevices++
		}
		if device.RelayRegistered {
			o.RelayConnections++
		}
	}
	for _, runtime := range s.runtimes {
		if runtime.ObservedState == "running" || runtime.ObservedState == "online" {
			o.RunningRuntimes++
		}
	}
	for _, session := range s.sessions {
		o.SessionsByState[session.Status]++
		if session.Status == "active" || session.Status == "ready" {
			o.ActiveSessions++
		}
	}
	for _, operation := range s.operations {
		o.OperationsByState[operation.Status]++
	}
	for _, integrationUser := range s.integrationUsers {
		o.IntegrationUsersByState[integrationUser.Status]++
	}
	for _, project := range s.projects {
		o.ProjectsByState[project.Status]++
	}
	for _, preview := range s.previews {
		o.PreviewsByState[preview.Status]++
	}
	for _, conversation := range s.conversations {
		o.ConversationsByState[conversation.Status]++
		if conversation.Status != "closed" && conversation.Status != "deleted" {
			o.ActiveConversations++
		}
	}
	return o
}

func (s *Service) User(id string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.users[id]
	return value, ok
}

func (s *Service) Node(id string) (Node, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.nodes[id]
	return cloneNode(value), ok
}

func (s *Service) Device(id string) (Device, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.devices[id]
	return cloneDevice(value), ok
}

func (s *Service) Runtime(id string) (RuntimeInstance, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.runtimes[id]
	return value, ok
}

func (s *Service) Session(id string) (Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.sessions[id]
	return value, ok
}

func (s *Service) Participant(id string) (Participant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.participants[id]
	return value, ok
}

func (s *Service) Grant(id string) (AccessGrant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.grants[id]
	return cloneGrant(value), ok
}

func (s *Service) Operation(id string) (Operation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.operations[id]
	return cloneOperation(value), ok
}

func (s *Service) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer < 1 {
		buffer = 1
	}
	ch := make(chan Event, buffer)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() { once.Do(func() { s.mu.Lock(); delete(s.subscribers, ch); close(ch); s.mu.Unlock() }) }
}

func (s *Service) emitLocked(kind, action, id string) {
	event := Event{Sequence: s.sequence.Add(1), Kind: kind, Action: action, ID: id, At: s.now().UTC()}
	for ch := range s.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}

func (s *Service) lock(kind, id string) func() {
	value, _ := s.locks.LoadOrStore(kind+"\x00"+id, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// WithRecordOperation serializes a multi-step record transition both within
// this process and, when the store supports it, across Manager replicas. The
// refresh after acquiring the distributed lock ensures the callback starts
// from the durable version selected by the lock winner.
func (s *Service) WithRecordOperation(ctx context.Context, kind, id string, fn func() error) error {
	if !validRecordPart(kind) || !validRecordPart(id) || fn == nil {
		return ErrInvalid
	}
	unlock := s.lock("record-operation:"+kind, id)
	defer unlock()
	run := func() error {
		if err := s.Refresh(ctx); err != nil {
			return err
		}
		return fn()
	}
	if distributed, ok := s.store.(DistributedLocker); ok {
		return distributed.WithLock(ctx, "record-operation:"+kind+":"+id, run)
	}
	return run()
}

// WithLocalRecordOperation shares the same in-process operation lock without
// holding a database advisory lock. Reconcilers use it for frequent,
// idempotent observations so API transitions stay serialized locally without
// keeping a PostgreSQL transaction open during runtime health checks.
func (s *Service) WithLocalRecordOperation(ctx context.Context, kind, id string, fn func() error) error {
	if !validRecordPart(kind) || !validRecordPart(id) || fn == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock := s.lock("record-operation:"+kind, id)
	defer unlock()
	return fn()
}

func (s *Service) PutUser(ctx context.Context, user User, expected int64, meta MutationMeta) (User, error) {
	if !validUserID(user.ID) || !validResourceQuota(user.Quota) {
		return User{}, ErrInvalid
	}
	unlock := s.lock(KindUser, user.ID)
	defer unlock()
	s.mu.RLock()
	before, exists := s.users[user.ID]
	s.mu.RUnlock()
	if exists && before.Version != expected || !exists && expected != 0 {
		return User{}, ErrConflict
	}
	now := s.now().UTC()
	if !exists && user.Quota == (ResourceQuota{}) {
		s.mu.RLock()
		user.Quota = s.defaultQuota
		s.mu.RUnlock()
	}
	if user.Status == "" {
		user.Status = "active"
	}
	if user.Status != "active" && user.Status != "suspended" && user.Status != "deleted" {
		return User{}, ErrInvalid
	}
	if user.ExternalSubject == "" {
		user.ExternalSubject = user.ID
	}
	if exists {
		user.CreatedAt = before.CreatedAt
	} else if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt, user.Version = now, expected+1
	if err := s.persist(ctx, KindUser, user.ID, expected, user); err != nil {
		return User{}, err
	}
	s.mu.Lock()
	s.users[user.ID] = user
	s.emitLocked(KindUser, mutationAction(exists), user.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "user."+mutationAction(exists), KindUser, user.ID, before, user, true, "")
	return user, nil
}

func (s *Service) PutNode(ctx context.Context, node Node, expected int64, meta MutationMeta) (Node, error) {
	if !validID(node.ID) || (node.Kind != "docker" && node.Kind != "relay") {
		return Node{}, ErrInvalid
	}
	if node.PoolID != "" && !validID(node.PoolID) || node.Region != "" && !validID(node.Region) || node.Capacity.MaxConnections < 0 {
		return Node{}, ErrInvalid
	}
	for _, applicationID := range node.AllowedApplications {
		if !validID(applicationID) {
			return Node{}, ErrInvalid
		}
	}
	unlock := s.lock(KindNode, node.ID)
	defer unlock()
	s.mu.RLock()
	before, exists := s.nodes[node.ID]
	s.mu.RUnlock()
	if exists && before.Version != expected || !exists && expected != 0 {
		return Node{}, ErrConflict
	}
	now := s.now().UTC()
	if node.Status == "" {
		node.Status = "ready"
	}
	if exists {
		node.CreatedAt = before.CreatedAt
	} else {
		node.CreatedAt = now
	}
	node.UpdatedAt, node.Version = now, expected+1
	node.Labels = cloneStrings(node.Labels)
	node.AllowedApplications = uniqueSortedStrings(node.AllowedApplications)
	if err := s.persist(ctx, KindNode, node.ID, expected, node); err != nil {
		return Node{}, err
	}
	s.mu.Lock()
	s.nodes[node.ID] = node
	s.emitLocked(KindNode, mutationAction(exists), node.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "node."+mutationAction(exists), KindNode, node.ID, before, node, true, "")
	return cloneNode(node), nil
}

// EnsureSessionRelayNode pins a versioned session to one healthy node. Host
// and Participant credentials subsequently carry the same relay_node claim,
// so a generic load balancer cannot split the two websocket legs.
func (s *Service) EnsureSessionRelayNode(ctx context.Context, sessionID string, meta MutationMeta) (Session, Node, error) {
	if !validID(sessionID) {
		return Session{}, Node{}, ErrInvalid
	}
	initial, ok := s.Session(sessionID)
	if !ok {
		return Session{}, Node{}, ErrNotFound
	}
	assignmentKey := initial.PoolID
	if assignmentKey == "" {
		assignmentKey = sessionID
	}
	unlock := s.lock("relay-assignment", assignmentKey)
	defer unlock()
	assign := func() (Session, Node, error) {
		return s.ensureSessionRelayNode(ctx, sessionID, meta)
	}
	if distributed, ok := s.store.(DistributedLocker); ok {
		var session Session
		var node Node
		var assignErr error
		err := distributed.WithLock(ctx, "relay-assignment:"+assignmentKey, func() error {
			if err := s.Refresh(ctx); err != nil {
				return err
			}
			session, node, assignErr = assign()
			return assignErr
		})
		if err != nil {
			return Session{}, Node{}, err
		}
		return session, node, nil
	}
	return assign()
}

func (s *Service) ensureSessionRelayNode(ctx context.Context, sessionID string, meta MutationMeta) (Session, Node, error) {
	session, ok := s.Session(sessionID)
	if !ok {
		return Session{}, Node{}, ErrNotFound
	}
	if session.ApplicationID == "" && session.PoolID == "" {
		return session, Node{}, nil
	}
	if session.ApplicationID == "" || session.PoolID == "" || session.TenantID == "" || session.ResourceType == "" || session.ResourceID == "" {
		return Session{}, Node{}, ErrInvalid
	}
	if session.RelayNodeID != "" {
		node, exists := s.Node(session.RelayNodeID)
		if exists && relayNodeCompatible(node, session.ApplicationID, session.PoolID, s.now()) {
			return session, node, nil
		}
		if session.Status == "active" {
			return Session{}, Node{}, ErrNoRelayCapacity
		}
		session.RelayNodeID = ""
	}
	node, ok := s.SelectRelayNode(session.ApplicationID, session.PoolID)
	if !ok {
		return Session{}, Node{}, ErrNoRelayCapacity
	}
	session.RelayNodeID = node.ID
	updated, err := s.PutSession(ctx, session, session.Version, meta)
	if err != nil {
		return Session{}, Node{}, err
	}
	return updated, node, nil
}

// SelectRelayNode uses least-connections scheduling with a stable ID tie
// breaker. A configured connection ceiling is honored before ranking.
func (s *Service) SelectRelayNode(applicationID, poolID string) (Node, bool) {
	if !validID(applicationID) || !validID(poolID) {
		return Node{}, false
	}
	now := s.now()
	assigned := map[string]int{}
	for _, session := range s.Sessions() {
		if session.RelayNodeID != "" && session.Status != "closed" && session.Status != "error" {
			assigned[session.RelayNodeID]++
		}
	}
	type candidate struct {
		node Node
		load int
	}
	candidates := make([]candidate, 0)
	for _, node := range s.Nodes() {
		if !relayNodeCompatible(node, applicationID, poolID, now) {
			continue
		}
		load := max(node.Usage.Connections, assigned[node.ID])
		if node.Capacity.MaxConnections > 0 && load >= node.Capacity.MaxConnections {
			continue
		}
		candidates = append(candidates, candidate{node: node, load: load})
	}
	if len(candidates) == 0 {
		return Node{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].load == candidates[j].load {
			return candidates[i].node.ID < candidates[j].node.ID
		}
		return candidates[i].load < candidates[j].load
	})
	return candidates[0].node, true
}

func relayNodeCompatible(node Node, applicationID, poolID string, now time.Time) bool {
	return relayNodeCompatibleWithin(node, applicationID, poolID, now, 90*time.Second)
}

func relayNodeCompatibleWithin(node Node, applicationID, poolID string, now time.Time, staleAfter time.Duration) bool {
	if node.Kind != "relay" || node.Status != "ready" || node.Address == "" || node.PoolID != poolID {
		return false
	}
	if node.LastHeartbeat.IsZero() || node.LastHeartbeat.Before(now.Add(-staleAfter)) {
		return false
	}
	for _, allowed := range node.AllowedApplications {
		if allowed == applicationID {
			return true
		}
	}
	return false
}

func (s *Service) RegisterDevice(ctx context.Context, device Device, meta MutationMeta) (Device, error) {
	if !validID(device.ID) || !validID(device.OwnerUserID) || (device.Kind != "local" && device.Kind != "docker") || !validDeviceMetadata(device.Metadata) {
		return Device{}, ErrInvalid
	}
	unlock := s.lock(KindDevice, device.ID)
	defer unlock()
	s.mu.RLock()
	user, userOK := s.users[device.OwnerUserID]
	before, exists := s.devices[device.ID]
	s.mu.RUnlock()
	if !userOK || user.Status != "active" {
		return Device{}, ErrForbidden
	}
	if exists && before.OwnerUserID != device.OwnerUserID {
		return Device{}, ErrForbidden
	}
	now := s.now().UTC()
	expected := int64(0)
	if exists {
		expected = before.Version
		device.CreatedAt = before.CreatedAt
		if device.RuntimeID == "" {
			device.RuntimeID = before.RuntimeID
		}
	} else {
		device.CreatedAt = now
	}
	if device.Name == "" {
		device.Name = device.ID
	}
	if device.DesiredState == "" {
		device.DesiredState = "running"
	}
	if device.ObservedState == "" {
		device.ObservedState = "starting"
	}
	device.UpdatedAt, device.Version = now, expected+1
	device.Metadata = cloneStrings(device.Metadata)
	if err := s.persist(ctx, KindDevice, device.ID, expected, device); err != nil {
		return Device{}, err
	}
	s.mu.Lock()
	s.devices[device.ID] = device
	s.emitLocked(KindDevice, mutationAction(exists), device.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "device."+mutationAction(exists), KindDevice, device.ID, before, device, true, "")
	return cloneDevice(device), nil
}

func (s *Service) HeartbeatDevice(ctx context.Context, id, owner string, heartbeat DeviceHeartbeat, meta MutationMeta) (Device, error) {
	if !validID(id) || !validDeviceMetadata(heartbeat.Metadata) {
		return Device{}, ErrInvalid
	}
	unlock := s.lock(KindDevice, id)
	defer unlock()
	s.mu.RLock()
	before, exists := s.devices[id]
	s.mu.RUnlock()
	if !exists {
		return Device{}, ErrNotFound
	}
	if owner != "" && before.OwnerUserID != owner {
		return Device{}, ErrForbidden
	}
	if heartbeat.ObservedState == "" {
		heartbeat.ObservedState = deriveDeviceState(heartbeat)
	}
	if !validObservedState(heartbeat.ObservedState) || heartbeat.ActiveSessions < 0 {
		return Device{}, ErrInvalid
	}
	next := before
	next.ObservedState = heartbeat.ObservedState
	next.RuntimeRunning = heartbeat.RuntimeRunning
	next.RuntimeHealthy = heartbeat.RuntimeHealthy
	next.ClientConnected = heartbeat.ClientConnected
	next.RelayRegistered = heartbeat.RelayRegistered
	next.RelayNodeID = heartbeat.RelayNodeID
	next.ActiveSessions = heartbeat.ActiveSessions
	if heartbeat.Metadata != nil {
		next.Metadata = cloneStrings(heartbeat.Metadata)
	}
	now := s.now().UTC()
	next.LastHeartbeat, next.UpdatedAt, next.Version = now, now, before.Version+1
	if err := s.persist(ctx, KindDevice, id, before.Version, next); err != nil {
		return Device{}, err
	}
	s.mu.Lock()
	s.devices[id] = next
	s.emitLocked(KindDevice, "heartbeat", id)
	s.mu.Unlock()
	return cloneDevice(next), nil
}

func (s *Service) PutRuntime(ctx context.Context, runtime RuntimeInstance, expected int64, meta MutationMeta) (RuntimeInstance, error) {
	if !validID(runtime.ID) || !validID(runtime.DeviceID) || !validID(runtime.OwnerUserID) || !validID(runtime.NodeID) || !validResourceQuota(runtime.Quota) {
		return RuntimeInstance{}, ErrInvalid
	}
	unlock := s.lock(KindRuntime, runtime.ID)
	defer unlock()
	s.mu.RLock()
	before, exists := s.runtimes[runtime.ID]
	device, deviceOK := s.devices[runtime.DeviceID]
	_, nodeOK := s.nodes[runtime.NodeID]
	s.mu.RUnlock()
	if !deviceOK || !nodeOK || device.OwnerUserID != runtime.OwnerUserID {
		return RuntimeInstance{}, ErrForbidden
	}
	if exists && before.Version != expected || !exists && expected != 0 {
		return RuntimeInstance{}, ErrConflict
	}
	now := s.now().UTC()
	if runtime.DesiredState == "" {
		runtime.DesiredState = "running"
	}
	if runtime.ObservedState == "" {
		runtime.ObservedState = "provisioning"
	}
	if exists {
		runtime.CreatedAt = before.CreatedAt
	} else {
		runtime.CreatedAt = now
	}
	runtime.UpdatedAt, runtime.Version = now, expected+1
	if err := s.persist(ctx, KindRuntime, runtime.ID, expected, runtime); err != nil {
		return RuntimeInstance{}, err
	}
	s.mu.Lock()
	s.runtimes[runtime.ID] = runtime
	s.emitLocked(KindRuntime, mutationAction(exists), runtime.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "runtime."+mutationAction(exists), KindRuntime, runtime.ID, before, runtime, true, "")
	return runtime, nil
}

func (s *Service) PutSession(ctx context.Context, session Session, expected int64, meta MutationMeta) (Session, error) {
	return s.putSession(ctx, session, expected, meta, false)
}

func (s *Service) putSession(ctx context.Context, session Session, expected int64, meta MutationMeta, allowRelayGenerationChange bool) (Session, error) {
	if session.ID == "" && expected == 0 {
		session.ID = randomID("session")
	}
	if !validID(session.ID) || !validID(session.OwnerUserID) || !validID(session.DeviceID) {
		return Session{}, ErrInvalid
	}
	unlock := s.lock(KindSession, session.ID)
	defer unlock()
	s.mu.RLock()
	before, exists := s.sessions[session.ID]
	device, deviceOK := s.devices[session.DeviceID]
	user, userOK := s.users[session.OwnerUserID]
	defaultApplication, defaultRelayPool := s.defaultApplication, s.defaultRelayPool
	s.mu.RUnlock()
	if !deviceOK || !userOK || device.OwnerUserID != session.OwnerUserID || device.DesiredState == "revoked" {
		return Session{}, ErrForbidden
	}
	if session.AgentMode == "" {
		session.AgentMode = "terminal"
	}
	legacyContextMigration := exists && session.TransportMode != "lan" && sessionRelayContextEmpty(before) && sessionRelayContextEmpty(session) && defaultApplication != ""
	if (!exists && session.TransportMode != "lan" && sessionRelayContextEmpty(session) || legacyContextMigration) && defaultApplication != "" {
		session.ApplicationID = defaultApplication
		session.PoolID = defaultRelayPool
		session.TenantID = user.OrganizationID
		if session.TenantID == "" {
			session.TenantID = user.ID
		}
		session.ResourceType = "device"
		session.ResourceID = session.DeviceID
		if session.AgentMode == "acp" {
			session.Protocol = "acp"
		} else {
			session.Protocol = "terminal"
		}
		if legacyContextMigration {
			before.ApplicationID = session.ApplicationID
			before.PoolID = session.PoolID
			before.TenantID = session.TenantID
			before.ResourceType = session.ResourceType
			before.ResourceID = session.ResourceID
			before.Protocol = session.Protocol
		}
	}
	if !validSessionOptions(session) {
		return Session{}, ErrInvalid
	}
	// A session executes where its registered device says it executes.  Do not
	// accept a client-provided label that could make a Host OS agent look like a
	// Docker executor (or vice versa); Desktop uses this value as the source of
	// truth for routing and for the execution-environment badge.
	if device.Kind != session.ExecutionTarget {
		return Session{}, ErrInvalid
	}
	if session.ProjectID != "" {
		project, projectOK := s.Project(session.ProjectID)
		if !projectOK || project.Status != "ready" || project.OwnerUserID != session.OwnerUserID || project.WorkingDir != ProjectWorkingDir(project.ID) || session.ExecutionTarget != "docker" || (session.AgentMode != "chat" && session.AgentMode != "acp") {
			return Session{}, ErrForbidden
		}
	}
	if exists && before.Version != expected || !exists && expected != 0 {
		return Session{}, ErrConflict
	}
	if exists && (before.ProjectID != session.ProjectID || before.ApplicationID != session.ApplicationID ||
		before.PoolID != session.PoolID || before.TenantID != session.TenantID ||
		before.ResourceType != session.ResourceType || before.ResourceID != session.ResourceID ||
		before.AgentID != session.AgentID || before.Protocol != session.Protocol) {
		return Session{}, ErrForbidden
	}
	// Initialize records created before fencing epochs were introduced. This is
	// a one-way server migration; later caller attempts to change the epoch are
	// still rejected below.
	if exists && before.RelayGeneration == 0 && session.RelayGeneration == 0 && session.ApplicationID != "" {
		session.RelayGeneration = 1
		before.RelayGeneration = 1
	}
	if exists && before.RelayGeneration != session.RelayGeneration && !allowRelayGenerationChange {
		return Session{}, ErrForbidden
	}
	if !exists {
		session.RelayGeneration = 1
		active := s.userActiveSessionCount(session.OwnerUserID)
		s.mu.RLock()
		user := s.users[session.OwnerUserID]
		s.mu.RUnlock()
		if user.Quota.MaxSessions > 0 && active >= user.Quota.MaxSessions {
			return Session{}, ErrQuota
		}
	}
	now := s.now().UTC()
	if session.Status == "" {
		session.Status = "creating"
	}
	if exists {
		session.CreatedAt = before.CreatedAt
	} else {
		session.CreatedAt = now
	}
	session.UpdatedAt, session.Version = now, expected+1
	if err := s.persist(ctx, KindSession, session.ID, expected, session); err != nil {
		return Session{}, err
	}
	s.mu.Lock()
	s.sessions[session.ID] = session
	s.emitLocked(KindSession, mutationAction(exists), session.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "session."+mutationAction(exists), KindSession, session.ID, before, session, true, "")
	return session, nil
}

// RecoverStaleRelaySessions fences sessions whose Relay node lease expired.
// Incrementing RelayGeneration changes the verified routing key before a new
// node is selected, so stale host/participant credentials stay isolated.
func (s *Service) RecoverStaleRelaySessions(ctx context.Context, staleAfter time.Duration, meta MutationMeta) (int, error) {
	if staleAfter <= 0 {
		staleAfter = 90 * time.Second
	}
	now := s.now()
	recovered := 0
	var results []error
	for _, candidate := range s.Sessions() {
		if candidate.RelayNodeID == "" || candidate.ApplicationID == "" || candidate.PoolID == "" ||
			candidate.Status == "closing" || candidate.Status == "closed" || candidate.Status == "error" {
			continue
		}
		node, exists := s.Node(candidate.RelayNodeID)
		if exists && relayNodeCompatibleWithin(node, candidate.ApplicationID, candidate.PoolID, now, staleAfter) {
			continue
		}
		current, ok := s.Session(candidate.ID)
		if !ok || current.RelayNodeID != candidate.RelayNodeID {
			continue
		}
		current.Status = "reconnecting"
		current.RelayNodeID = ""
		current.HostConnectionID = ""
		current.SelectedTransport = ""
		current.DriverUserID = ""
		current.DriverLeaseExpiresAt = nil
		current.RelayGeneration++
		if current.RelayGeneration < 1 {
			current.RelayGeneration = 1
		}
		current.LastError = "assigned Relay node lease expired; reconnecting on a healthy node"
		if _, err := s.putSession(ctx, current, current.Version, meta, true); err != nil {
			if !errors.Is(err, ErrConflict) {
				results = append(results, fmt.Errorf("recover Relay session %s: %w", current.ID, err))
			}
			continue
		}
		for _, participant := range s.Participants() {
			if participant.SessionID != current.ID {
				continue
			}
			if err := s.DeleteParticipant(ctx, participant.ID, MutationMeta{ActorUserID: meta.ActorUserID, RequestID: meta.RequestID, SourceIP: meta.SourceIP, SkipAudit: true, Trusted: meta.Trusted}); err != nil && !errors.Is(err, ErrNotFound) {
				results = append(results, fmt.Errorf("remove stale participant %s: %w", participant.ID, err))
			}
		}
		recovered++
	}
	return recovered, errors.Join(results...)
}

func (s *Service) CanAccessSession(userID, sessionID, access string, now time.Time) (Session, bool) {
	session, ok := s.Session(sessionID)
	if !ok {
		return Session{}, false
	}
	if session.OwnerUserID == userID {
		return session, true
	}
	// Managed Docker workspaces are strictly owner-only. Sharing grants and
	// operator credentials apply only to explicitly shared Host OS sessions.
	if session.ExecutionTarget == "docker" {
		return session, false
	}
	return session, s.hasGrant(userID, session.DeviceID, session.ID, access, now)
}

func (s *Service) PutParticipant(ctx context.Context, participant Participant, meta MutationMeta) (Participant, error) {
	if participant.ID == "" {
		participant.ID = participant.SessionID + ":" + participant.ConnectionID
	}
	if !validID(participant.ID) || !validID(participant.SessionID) || !validID(participant.UserID) || !validID(participant.ConnectionID) {
		return Participant{}, ErrInvalid
	}
	if participant.Role != "host" && participant.Role != "participant" || participant.Access != "view" && participant.Access != "control" {
		return Participant{}, ErrInvalid
	}
	unlock := s.lock(KindParticipant, participant.ID)
	defer unlock()
	s.mu.RLock()
	session, sessionOK := s.sessions[participant.SessionID]
	before, exists := s.participants[participant.ID]
	s.mu.RUnlock()
	if !sessionOK {
		return Participant{}, ErrNotFound
	}
	trustedSharedPresence := meta.Trusted && session.AccessMode == "shared"
	if participant.UserID != session.OwnerUserID && !trustedSharedPresence && !s.hasGrant(participant.UserID, session.DeviceID, session.ID, participant.Access, s.now()) {
		return Participant{}, ErrForbidden
	}
	if !exists {
		count := s.sessionParticipantCount(session.ID)
		s.mu.RLock()
		owner := s.users[session.OwnerUserID]
		s.mu.RUnlock()
		if owner.Quota.MaxParticipants > 0 && count >= owner.Quota.MaxParticipants {
			return Participant{}, ErrQuota
		}
	}
	now := s.now().UTC()
	expected := int64(0)
	if exists {
		expected, participant.ConnectedAt = before.Version, before.ConnectedAt
	} else if participant.ConnectedAt.IsZero() {
		participant.ConnectedAt = now
	}
	participant.LastSeenAt, participant.Version = now, expected+1
	if err := s.persist(ctx, KindParticipant, participant.ID, expected, participant); err != nil {
		return Participant{}, err
	}
	s.mu.Lock()
	s.participants[participant.ID] = participant
	s.emitLocked(KindParticipant, mutationAction(exists), participant.ID)
	s.mu.Unlock()
	return participant, nil
}

func (s *Service) DeleteParticipant(ctx context.Context, id string, meta MutationMeta) error {
	unlock := s.lock(KindParticipant, id)
	defer unlock()
	s.mu.RLock()
	before, exists := s.participants[id]
	s.mu.RUnlock()
	if !exists {
		return ErrNotFound
	}
	if err := s.store.Delete(ctx, KindParticipant, id, before.Version); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.participants, id)
	s.emitLocked(KindParticipant, "deleted", id)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "participant.deleted", KindParticipant, id, before, nil, true, "")
	return nil
}

func (s *Service) ApplyRelayPresence(ctx context.Context, presence RelayPresence, meta MutationMeta) (resultErr error) {
	if presence.EventID == "" || presence.ConnectionID == "" || (presence.Kind != "host" && presence.Kind != "participant" && presence.Kind != "node") {
		return ErrInvalid
	}
	unlock := s.lock("presence", presence.EventID)
	defer unlock()
	s.mu.RLock()
	_, duplicate := s.presenceSeen[presence.EventID]
	s.mu.RUnlock()
	if duplicate {
		return nil
	}
	defer func() {
		if resultErr != nil {
			return
		}
		s.mu.Lock()
		s.presenceSeen[presence.EventID] = struct{}{}
		s.presenceOrder = append(s.presenceOrder, presence.EventID)
		if len(s.presenceOrder) > 10000 {
			for _, id := range s.presenceOrder[:2000] {
				delete(s.presenceSeen, id)
			}
			s.presenceOrder = append([]string(nil), s.presenceOrder[2000:]...)
		}
		s.mu.Unlock()
	}()
	if presence.Kind == "node" {
		err := s.updateRelayNode(ctx, presence.NodeID, presence.PublicURL, presence.ControlURL, presence.PoolID, presence.ApplicationID, presence.Connected, !presence.Heartbeat)
		if errors.Is(err, ErrConflict) {
			return nil
		}
		return err
	}
	if presence.DeviceID == "" || presence.SessionID == "" {
		// Legacy room-only connections remain supported by Relay but cannot be
		// projected into the device/session Control Plane accurately.
		return nil
	}
	scopedSession, sessionOK := s.Session(presence.SessionID)
	if !sessionOK || scopedSession.ApplicationID != presence.ApplicationID || scopedSession.PoolID != presence.PoolID ||
		scopedSession.RelayGeneration != presence.RelayGeneration ||
		scopedSession.ApplicationID != "" && scopedSession.RelayNodeID != "" && scopedSession.RelayNodeID != presence.NodeID {
		return ErrForbidden
	}
	if err := s.updateRelayNode(ctx, presence.NodeID, presence.PublicURL, presence.ControlURL, presence.PoolID, presence.ApplicationID, true, !presence.Heartbeat); err != nil && !errors.Is(err, ErrConflict) {
		return err
	}
	if presence.Kind == "host" {
		device, ok := s.Device(presence.DeviceID)
		if !ok {
			return ErrNotFound
		}
		session := scopedSession
		if session.DeviceID != device.ID || session.OwnerUserID != device.OwnerUserID || presence.UserID != session.OwnerUserID {
			return ErrForbidden
		}
		meaningfulChange := false
		if presence.HostOnline && session.Status != "closed" && session.Status != "closing" {
			meaningfulChange = session.Status != "active" || session.SelectedTransport != "relay" || session.RelayNodeID != presence.NodeID || session.HostConnectionID != presence.ConnectionID
			session.Status, session.SelectedTransport, session.RelayNodeID, session.LastActivityAt = "active", "relay", presence.NodeID, presence.At
			session.HostConnectionID = presence.ConnectionID
		} else if !presence.HostOnline && session.Status == "active" {
			meaningfulChange = true
			session.Status, session.LastActivityAt = "ready", presence.At
			if session.HostConnectionID == presence.ConnectionID {
				session.HostConnectionID = ""
			}
		}
		persistActivity := !presence.Heartbeat || meaningfulChange || s.now().Sub(session.UpdatedAt) >= time.Minute
		if persistActivity {
			sessionMeta := meta
			if presence.Heartbeat && !meaningfulChange {
				sessionMeta.SkipAudit = true
			}
			if _, err := s.PutSession(ctx, session, session.Version, sessionMeta); err != nil && !errors.Is(err, ErrConflict) {
				return err
			}
		}
		activeSessions := 0
		for _, value := range s.Sessions() {
			if value.DeviceID == device.ID && value.Status == "active" {
				activeSessions++
			}
		}
		heartbeat := DeviceHeartbeat{
			ObservedState: device.ObservedState, RuntimeRunning: device.RuntimeRunning, RuntimeHealthy: device.RuntimeHealthy,
			ClientConnected: presence.HostOnline, RelayRegistered: presence.HostOnline, RelayNodeID: presence.NodeID,
			ActiveSessions: activeSessions, Metadata: device.Metadata,
		}
		if presence.HostOnline {
			heartbeat.ObservedState = "online"
		} else if device.RuntimeRunning {
			heartbeat.ObservedState = "degraded"
		} else {
			heartbeat.ObservedState = "offline"
		}
		if presence.Heartbeat && !meaningfulChange && !device.LastHeartbeat.IsZero() && s.now().Sub(device.LastHeartbeat) < time.Minute {
			return nil
		}
		deviceMeta := meta
		if presence.Heartbeat {
			deviceMeta.SkipAudit = true
		}
		_, err := s.HeartbeatDevice(ctx, device.ID, device.OwnerUserID, heartbeat, deviceMeta)
		if err == nil {
			_ = s.updateRelayNode(ctx, presence.NodeID, presence.PublicURL, presence.ControlURL, presence.PoolID, presence.ApplicationID, true, !presence.Heartbeat || meaningfulChange)
		}
		return err
	}
	participantID := presence.SessionID + ":" + presence.ConnectionID
	if !presence.Connected {
		if _, ok := s.Participant(participantID); !ok {
			return nil
		}
		return s.DeleteParticipant(ctx, participantID, meta)
	}
	existing, exists := s.Participant(participantID)
	if presence.Heartbeat && exists && s.now().Sub(existing.LastSeenAt) < time.Minute {
		return nil
	}
	_, err := s.PutParticipant(ctx, Participant{
		ID: participantID, SessionID: presence.SessionID, UserID: presence.UserID, ConnectionID: presence.ConnectionID,
		Role: presence.Role, Access: presence.Access, Transport: "relay", ConnectedAt: presence.At,
	}, meta)
	if err == nil {
		_ = s.updateRelayNode(ctx, presence.NodeID, presence.PublicURL, presence.ControlURL, presence.PoolID, presence.ApplicationID, true, !presence.Heartbeat)
	}
	return err
}

func (s *Service) updateRelayNode(ctx context.Context, nodeID, publicAddress, controlAddress, poolID, applicationID string, online, force bool) error {
	if nodeID == "" || !validID(nodeID) {
		return ErrInvalid
	}
	node, exists := s.Node(nodeID)
	if exists && node.Kind != "relay" {
		return ErrForbidden
	}
	if exists && !force && node.LastHeartbeat.After(s.now().Add(-15*time.Second)) {
		return nil
	}
	if !exists {
		node = Node{ID: nodeID, Kind: "relay"}
	}
	if publicAddress != "" {
		node.Address = strings.TrimRight(publicAddress, "/")
	}
	if controlAddress != "" {
		node.ControlAddress = strings.TrimRight(controlAddress, "/")
	}
	if poolID != "" {
		node.PoolID = poolID
	}
	if applicationID != "" {
		node.AllowedApplications = uniqueSortedStrings(append(node.AllowedApplications, applicationID))
	}
	if online {
		node.Status = "ready"
	} else {
		node.Status = "offline"
	}
	node.LastHeartbeat = s.now().UTC()
	connections := 0
	for _, session := range s.Sessions() {
		if session.RelayNodeID == nodeID && session.Status == "active" {
			connections++
		}
	}
	for _, participant := range s.Participants() {
		if session, ok := s.Session(participant.SessionID); ok && session.RelayNodeID == nodeID {
			connections++
		}
	}
	node.Usage.Connections = connections
	_, err := s.PutNode(ctx, node, node.Version, MutationMeta{ActorUserID: "relay-presence", SkipAudit: true})
	return err
}

func (s *Service) PutGrant(ctx context.Context, grant AccessGrant, expected int64, meta MutationMeta) (AccessGrant, error) {
	if grant.ID == "" {
		grant.ID = randomID("grant")
	}
	if !validID(grant.ID) || !validID(grant.OwnerUserID) || !validID(grant.SubjectUserID) || !validID(grant.TargetDeviceID) {
		return AccessGrant{}, ErrInvalid
	}
	if grant.Access != "view" && grant.Access != "control" || grant.ExpiresAt.IsZero() || !grant.ExpiresAt.After(s.now()) {
		return AccessGrant{}, ErrInvalid
	}
	unlock := s.lock(KindGrant, grant.ID)
	defer unlock()
	s.mu.RLock()
	before, exists := s.grants[grant.ID]
	device, deviceOK := s.devices[grant.TargetDeviceID]
	subject, subjectOK := s.users[grant.SubjectUserID]
	s.mu.RUnlock()
	if !deviceOK || device.OwnerUserID != grant.OwnerUserID {
		return AccessGrant{}, ErrForbidden
	}
	if device.Kind == "docker" {
		return AccessGrant{}, ErrForbidden
	}
	if !subjectOK || subject.Status != "active" || grant.SubjectUserID == grant.OwnerUserID {
		return AccessGrant{}, ErrForbidden
	}
	if exists && before.Version != expected || !exists && expected != 0 {
		return AccessGrant{}, ErrConflict
	}
	now := s.now().UTC()
	if grant.Access == "view" {
		grant.Capabilities = []string{"terminal:view"}
	} else {
		grant.Capabilities = []string{"terminal:view", "terminal:control"}
	}
	if exists {
		grant.CreatedAt = before.CreatedAt
	} else {
		grant.CreatedAt = now
	}
	grant.UpdatedAt, grant.Version = now, expected+1
	if err := s.persist(ctx, KindGrant, grant.ID, expected, grant); err != nil {
		return AccessGrant{}, err
	}
	s.mu.Lock()
	s.grants[grant.ID] = grant
	s.emitLocked(KindGrant, mutationAction(exists), grant.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "grant."+mutationAction(exists), KindGrant, grant.ID, before, grant, true, "")
	return cloneGrant(grant), nil
}

func (s *Service) RevokeGrant(ctx context.Context, id string, expected int64, meta MutationMeta) (AccessGrant, error) {
	unlock := s.lock(KindGrant, id)
	defer unlock()
	s.mu.RLock()
	before, exists := s.grants[id]
	s.mu.RUnlock()
	if !exists {
		return AccessGrant{}, ErrNotFound
	}
	if before.Version != expected {
		return AccessGrant{}, ErrConflict
	}
	now := s.now().UTC()
	next := before
	next.RevokedAt, next.UpdatedAt, next.Version = &now, now, before.Version+1
	if err := s.persist(ctx, KindGrant, id, expected, next); err != nil {
		return AccessGrant{}, err
	}
	s.mu.Lock()
	s.grants[id] = next
	s.emitLocked(KindGrant, "revoked", id)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "grant.revoked", KindGrant, id, before, next, true, "")
	return cloneGrant(next), nil
}

func (s *Service) BeginOperation(ctx context.Context, operation Operation, meta MutationMeta) (Operation, bool, error) {
	if operation.IdempotencyKey != "" {
		s.mu.RLock()
		existingID := s.idempotency[idempotencyIndex(operation.ActorUserID, operation.IdempotencyKey)]
		existing, ok := s.operations[existingID]
		s.mu.RUnlock()
		if ok {
			if !sameOperationIntent(existing, operation) {
				return Operation{}, false, ErrConflict
			}
			return cloneOperation(existing), true, nil
		}
		operation.ID = idempotentOperationID(operation.ActorUserID, operation.IdempotencyKey)
	}
	if operation.ID == "" {
		operation.ID = randomID("op")
	}
	if !validID(operation.ID) || operation.Type == "" || !validRecordPart(operation.TargetKind) || !validID(operation.TargetID) {
		return Operation{}, false, ErrInvalid
	}
	unlock := s.lock(KindOperation, operation.ID)
	defer unlock()
	now := s.now().UTC()
	operation.Status, operation.Progress = "queued", 0
	operation.CreatedAt, operation.UpdatedAt, operation.Version = now, now, 1
	operation.Request = cloneAnyMap(operation.Request)
	if err := s.persist(ctx, KindOperation, operation.ID, 0, operation); err != nil {
		if !errors.Is(err, ErrConflict) || operation.IdempotencyKey == "" {
			return Operation{}, false, err
		}
		// A different Manager may have claimed the same deterministic ID after
		// our local snapshot was loaded. Refresh and return that exact intent as
		// the duplicate; a key reused for another intent remains a conflict.
		if refreshErr := s.Refresh(ctx); refreshErr != nil {
			return Operation{}, false, errors.Join(err, refreshErr)
		}
		s.mu.RLock()
		existing, ok := s.operations[operation.ID]
		s.mu.RUnlock()
		if !ok || !sameOperationIntent(existing, operation) {
			return Operation{}, false, ErrConflict
		}
		return cloneOperation(existing), true, nil
	}
	s.mu.Lock()
	s.operations[operation.ID] = operation
	if operation.IdempotencyKey != "" {
		s.idempotency[idempotencyIndex(operation.ActorUserID, operation.IdempotencyKey)] = operation.ID
	}
	s.emitLocked(KindOperation, "created", operation.ID)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "operation.created", KindOperation, operation.ID, nil, operation, true, "")
	return cloneOperation(operation), false, nil
}

func (s *Service) UpdateOperation(ctx context.Context, id, status string, progress int, result map[string]any, operationErr string, meta MutationMeta) (Operation, error) {
	if status != "running" && status != "succeeded" && status != "failed" && status != "canceled" {
		return Operation{}, ErrInvalid
	}
	if progress < 0 || progress > 100 {
		return Operation{}, ErrInvalid
	}
	unlock := s.lock(KindOperation, id)
	defer unlock()
	s.mu.RLock()
	before, exists := s.operations[id]
	s.mu.RUnlock()
	if !exists {
		return Operation{}, ErrNotFound
	}
	if terminalOperation(before.Status) {
		return Operation{}, ErrConflict
	}
	now := s.now().UTC()
	next := before
	next.Status, next.Progress, next.Result, next.Error = status, progress, cloneAnyMap(result), operationErr
	if status == "running" && next.StartedAt == nil {
		next.StartedAt = &now
	}
	if terminalOperation(status) {
		next.FinishedAt = &now
		if status == "succeeded" {
			next.Progress = 100
		}
	}
	next.UpdatedAt, next.Version = now, before.Version+1
	if err := s.persist(ctx, KindOperation, id, before.Version, next); err != nil {
		return Operation{}, err
	}
	s.mu.Lock()
	s.operations[id] = next
	s.emitLocked(KindOperation, "updated", id)
	s.mu.Unlock()
	s.auditMutation(ctx, meta, "operation."+status, KindOperation, id, before, next, status != "failed", operationErr)
	return cloneOperation(next), nil
}

func (s *Service) persist(ctx context.Context, kind, id string, expected int64, value any) error {
	record, err := makeRecord(kind, id, expected+1, value)
	if err != nil {
		return err
	}
	return s.store.Put(ctx, record, expected)
}

func (s *Service) auditMutation(ctx context.Context, meta MutationMeta, action, targetKind, targetID string, before, after any, success bool, reason string) {
	if meta.SkipAudit {
		return
	}
	id := randomID("audit")
	event := AuditEvent{
		ID: id, ActorUserID: meta.ActorUserID, Action: action, TargetKind: targetKind, TargetID: targetID,
		RequestID: meta.RequestID, SourceIP: meta.SourceIP, Success: success, Reason: reason,
		Before: toMap(before), After: toMap(after), CreatedAt: s.now().UTC(), Version: 1,
	}
	if event.ActorUserID == "" {
		event.ActorUserID = "system"
	}
	if err := s.persist(ctx, KindAudit, id, 0, event); err != nil {
		return
	}
	s.mu.Lock()
	s.audit[id] = event
	if len(s.audit) > 2200 {
		s.pruneAuditMemory(2000)
	}
	s.emitLocked(KindAudit, "created", id)
	s.mu.Unlock()
}

func loadCurrentRecords(ctx context.Context, store Store) ([]Record, error) {
	if current, ok := store.(CurrentStore); ok {
		return current.LoadCurrent(ctx, 2000)
	}
	return store.Load(ctx)
}

func fingerprintRecords(records []Record) [32]byte {
	keys := make([]string, 0, len(records))
	for _, record := range records {
		keys = append(keys, fmt.Sprintf("%s\x00%s\x00%d", record.Kind, record.ID, record.Version))
	}
	sort.Strings(keys)
	return sha256.Sum256([]byte(strings.Join(keys, "\n")))
}

func (s *Service) pruneAuditMemory(limit int) {
	if limit < 1 || len(s.audit) <= limit {
		return
	}
	values := make([]AuditEvent, 0, len(s.audit))
	for _, value := range s.audit {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	for _, value := range values[limit:] {
		delete(s.audit, value.ID)
	}
}

func (s *Service) userActiveSessionCount(userID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, session := range s.sessions {
		if session.OwnerUserID == userID && session.Status != "closed" && session.Status != "error" {
			count++
		}
	}
	return count
}

func (s *Service) sessionParticipantCount(sessionID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, participant := range s.participants {
		if participant.SessionID == sessionID {
			count++
		}
	}
	return count
}

func (s *Service) hasGrant(subject, deviceID, sessionID, access string, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, grant := range s.grants {
		if grant.SubjectUserID != subject || grant.TargetDeviceID != deviceID || grant.RevokedAt != nil || !grant.ExpiresAt.After(now) {
			continue
		}
		if grant.SessionID != "" && grant.SessionID != sessionID {
			continue
		}
		if access == "control" && grant.Access != "control" {
			continue
		}
		return true
	}
	return false
}

func validID(value string) bool { return validRecordPart(value) }

// User IDs become directory names and Docker container-name components in the
// Executor Manager. Keep this boundary stricter than general control-plane IDs
// so a user accepted by the API can always be provisioned safely.
func validUserID(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
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

func validResourceQuota(quota ResourceQuota) bool {
	if quota.MemoryBytes < 0 || quota.PIDs < 0 || quota.DiskBytes < 0 || quota.MaxSessions < 0 || quota.MaxParticipants < 0 {
		return false
	}
	if quota.CPUs == "" {
		return true
	}
	value, err := strconv.ParseFloat(quota.CPUs, 64)
	return err == nil && value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}

func validObservedState(value string) bool {
	switch value {
	case "unassigned", "provisioning", "starting", "online", "degraded", "offline", "draining", "stopping", "stopped", "error":
		return true
	}
	return false
}

func deriveDeviceState(value DeviceHeartbeat) string {
	if value.RuntimeRunning && !value.RuntimeHealthy {
		return "degraded"
	}
	if value.ClientConnected && (value.RelayRegistered || value.RuntimeRunning) {
		return "online"
	}
	if value.RuntimeRunning || value.ClientConnected {
		return "degraded"
	}
	return "offline"
}

func validSessionOptions(value Session) bool {
	for _, scope := range []string{value.ApplicationID, value.PoolID, value.TenantID, value.ResourceType, value.ResourceID, value.AgentID} {
		if scope != "" && !validID(scope) {
			return false
		}
	}
	contextValues := []string{value.ApplicationID, value.PoolID, value.TenantID, value.ResourceType, value.ResourceID}
	contextCount := 0
	for _, scope := range contextValues {
		if scope != "" {
			contextCount++
		}
	}
	if contextCount != 0 && contextCount != len(contextValues) {
		return false
	}
	if contextCount == 0 && (value.AgentID != "" || value.Protocol != "") {
		return false
	}
	if value.Protocol != "" && value.Protocol != "terminal" && value.Protocol != "acp" {
		return false
	}
	if value.ExecutionTarget != "local" && value.ExecutionTarget != "docker" {
		return false
	}
	if value.AccessMode != "private" && value.AccessMode != "shared" {
		return false
	}
	if value.TransportMode != "auto" && value.TransportMode != "lan" && value.TransportMode != "relay" {
		return false
	}
	if value.AgentMode != "" && value.AgentMode != "terminal" && value.AgentMode != "chat" && value.AgentMode != "acp" {
		return false
	}
	switch value.Status {
	case "", "creating", "starting", "provisioning", "ready", "active", "idle", "reconnecting", "closing", "closed", "error":
		return true
	}
	return false
}

func sessionRelayContextEmpty(value Session) bool {
	return value.ApplicationID == "" && value.PoolID == "" && value.TenantID == "" && value.ResourceType == "" && value.ResourceID == "" && value.AgentID == "" && value.Protocol == ""
}

func terminalOperation(status string) bool {
	return status == "succeeded" || status == "failed" || status == "canceled"
}
func idempotencyIndex(actor, key string) string { return actor + "\x00" + key }

func idempotentOperationID(actor, key string) string {
	sum := sha256.Sum256([]byte(idempotencyIndex(actor, key)))
	return "op-" + hex.EncodeToString(sum[:16])
}

func sameOperationIntent(existing, candidate Operation) bool {
	return existing.ActorUserID == candidate.ActorUserID && existing.IdempotencyKey == candidate.IdempotencyKey &&
		existing.Type == candidate.Type && existing.TargetKind == candidate.TargetKind && existing.TargetID == candidate.TargetID &&
		reflect.DeepEqual(existing.Request, candidate.Request)
}

func mutationAction(exists bool) string {
	if exists {
		return "updated"
	}
	return "created"
}

func randomID(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(raw[:])
	}
	return prefix + "-" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
}

func toMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func cloneStrings(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	out := make(map[string]string, len(value))
	for k, v := range value {
		out[k] = v
	}
	return out
}
func validDeviceMetadata(value map[string]string) bool {
	if len(value) > 32 {
		return false
	}
	total := 0
	for key, item := range value {
		if key == "" || len(key) > 64 || len(item) > 24<<10 {
			return false
		}
		total += len(key) + len(item)
		if total > 32<<10 {
			return false
		}
	}
	return true
}
func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
func cloneAnyMap(value map[string]any) map[string]any { return toMap(value) }
func cloneNode(value Node) Node {
	value.Labels = cloneStrings(value.Labels)
	value.AllowedApplications = append([]string(nil), value.AllowedApplications...)
	return value
}
func cloneDevice(value Device) Device { value.Metadata = cloneStrings(value.Metadata); return value }
func cloneGrant(value AccessGrant) AccessGrant {
	value.Capabilities = append([]string(nil), value.Capabilities...)
	return value
}
func cloneOperation(value Operation) Operation {
	value.Request = cloneAnyMap(value.Request)
	value.Result = cloneAnyMap(value.Result)
	return value
}
func cloneAudit(value AuditEvent) AuditEvent {
	value.Before = cloneAnyMap(value.Before)
	value.After = cloneAnyMap(value.After)
	return value
}
