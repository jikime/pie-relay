package sessionmanager

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"cli-relay/client/internal/chatagent"
	"cli-relay/client/internal/ptyagent"
)

var (
	ErrExists   = errors.New("session already exists with different configuration")
	ErrNotFound = errors.New("session not found")
	ErrLimit    = errors.New("session limit reached")
)

type Config struct {
	ID                string `json:"id"`
	AgentID           string `json:"agentId,omitempty"`
	AgentMode         string `json:"agentMode,omitempty"`
	RelayURL          string `json:"relayUrl"`
	Token             string `json:"token,omitempty"`
	PTYHostPath       string `json:"ptyHostPath,omitempty"`
	ExecutorPath      string `json:"executorPath,omitempty"`
	StreamID          string `json:"streamId,omitempty"`
	WorkingDir        string `json:"workingDir,omitempty"`
	InitialDriver     string `json:"initialDriver,omitempty"`
	ClaudeOAuthToken  string `json:"claudeOAuthToken,omitempty"`
	ClaudeAuthVersion string `json:"claudeAuthVersion,omitempty"`
}

type Status struct {
	ID                string     `json:"id"`
	AgentID           string     `json:"agentId,omitempty"`
	AgentMode         string     `json:"agentMode"`
	RelayURL          string     `json:"relayUrl"`
	StreamID          string     `json:"streamId"`
	WorkingDir        string     `json:"workingDir,omitempty"`
	ClaudeAuthVersion string     `json:"claudeAuthVersion,omitempty"`
	State             string     `json:"state"`
	RelayState        string     `json:"relayState"`
	StartedAt         time.Time  `json:"startedAt"`
	StoppedAt         *time.Time `json:"stoppedAt,omitempty"`
	LastError         string     `json:"lastError,omitempty"`
}

type Runner func(context.Context, string, string, string, ptyagent.Options) error

type session struct {
	config Config
	status Status
	cancel context.CancelFunc
	done   chan struct{}
}

type Manager struct {
	ctx                 context.Context
	terminalRunner      Runner
	chatRunner          Runner
	defaultPTYHostPath  string
	defaultExecutorPath string
	defaultACPPath      string
	limit               int
	mu                  sync.RWMutex
	sessions            map[string]*session
}

func New(ctx context.Context, defaultPTYHostPath string, limit int, runner Runner) *Manager {
	return NewWithRunners(ctx, defaultPTYHostPath, "", limit, runner, nil)
}

// NewWithRunners supports both terminal and Claude Agent SDK chat sessions.
// The legacy New constructor remains terminal-only compatible for callers and
// tests that inject a single PTY runner.
func NewWithRunners(ctx context.Context, defaultPTYHostPath, defaultExecutorPath string, limit int, terminalRunner, chatRunner Runner) *Manager {
	defaultACPPath := "acp-executor.mjs"
	if defaultExecutorPath != "" {
		defaultACPPath = filepath.Join(filepath.Dir(defaultExecutorPath), "acp-executor.mjs")
	}
	return NewWithExecutors(ctx, defaultPTYHostPath, defaultExecutorPath, defaultACPPath, limit, terminalRunner, chatRunner)
}

// NewWithExecutors allows packaged clientd deployments to provide distinct
// legacy SDK-chat and ACP adapter entrypoints without changing the public
// session API. ACP uses the same reliable Relay bridge as chat; only the local
// stdio adapter differs.
func NewWithExecutors(ctx context.Context, defaultPTYHostPath, defaultExecutorPath, defaultACPPath string, limit int, terminalRunner, chatRunner Runner) *Manager {
	if limit < 1 {
		limit = 16
	}
	if terminalRunner == nil {
		terminalRunner = ptyagent.RunWithOptions
	}
	if chatRunner == nil {
		chatRunner = func(ctx context.Context, relayURL, executorPath, token string, options ptyagent.Options) error {
			return chatagent.RunWithRelayStateAndRuntimeAuth(ctx, relayURL, executorPath, token, options.WorkingDir, options.AgentID, options.ClaudeOAuthToken, options.OnRelayState)
		}
	}
	return &Manager{ctx: ctx, terminalRunner: terminalRunner, chatRunner: chatRunner, defaultPTYHostPath: defaultPTYHostPath, defaultExecutorPath: defaultExecutorPath, defaultACPPath: defaultACPPath, limit: limit, sessions: map[string]*session{}}
}

func (m *Manager) Start(config Config) (Status, bool, error) {
	if !validID(config.ID) || config.RelayURL == "" || config.Token == "" {
		return Status{}, false, errors.New("id, relayUrl and token are required")
	}
	if config.AgentMode == "" {
		config.AgentMode = "terminal"
	}
	if config.AgentMode != "terminal" && config.AgentMode != "chat" && config.AgentMode != "acp" {
		return Status{}, false, errors.New("agentMode must be terminal, chat or acp")
	}
	if config.StreamID == "" {
		config.StreamID = config.AgentMode
	}
	if !validID(config.StreamID) {
		return Status{}, false, errors.New("invalid stream id")
	}
	if config.AgentMode == "terminal" {
		if config.PTYHostPath == "" {
			config.PTYHostPath = m.defaultPTYHostPath
		}
		if config.PTYHostPath == "" {
			return Status{}, false, errors.New("pty host path is required")
		}
	} else {
		if config.ExecutorPath == "" {
			if config.AgentMode == "acp" {
				config.ExecutorPath = m.defaultACPPath
			} else {
				config.ExecutorPath = m.defaultExecutorPath
			}
		}
		if config.ExecutorPath == "" {
			return Status{}, false, errors.New("executor path is required for chat and acp sessions")
		}
	}
	m.mu.Lock()
	if existing := m.sessions[config.ID]; existing != nil {
		status := existing.status
		if sameConfig(existing.config, config) {
			m.mu.Unlock()
			return status, true, nil
		}
		m.mu.Unlock()
		return Status{}, false, ErrExists
	}
	active := 0
	for _, value := range m.sessions {
		if value.status.State == "starting" || value.status.State == "running" {
			active++
		}
	}
	if active >= m.limit {
		m.mu.Unlock()
		return Status{}, false, ErrLimit
	}
	ctx, cancel := context.WithCancel(m.ctx)
	now := time.Now().UTC()
	s := &session{config: config, status: Status{ID: config.ID, AgentID: config.AgentID, AgentMode: config.AgentMode, RelayURL: config.RelayURL, StreamID: config.StreamID, WorkingDir: config.WorkingDir, ClaudeAuthVersion: config.ClaudeAuthVersion, State: "starting", RelayState: "connecting", StartedAt: now}, cancel: cancel, done: make(chan struct{})}
	m.sessions[config.ID] = s
	// Copy the initial status while holding the same lock used by run(). Once
	// the goroutine starts it may immediately transition State to "running".
	// Returning s.status after unlock raced with that write under -race.
	status := s.status
	m.mu.Unlock()
	go m.run(ctx, s)
	return status, false, nil
}

func (m *Manager) run(ctx context.Context, value *session) {
	m.mu.Lock()
	if value.status.State == "starting" {
		value.status.State = "running"
	}
	m.mu.Unlock()
	runner, path := m.terminalRunner, value.config.PTYHostPath
	if value.config.AgentMode == "chat" || value.config.AgentMode == "acp" {
		runner, path = m.chatRunner, value.config.ExecutorPath
	}
	onRelayState := func(state string) {
		m.mu.Lock()
		value.status.RelayState = state
		m.mu.Unlock()
	}
	err := runner(ctx, value.config.RelayURL, path, value.config.Token, ptyagent.Options{SessionID: value.config.ID, StreamID: value.config.StreamID, WorkingDir: value.config.WorkingDir, InitialDriver: value.config.InitialDriver, AgentID: value.config.AgentID, ClaudeOAuthToken: value.config.ClaudeOAuthToken, OnRelayState: onRelayState})
	now := time.Now().UTC()
	m.mu.Lock()
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		value.status.State = "stopped"
	} else {
		value.status.State = "failed"
		if err != nil {
			value.status.LastError = err.Error()
		}
	}
	value.status.RelayState = "disconnected"
	value.status.StoppedAt = &now
	// Drop both capabilities from the long-lived session record as soon as the
	// runner exits. Status responses never contained them, and stopped sessions
	// no longer need them for idempotency.
	value.config.Token = ""
	value.config.ClaudeOAuthToken = ""
	close(value.done)
	m.mu.Unlock()
}

func (m *Manager) Stop(ctx context.Context, id string) (Status, error) {
	m.mu.RLock()
	value := m.sessions[id]
	m.mu.RUnlock()
	if value == nil {
		return Status{}, ErrNotFound
	}
	value.cancel()
	select {
	case <-value.done:
	case <-ctx.Done():
		return Status{}, ctx.Err()
	}
	m.mu.RLock()
	status := value.status
	m.mu.RUnlock()
	return status, nil
}

func (m *Manager) Remove(ctx context.Context, id string) error {
	status, err := m.Stop(ctx, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.sessions[id]
	if current != nil && (status.State == "stopped" || status.State == "failed") {
		delete(m.sessions, id)
		return nil
	}
	if current == nil {
		return ErrNotFound
	}
	return errors.New("session is still active")
}

func (m *Manager) Get(id string) (Status, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	value := m.sessions[id]
	if value == nil {
		return Status{}, false
	}
	return value.status, true
}

func (m *Manager) List() []Status {
	m.mu.RLock()
	out := make([]Status, 0, len(m.sessions))
	for _, value := range m.sessions {
		out = append(out, value.status)
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.RLock()
	values := make([]*session, 0, len(m.sessions))
	for _, value := range m.sessions {
		values = append(values, value)
	}
	m.mu.RUnlock()
	for _, value := range values {
		value.cancel()
	}
	for _, value := range values {
		select {
		case <-value.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func sameConfig(a, b Config) bool {
	// A controller retry can mint a fresh equivalent capability after it has
	// already started the session but before the ready state was persisted. The
	// active runner keeps its original valid token; token rotation is handled by
	// an explicit stop/start, not by spawning a duplicate PTY.
	return a.ID == b.ID && a.AgentID == b.AgentID && a.AgentMode == b.AgentMode && a.RelayURL == b.RelayURL && a.PTYHostPath == b.PTYHostPath && a.ExecutorPath == b.ExecutorPath && a.StreamID == b.StreamID && a.WorkingDir == b.WorkingDir && a.InitialDriver == b.InitialDriver
}
func validID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':' || c == '/' {
			continue
		}
		return false
	}
	return true
}
