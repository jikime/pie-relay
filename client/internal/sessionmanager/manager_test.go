package sessionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"cli-relay/client/internal/ptyagent"
)

type fakeRunner struct {
	mu      sync.Mutex
	started []ptyagent.Options
	paths   []string
}

func (r *fakeRunner) run(ctx context.Context, _ string, path string, _ string, options ptyagent.Options) error {
	r.mu.Lock()
	r.started = append(r.started, options)
	r.paths = append(r.paths, path)
	r.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func TestManagerSelectsChatRunnerAndExecutorPath(t *testing.T) {
	terminal, chat := &fakeRunner{}, &fakeRunner{}
	manager := NewWithRunners(context.Background(), "pty-host.mjs", "executor.mjs", 2, terminal.run, chat.run)
	const oauth = "sk-ant-oat-session-manager-secret-000000000001"
	status, duplicate, err := manager.Start(Config{ID: "chat-a", AgentMode: "chat", RelayURL: "ws://relay/ws/agent", Token: "secret", ClaudeOAuthToken: oauth, ClaudeAuthVersion: "v-oauth-1"})
	if err != nil || duplicate {
		t.Fatalf("status=%+v duplicate=%t err=%v", status, duplicate, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		chat.mu.Lock()
		started, paths, options := len(chat.started), append([]string(nil), chat.paths...), append([]ptyagent.Options(nil), chat.started...)
		chat.mu.Unlock()
		if started == 1 {
			if paths[0] != "executor.mjs" {
				t.Fatalf("executor path=%q", paths[0])
			}
			if options[0].ClaudeOAuthToken != oauth {
				t.Fatalf("chat runner did not receive OAuth token")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("chat runner did not start")
		}
		time.Sleep(time.Millisecond)
	}
	terminal.mu.Lock()
	terminalStarts := len(terminal.started)
	terminal.mu.Unlock()
	if terminalStarts != 0 {
		t.Fatalf("terminal runner was selected %d times", terminalStarts)
	}
	current, ok := manager.Get("chat-a")
	if !ok || current.AgentMode != "chat" || current.StreamID != "chat" || current.ClaudeAuthVersion != "v-oauth-1" {
		t.Fatalf("chat status=%+v ok=%t", current, ok)
	}
	encoded, err := json.Marshal(manager.List())
	if err != nil || strings.Contains(string(encoded), oauth) {
		t.Fatalf("session status leaked OAuth token: %s err=%v", encoded, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestManagerSelectsACPRunnerAndDedicatedExecutorPath(t *testing.T) {
	terminal, agent := &fakeRunner{}, &fakeRunner{}
	manager := NewWithExecutors(context.Background(), "pty-host.mjs", "executor.mjs", "acp-executor.mjs", 2, terminal.run, agent.run)
	status, duplicate, err := manager.Start(Config{ID: "acp-a", AgentMode: "acp", RelayURL: "ws://relay/ws/agent", Token: "secret"})
	if err != nil || duplicate {
		t.Fatalf("status=%+v duplicate=%t err=%v", status, duplicate, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		agent.mu.Lock()
		started, paths := len(agent.started), append([]string(nil), agent.paths...)
		agent.mu.Unlock()
		if started == 1 {
			if paths[0] != "acp-executor.mjs" {
				t.Fatalf("ACP executor path=%q", paths[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ACP runner did not start")
		}
		time.Sleep(time.Millisecond)
	}
	current, ok := manager.Get("acp-a")
	if !ok || current.AgentMode != "acp" || current.StreamID != "acp" {
		t.Fatalf("ACP status=%+v ok=%t", current, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestManagerRunsIndependentSessions(t *testing.T) {
	runner := &fakeRunner{}
	manager := New(context.Background(), "pty-host.mjs", 2, runner.run)
	first, duplicate, err := manager.Start(Config{ID: "session-a", RelayURL: "ws://relay/ws/agent", Token: "a", WorkingDir: "/workspace/a"})
	if err != nil || duplicate {
		t.Fatalf("first=%+v dup=%t err=%v", first, duplicate, err)
	}
	if _, _, err := manager.Start(Config{ID: "session-b", RelayURL: "ws://relay/ws/agent", Token: "b", StreamID: "terminal-b"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Start(Config{ID: "session-c", RelayURL: "ws://relay/ws/agent", Token: "c"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("limit=%v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		runner.mu.Lock()
		count := len(runner.started)
		runner.mu.Unlock()
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sessions did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if len(manager.List()) != 2 {
		t.Fatal("sessions not listed")
	}
	if err := manager.Remove(context.Background(), "session-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Get("session-a"); ok {
		t.Fatal("removed session remains")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestManagerStartIsIdempotentButRejectsConfigDrift(t *testing.T) {
	runner := &fakeRunner{}
	manager := New(context.Background(), "pty-host.mjs", 1, runner.run)
	defer manager.Close(context.Background())
	config := Config{ID: "session-a", RelayURL: "ws://relay/ws/agent", Token: "token"}
	if _, _, err := manager.Start(config); err != nil {
		t.Fatal(err)
	}
	if _, duplicate, err := manager.Start(config); err != nil || !duplicate {
		t.Fatalf("duplicate=%t err=%v", duplicate, err)
	}
	config.Token = "different"
	if _, duplicate, err := manager.Start(config); err != nil || !duplicate {
		t.Fatalf("rotated token retry duplicate=%t err=%v", duplicate, err)
	}
	config.RelayURL = "ws://different/ws/agent"
	if _, _, err := manager.Start(config); !errors.Is(err, ErrExists) {
		t.Fatalf("drift=%v", err)
	}
}

func TestManagerTracksActualRelayConnectionState(t *testing.T) {
	connected := make(chan struct{})
	runner := func(ctx context.Context, _, _, _ string, options ptyagent.Options) error {
		options.OnRelayState("connected")
		close(connected)
		<-ctx.Done()
		return ctx.Err()
	}
	manager := New(context.Background(), "pty-host.mjs", 1, runner)
	if _, _, err := manager.Start(Config{ID: "session-state", RelayURL: "ws://relay/ws/agent", Token: "token"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("runner did not report Relay connection")
	}
	status, ok := manager.Get("session-state")
	if !ok || status.State != "running" || status.RelayState != "connected" {
		t.Fatalf("status=%+v ok=%t", status, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	status, _ = manager.Get("session-state")
	if status.RelayState != "disconnected" || status.State != "stopped" {
		t.Fatalf("stopped status=%+v", status)
	}
}
