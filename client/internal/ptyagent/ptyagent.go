// Package ptyagent bridges the local PTY host to the relay's /ws/agent leg.
//
// It is the terminal-room counterpart of chatagent: instead of supervising the
// SDK chat executor, it supervises node-executor/pty-host.mjs (a node-pty shell)
// and bridges relay ws ⇄ pty-host stdio VERBATIM as NDJSON lines —
//
//	pty-host stdout (one JSON frame per line) → relay ws
//	relay ws                                   → pty-host stdin
//
// There is no per-frame logic here: the driver gate, set_driver handling and
// base64 output live in pty-host.mjs; the relay injects the trusted `from` on
// participant frames and gates set_driver to host-only. This layer is a byte
// pump with the same reconnect/backoff discipline as chatagent.Run, and the
// same restart-on-crash discipline: a mid-session pty-host crash respawns the
// process instead of exiting the daemon (see supervisePTYHostWithOptions).
package ptyagent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/coder/websocket"

	"cli-relay/client/internal/chatagent"
)

const (
	// pty_output frames carry base64 terminal bytes; a burst (e.g. `cat` of a big
	// file) can exceed the default scanner line size, so allow a generous cap.
	maxLine = 8 * 1024 * 1024
)

// These are `var`, not `const`, solely so tests can shrink them for fast,
// deterministic runs (e.g. a crash-loop escalation test would otherwise take
// tens of seconds of real backoff). Production behavior is unaffected.
var (
	reconnectBase = 1 * time.Second
	reconnectMax  = 30 * time.Second

	// Child-process (pty-host) restart supervision: a mid-session crash
	// restarts pty-host instead of tearing down the whole daemon (which would
	// deregister the host from the relay and drop the user's shell entirely).
	// A genuine crash-loop must still surface rather than spin forever, so a
	// restart budget of maxChildRestarts within childRestartWindow escalates
	// to a fatal error once exhausted.
	maxChildRestarts   = 5
	childRestartWindow = 30 * time.Second
)

// ptyHost holds one supervised pty-host child process instance: its stdin
// (relay→pty-host) and stdout (pty-host→relay) pipes.
type ptyHost struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

type Options struct {
	SessionID     string
	StreamID      string
	WorkingDir    string
	InitialDriver string
	AgentID       string
	// ClaudeOAuthToken is consumed only by the chat runner. Terminal/PTY
	// processes never receive the subscription credential.
	ClaudeOAuthToken string
	// OnRelayState reports the data-plane state to the supervising Session
	// Manager. It must be non-blocking; callers normally update an in-memory
	// status record. Keeping this callback out of the wire protocol lets both
	// terminal and chat runners expose the same health signal.
	OnRelayState func(string)
}

func reportRelayState(callback func(string), state string) {
	if callback != nil {
		callback(state)
	}
}

func spawnPTYHostWithOptions(ctx context.Context, ptyHostPath string, options Options) (*ptyHost, error) {
	cmd := exec.CommandContext(ctx, "node", ptyHostPath)
	cmd.Env = append([]string(nil), os.Environ()...)
	if options.WorkingDir != "" {
		cmd.Env = append(cmd.Env, "CLI_RELAY_DEFAULT_CWD="+options.WorkingDir)
	}
	if options.InitialDriver != "" {
		cmd.Env = append(cmd.Env, "CLI_RELAY_INITIAL_DRIVER="+options.InitialDriver)
	}
	if options.StreamID != "" {
		cmd.Env = append(cmd.Env, "CLI_RELAY_STREAM_ID="+options.StreamID)
	}
	if options.SessionID != "" {
		cmd.Env = append(cmd.Env, "PIE_RELAY_SESSION_ID="+options.SessionID)
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
	return &ptyHost{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

// close signals EOF on stdin (best-effort) and reaps the process, mirroring
// executor.Executor.Close.
func (h *ptyHost) close() {
	_ = h.stdin.Close()
	_ = h.cmd.Wait()
}

// Run spawns `node <ptyHostPath>` once and bridges it to the relay agent
// endpoint, reconnecting the ws with backoff if it drops WHILE KEEPING THE
// PTY-HOST (and thus the user's shell) ALIVE — a transient relay blip must not
// kill the terminal. If pty-host itself dies mid-session (Node crash,
// OOM-kill, etc.), Run RESTARTS it in place (see supervisePTYHostWithOptions) rather than
// exiting — only a genuine crash-loop (maxChildRestarts within
// childRestartWindow) surfaces as a fatal error. Returns only when ctx is done
// or pty-host crash-loops past its restart budget.
// The signature matches chatagent.Run so main's runConnect can drive either.
// On a 401/403 handshake it returns chatagent.ErrUnauthorized so the daemon
// surfaces the same "re-enroll" guidance for both room modes.
func Run(ctx context.Context, relayURL, ptyHostPath, token string) error {
	return RunWithOptions(ctx, relayURL, ptyHostPath, token, Options{})
}

func RunWithOptions(ctx context.Context, relayURL, ptyHostPath, token string, options Options) error {
	reportRelayState(options.OnRelayState, "connecting")
	defer reportRelayState(options.OnRelayState, "disconnected")
	var dialOpts *websocket.DialOptions
	if token != "" {
		dialOpts = &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer " + token}}}
	}

	host, err := spawnPTYHostWithOptions(ctx, ptyHostPath, options)
	if err != nil {
		return err
	}

	// Current pty-host instance, swapped by supervisePTYHostWithOptions on each restart.
	// The ws→pty-host write path always writes to whatever instance is
	// current (nil during the brief gap while a fresh one is spawned).
	var hMu sync.Mutex
	var curHost *ptyHost
	setHost := func(h *ptyHost) { hMu.Lock(); curHost = h; hMu.Unlock() }
	getHost := func() *ptyHost { hMu.Lock(); defer hMu.Unlock(); return curHost }
	setHost(host)

	// Current agent ws, swapped on each (re)connect. The single stdout→ws pump
	// below always writes to whatever conn is current.
	var mu sync.Mutex
	var cur *boundedWSWriter
	setConn := func(c *boundedWSWriter) { mu.Lock(); cur = c; mu.Unlock() }
	getConn := func() *boundedWSWriter { mu.Lock(); defer mu.Unlock(); return cur }

	// pty-host stdout (NDJSON) → current relay ws, verbatim, for one pty-host
	// instance. Frames emitted during a brief reconnect gap are dropped
	// rather than tearing down the shell. Returns once h's stdout closes,
	// i.e. h's process exited.
	pumpStdout := func(h *ptyHost) {
		sc := bufio.NewScanner(h.stdout)
		sc.Buffer(make([]byte, 0, 64*1024), maxLine)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			if writer := getConn(); writer != nil {
				raw := append([]byte(nil), line...) // copy: Bytes() is reused by the scanner
				_ = writer.Send(raw)
			}
		}
	}

	var writeMu sync.Mutex
	writeLine := func(data []byte) error {
		h := getHost()
		if h == nil {
			return fmt.Errorf("pty-host 재시작 중")
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, werr := h.stdin.Write(append(append([]byte(nil), data...), '\n'))
		return werr
	}

	// hostDone closes once the pty-host supervisor stops for good: either a
	// clean shutdown (ctx done) or a fatal crash-loop escalation (fatalCh
	// carries the error in the latter case).
	hostDone := make(chan struct{})
	fatalCh := make(chan error, 1)
	go func() {
		defer close(hostDone)
		supervisePTYHostWithOptions(ctx, host, ptyHostPath, options, pumpStdout, setHost, fatalCh)
	}()

	defer func() {
		// Ask the currently-supervised instance to exit (mirrors the original
		// single-instance defer stdin.Close()+cmd.Wait()), then wait for the
		// supervisor goroutine to fully stop so no goroutine/process outlives
		// Run.
		if h := getHost(); h != nil {
			h.close()
		}
		<-hostDone
	}()

	backoff := reconnectBase
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c, resp, derr := websocket.Dial(ctx, relayURL, dialOpts)
		if derr != nil {
			// Auth rejection at the handshake is terminal — mirror chatagent so the
			// daemon reports the token as expired/revoked (re-enroll from the app).
			if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
				return fmt.Errorf("%w (HTTP %d)", chatagent.ErrUnauthorized, resp.StatusCode)
			}
			reportRelayState(options.OnRelayState, "reconnecting")
			delay := jitteredDelay(backoff)
			log.Printf("relay 연결 실패(터미널): %s — %v (%s 후 재시도)", relayURL, derr, delay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ferr := <-fatalCh:
				return ferr
			case <-time.After(delay):
			}
			backoff = nextBackoff(backoff)
			continue
		}
		c.SetReadLimit(16 << 20)
		writer := newBoundedWSWriter(ctx, c)
		// Optional v2 negotiation. Old relays forward this unknown frame to the
		// pty-host, which ignores it; v2 relays consume it and return an ack.
		streamID := options.StreamID
		if streamID == "" {
			streamID = "terminal"
		}
		join, _ := json.Marshal(map[string]string{"type": "relay_join", "protocolVersion": "2.0", "streamId": streamID, "clientId": "clientd"})
		_ = writer.Send(join)
		setConn(writer)
		reportRelayState(options.OnRelayState, "connected")
		log.Printf("relay 연결됨(터미널): %s", relayURL)
		backoff = reconnectBase

		// relay ws → pty-host stdin, until the ws drops. Reads happen on a
		// helper goroutine so this select can also react to fatalCh while the
		// ws is idle — a plain blocking c.Read(ctx) would hide a crash-loop
		// escalation until the connection happened to drop on its own, which
		// could be never for an idle-but-healthy relay session. A write
		// failure just means the CURRENT pty-host instance is between
		// processes (dead or restarting) — that self-heals via
		// supervisePTYHostWithOptions, so it no longer tears down the ws bridge; the
		// frame is dropped, best-effort.
		type wsMsg struct {
			data []byte
			err  error
		}
		readCh := make(chan wsMsg)
		connDone := make(chan struct{})
		go func() {
			for {
				_, data, err := c.Read(ctx)
				select {
				case readCh <- wsMsg{data, err}:
				case <-connDone:
					return
				}
				if err != nil {
					return
				}
			}
		}()

		var earlyExit error
	readLoop:
		for {
			select {
			case ferr := <-fatalCh:
				earlyExit = ferr
				break readLoop
			case m := <-readCh:
				if m.err != nil {
					break readLoop
				}
				if werr := writeLine(m.data); werr != nil {
					log.Printf("pty-host 전달 실패(재시작 중일 수 있음): %v", werr)
				}
			}
		}
		close(connDone) // unblocks the read goroutine's pending/next send, if any
		setConn(nil)
		reportRelayState(options.OnRelayState, "reconnecting")
		writer.Stop()
		_ = c.Close(websocket.StatusNormalClosure, "")
		if earlyExit != nil {
			return earlyExit
		}
		if err := ctx.Err(); err != nil {
			log.Printf("relay 세션 종료(터미널): %s", relayURL)
			return err
		}
		delay := jitteredDelay(backoff)
		log.Printf("relay 연결 끊김(터미널): %s (%s 후 재접속)", relayURL, delay)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case ferr := <-fatalCh:
			return ferr
		case <-time.After(delay):
		}
		backoff = nextBackoff(backoff)
	}
}

// supervisePTYHostWithOptions bridges the initial pty-host instance via pump, and on
// unexpected exit (pump returns while ctx is still live) restarts it: fresh
// process, rewired via setCurrent so the ws→pty-host write path always sees
// the live instance (or nil during the brief respawn gap). Restarts use the
// same jittered backoff progression as the ws reconnect loop. If
// maxChildRestarts happen within childRestartWindow, it gives up and sends the
// escalation error on fatalCh — a genuine crash-loop must still surface and
// end the daemon, but a one-off crash self-heals silently. Returns when ctx is
// done or after sending to fatalCh.
func supervisePTYHostWithOptions(ctx context.Context, first *ptyHost, ptyHostPath string, options Options, pump func(*ptyHost), setCurrent func(*ptyHost), fatalCh chan<- error) {
	tracker := &childRestartTracker{}
	h := first
	backoff := reconnectBase
	for {
		pump(h)
		if ctx.Err() != nil {
			return
		}
		setCurrent(nil)
		for {
			if !tracker.allow(time.Now()) {
				fatalCh <- fmt.Errorf("pty-host 프로세스가 반복적으로 종료되었습니다 (%d회/%s 이내) — 데몬을 다시 실행하세요", maxChildRestarts, childRestartWindow)
				return
			}
			if tracker.windowReset {
				backoff = reconnectBase
			}
			delay := jitteredDelay(backoff)
			log.Printf("pty-host 프로세스 종료 감지 — %s 후 재시작합니다 (%d/%d)", delay, tracker.restarts, maxChildRestarts)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			backoff = nextBackoff(backoff)
			if ctx.Err() != nil {
				return
			}
			nh, serr := spawnPTYHostWithOptions(ctx, ptyHostPath, options)
			if serr != nil {
				log.Printf("pty-host 재시작 실패: %v (다시 시도)", serr)
				continue
			}
			h = nh
			break
		}
		setCurrent(h)
	}
}

// childRestartTracker tracks a rolling restart-attempt window with no I/O of
// its own, so tests can drive it with an explicit clock instead of sleeping.
// It implements the crash-loop guard: up to maxChildRestarts attempts are
// allowed within childRestartWindow of the first attempt in the window; the
// window rolls over (resetting the count) once it's been quiet for longer
// than childRestartWindow.
type childRestartTracker struct {
	restarts    int
	windowStart time.Time
	windowReset bool // set by the most recent allow() call, for backoff reset
}

// allow reports whether another restart attempt is permitted at time now. It
// returns false once the budget (maxChildRestarts within childRestartWindow)
// is exhausted.
func (t *childRestartTracker) allow(now time.Time) bool {
	t.windowReset = false
	if t.windowStart.IsZero() || now.Sub(t.windowStart) > childRestartWindow {
		t.windowStart = now
		t.restarts = 0
		t.windowReset = true
	}
	t.restarts++
	return t.restarts <= maxChildRestarts
}

// jitteredDelay returns a randomized duration in [backoff/2, backoff] (equal
// jitter). Many daemons whose ws drops at the same instant (e.g. a relay
// restart) reset to reconnectBase together; without jitter they'd all re-dial
// in lockstep (thundering herd). The stored backoff accumulator (see
// nextBackoff) stays deterministic — only the actual sleep is randomized —
// so the doubling/cap progression stays easy to reason about and test.
func jitteredDelay(backoff time.Duration) time.Duration {
	if backoff <= 0 {
		return 0
	}
	half := backoff / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

func nextBackoff(b time.Duration) time.Duration {
	b *= 2
	if b > reconnectMax {
		b = reconnectMax
	}
	return b
}
