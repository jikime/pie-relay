package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"sync"
)

const maxLine = 32 * 1024 * 1024

// ChatRequest is a "chat" request to the executor (matches executor.mjs stdin).
type ChatRequest struct {
	Type             string `json:"type"` // set automatically to "chat"
	Prompt           string `json:"prompt"`
	Cwd              string `json:"cwd,omitempty"`
	Model            string `json:"model,omitempty"`
	PermissionMode   string `json:"permissionMode,omitempty"`
	SessionID        string `json:"sessionId,omitempty"`
	ExtendedThinking bool   `json:"extendedThinking,omitempty"`
	ClaudePath       string `json:"claudePath,omitempty"`
}

// Executor supervises a Node executor child process over stdio.
type Executor struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	events  chan Event
	writeMu sync.Mutex
	done    chan struct{} // closed by readLoop after cmd.Wait()
	waitErr error         // process exit error; read after <-done
}

type StartOptions struct {
	WorkingDir       string
	Environment      map[string]string
	ClaudeOAuthToken string
}

// Start spawns `node <executorPath>` (nodeBin defaults to "node"), wires stdio,
// and begins reading events. Caller drives via Chat/PermissionResponse/Abort and
// consumes Events(); Events closes when the executor exits.
func Start(ctx context.Context, nodeBin, executorPath string) (*Executor, error) {
	return StartWithOptions(ctx, nodeBin, executorPath, StartOptions{})
}

func StartWithOptions(ctx context.Context, nodeBin, executorPath string, options StartOptions) (*Executor, error) {
	if nodeBin == "" {
		nodeBin = "node"
	}
	cmd := exec.CommandContext(ctx, nodeBin, executorPath)
	cmd.Env = os.Environ()
	if options.WorkingDir != "" {
		cmd.Env = append(cmd.Env, "CLI_RELAY_DEFAULT_CWD="+options.WorkingDir)
	}
	for key, value := range options.Environment {
		if key != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	var authReader, authWriter *os.File
	if options.ClaudeOAuthToken != "" {
		var err error
		authReader, authWriter, err = os.Pipe()
		if err != nil {
			return nil, err
		}
		defer authReader.Close()
		defer authWriter.Close()
		cmd.ExtraFiles = append(cmd.ExtraFiles, authReader)
		// ExtraFiles starts at fd 3. Only the descriptor number is public; the
		// credential itself never enters argv or the Node process environment.
		cmd.Env = append(cmd.Env, "PIE_CLAUDE_OAUTH_FD=3")
	}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	e := &Executor{cmd: cmd, stdin: stdin, events: make(chan Event), done: make(chan struct{})}
	if options.ClaudeOAuthToken != "" {
		_ = authReader.Close()
		if _, err := io.WriteString(authWriter, options.ClaudeOAuthToken); err != nil {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, err
		}
		if err := authWriter.Close(); err != nil {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, err
		}
	}
	go e.readLoop(ctx, stdout)
	return e, nil
}

func (e *Executor) readLoop(ctx context.Context, stdout io.Reader) {
	// readLoop is the SOLE owner of cmd.Wait(): reap the process exactly once
	// after stdout is fully drained, then close events + signal done. Close()
	// waits on done instead of calling Wait() itself (avoids double-Wait races).
	defer func() {
		e.waitErr = e.cmd.Wait()
		close(e.events)
		close(e.done)
	}()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		if ev, ok := ParseEvent(sc.Bytes()); ok {
			select {
			case e.events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (e *Executor) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	_, err = e.stdin.Write(append(b, '\n'))
	return err
}

// WriteRaw writes a raw NDJSON line to the executor's stdin (used by the relay
// agent to forward browser messages verbatim). A trailing newline is added.
func (e *Executor) WriteRaw(line []byte) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	_, err := e.stdin.Write(append(append([]byte(nil), line...), '\n'))
	return err
}

// Chat starts a turn.
func (e *Executor) Chat(req ChatRequest) error {
	req.Type = "chat"
	return e.writeJSON(req)
}

// PermissionResponse answers a permission_request event.
func (e *Executor) PermissionResponse(requestID string, allow bool, updatedInput json.RawMessage) error {
	return e.writeJSON(map[string]any{
		"type": "permission_response", "requestId": requestID, "allow": allow, "updatedInput": updatedInput,
	})
}

// Abort interrupts the in-progress turn.
func (e *Executor) Abort() error {
	return e.writeJSON(map[string]any{"type": "abort"})
}

// Events returns the executor's event channel.
func (e *Executor) Events() <-chan Event { return e.events }

// Close signals EOF on stdin and waits for the executor to exit. Idempotent:
// stdin.Close is ignored on repeat and <-done returns immediately once closed.
// readLoop performs the single cmd.Wait(); Close returns its result.
func (e *Executor) Close() error {
	_ = e.stdin.Close()
	<-e.done
	return e.waitErr
}
