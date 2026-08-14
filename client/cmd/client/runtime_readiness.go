package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	aiRuntimeMetadataKey = "aiRuntimesV1"
	runtimeProbeTTL      = 5 * time.Minute
	runtimeRetryProbeTTL = 30 * time.Second
	runtimeProbeTimeout  = 10 * time.Second
)

type aiRuntimeModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
	IsDefault   bool   `json:"isDefault,omitempty"`
}

type aiRuntimeReadiness struct {
	Status        string           `json:"status"`
	Installed     bool             `json:"installed"`
	Authenticated bool             `json:"authenticated"`
	Version       string           `json:"version,omitempty"`
	Models        []aiRuntimeModel `json:"models,omitempty"`
}

type aiRuntimeSnapshot struct {
	CheckedAt  time.Time          `json:"checkedAt"`
	ClaudeCode aiRuntimeReadiness `json:"claudeCode"`
	Codex      aiRuntimeReadiness `json:"codex"`
}

type runtimeCommandRunner func(context.Context, string, ...string) ([]byte, error)
type codexModelLoader func(context.Context, string) ([]aiRuntimeModel, error)

type runtimeReadinessMonitor struct {
	mu         sync.Mutex
	snapshot   aiRuntimeSnapshot
	checkedAt  time.Time
	run        runtimeCommandRunner
	loadModels codexModelLoader
}

func newRuntimeReadinessMonitor() *runtimeReadinessMonitor {
	return &runtimeReadinessMonitor{
		run:        runRuntimeCommand,
		loadModels: loadCodexModels,
	}
}

func (m *runtimeReadinessMonitor) metadata(ctx context.Context, refresh bool) map[string]string {
	snapshot := m.read(ctx, refresh)
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return baseDeviceMetadata()
	}
	metadata := baseDeviceMetadata()
	metadata[aiRuntimeMetadataKey] = string(encoded)
	return metadata
}

func (m *runtimeReadinessMonitor) healthy(ctx context.Context) bool {
	snapshot := m.read(ctx, false)
	return snapshot.ClaudeCode.Status == "READY" || snapshot.Codex.Status == "READY"
}

func baseDeviceMetadata() map[string]string {
	return map[string]string{
		"os":     runtime.GOOS,
		"arch":   runtime.GOARCH,
		"client": "pie-client",
	}
}

func (m *runtimeReadinessMonitor) read(ctx context.Context, refresh bool) aiRuntimeSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !refresh && !m.checkedAt.IsZero() && time.Since(m.checkedAt) < runtimeReadinessTTL(m.snapshot) {
		return m.snapshot
	}
	probeCtx, cancel := context.WithTimeout(ctx, runtimeProbeTimeout)
	defer cancel()
	m.snapshot = probeRuntimeReadiness(probeCtx, m.run, m.loadModels)
	m.checkedAt = time.Now()
	return m.snapshot
}

func probeRuntimeReadiness(
	ctx context.Context,
	run runtimeCommandRunner,
	loadModels codexModelLoader,
) aiRuntimeSnapshot {
	type result struct {
		name  string
		value aiRuntimeReadiness
	}
	results := make(chan result, 2)
	go func() {
		results <- result{name: "claude", value: probeCLIRuntime(ctx, "claude", []string{"auth", "status"}, run, nil)}
	}()
	go func() {
		results <- result{name: "codex", value: probeCLIRuntime(ctx, "codex", []string{"login", "status"}, run, loadModels)}
	}()
	snapshot := aiRuntimeSnapshot{CheckedAt: time.Now().UTC()}
	for range 2 {
		current := <-results
		if current.name == "claude" {
			snapshot.ClaudeCode = current.value
		} else {
			snapshot.Codex = current.value
		}
	}
	return snapshot
}

func probeCLIRuntime(
	ctx context.Context,
	command string,
	authArgs []string,
	run runtimeCommandRunner,
	loadModels codexModelLoader,
) aiRuntimeReadiness {
	path, err := exec.LookPath(command)
	if err != nil {
		return aiRuntimeReadiness{Status: "NOT_INSTALLED"}
	}
	value := aiRuntimeReadiness{Installed: true, Status: "ERROR"}
	if output, versionErr := run(ctx, path, "--version"); versionErr == nil {
		value.Version = safeRuntimeText(output, 80)
	}
	authOutput, authErr := run(ctx, path, authArgs...)
	if authErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
			value.Status = "ERROR"
		} else if explicitlyLoggedOut(authOutput) {
			value.Status = "LOGIN_REQUIRED"
		}
		return value
	}
	if explicitlyLoggedOut(authOutput) {
		value.Status = "LOGIN_REQUIRED"
		return value
	}
	value.Status = "READY"
	value.Authenticated = true
	if loadModels != nil {
		models, modelErr := loadModels(ctx, path)
		if modelErr == nil {
			value.Models = models
		}
	}
	return value
}

func runtimeReadinessTTL(snapshot aiRuntimeSnapshot) time.Duration {
	for _, value := range []aiRuntimeReadiness{snapshot.ClaudeCode, snapshot.Codex} {
		if value.Status == "LOGIN_REQUIRED" || value.Status == "ERROR" {
			return runtimeRetryProbeTTL
		}
	}
	return runtimeProbeTTL
}

func explicitlyLoggedOut(output []byte) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(string(output)), " "))
	return strings.Contains(normalized, `"loggedin": false`) ||
		strings.Contains(normalized, `"loggedin":false`) ||
		strings.Contains(normalized, "not logged in") ||
		strings.Contains(normalized, "login required")
}

func runRuntimeCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	output, err := cmd.CombinedOutput()
	if len(output) > 4096 {
		output = output[:4096]
	}
	return output, err
}

func safeRuntimeText(value []byte, limit int) string {
	normalized := strings.Join(strings.Fields(string(value)), " ")
	if len(normalized) > limit {
		normalized = normalized[:limit]
	}
	return normalized
}

type appServerEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func loadCodexModels(ctx context.Context, codexPath string) ([]aiRuntimeModel, error) {
	cmd := exec.CommandContext(ctx, codexPath, "app-server", "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	encoder := json.NewEncoder(stdin)
	decoder := json.NewDecoder(stdout)
	if err := encoder.Encode(map[string]any{
		"method": "initialize",
		"id":     1,
		"params": map[string]any{
			"clientInfo":   map[string]any{"name": "pie-client", "title": "Pie Client", "version": "1"},
			"capabilities": nil,
		},
	}); err != nil {
		return nil, err
	}
	if _, err := readAppServerResponse(decoder, "1"); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized"}); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]any{
		"method": "model/list",
		"id":     2,
		"params": map[string]any{"limit": 50, "includeHidden": false},
	}); err != nil {
		return nil, err
	}
	result, err := readAppServerResponse(decoder, "2")
	if err != nil {
		return nil, err
	}
	var response struct {
		Data []struct {
			ID          string `json:"id"`
			Model       string `json:"model"`
			DisplayName string `json:"displayName"`
			Hidden      bool   `json:"hidden"`
			IsDefault   bool   `json:"isDefault"`
		} `json:"data"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return nil, err
	}
	models := make([]aiRuntimeModel, 0, len(response.Data))
	seen := make(map[string]struct{}, len(response.Data))
	for _, model := range response.Data {
		id := safeRuntimeText([]byte(fallback(model.ID, model.Model)), 100)
		if id == "" || model.Hidden {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, aiRuntimeModel{
			ID:          id,
			DisplayName: safeRuntimeText([]byte(model.DisplayName), 100),
			IsDefault:   model.IsDefault,
		})
		if len(models) >= 30 {
			break
		}
	}
	return models, nil
}

func readAppServerResponse(decoder *json.Decoder, expectedID string) (json.RawMessage, error) {
	for {
		var envelope appServerEnvelope
		if err := decoder.Decode(&envelope); err != nil {
			return nil, err
		}
		if string(envelope.ID) != expectedID {
			continue
		}
		if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			return nil, errors.New("Codex 모델 목록을 읽지 못했습니다")
		}
		return envelope.Result, nil
	}
}
