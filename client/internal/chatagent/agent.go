// Package chatagent bridges the local executor to the relay's /ws/agent leg.
package chatagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"cli-relay/client/internal/executor"
)

// These are `var`, not `const`, solely so tests can shrink them for fast,
// deterministic runs (e.g. a crash-loop escalation test would otherwise take
// tens of seconds of real backoff). Production behavior is unaffected.
var (
	reconnectBase = 1 * time.Second
	reconnectMax  = 30 * time.Second

	// Child-process (local executor) restart supervision: a mid-session crash
	// restarts the executor instead of tearing down the whole daemon (which
	// would deregister the host from the relay and strand participants). A
	// genuine crash-loop must still surface rather than spin forever, so a
	// restart budget of maxChildRestarts within childRestartWindow escalates
	// to a fatal error once exhausted.
	maxChildRestarts   = 5
	childRestartWindow = 30 * time.Second
)

// ErrUnauthorized is returned when the relay rejects the bearer token at the
// WebSocket handshake (HTTP 401/403). Reconnecting cannot fix an expired,
// revoked, or invalid PAT, so Run stops instead of entering its backoff loop —
// letting the caller surface a re-login hint.
var ErrUnauthorized = errors.New("relay rejected the token (expired or invalid)")

// Run starts the local executor and bridges it to the relay agent endpoint
// (relay ws ⇄ executor stdio, verbatim). If the agent ws drops (relay restart,
// network blip, idle close), it AUTOMATICALLY RECONNECTS with backoff — a
// transient drop must not leave the daemon permanently unregistered
// ("agent:unavailable"), which strands the browser at "AI 응답 중". If the
// local executor itself dies mid-session (Node crash, OOM-kill, etc.), Run
// RESTARTS it in place (see superviseExecutor) rather than exiting — only a
// genuine crash-loop (maxChildRestarts within childRestartWindow) surfaces as
// a fatal error. Returns only when ctx is done (clean shutdown) or the
// executor crash-loops past its restart budget.
// relayURL e.g. ws://host:13412/ws/agent?user=U. token, when non-empty, is sent
// as Authorization: Bearer <token> on each dial.
func Run(ctx context.Context, relayURL, executorPath, token string) error {
	return RunWithWorkingDir(ctx, relayURL, executorPath, token, "")
}

func RunWithWorkingDir(ctx context.Context, relayURL, executorPath, token, workingDir string) error {
	return RunWithRelayState(ctx, relayURL, executorPath, token, workingDir, nil)
}

// RunWithRelayState is the managed-session entrypoint. Standalone callers keep
// using Run/RunWithWorkingDir, while clientd can distinguish a live process
// from a live Relay websocket and report accurate health to the Control Plane.
func RunWithRelayState(ctx context.Context, relayURL, executorPath, token, workingDir string, onRelayState func(string)) error {
	return RunWithRelayStateAndRuntime(ctx, relayURL, executorPath, token, workingDir, "", onRelayState)
}

func RunWithRelayStateAndRuntime(ctx context.Context, relayURL, executorPath, token, workingDir, agentID string, onRelayState func(string)) error {
	return RunWithRelayStateAndRuntimeAuth(ctx, relayURL, executorPath, token, workingDir, agentID, "", onRelayState)
}

// RunWithRelayStateAndRuntimeAuth keeps the OAuth token out of the daemon and
// Node process environments. executor.Start sends it over the local stdin
// control pipe, and the Node adapter supplies it only to Claude Code children.
func RunWithRelayStateAndRuntimeAuth(ctx context.Context, relayURL, executorPath, token, workingDir, agentID, claudeOAuthToken string, onRelayState func(string)) error {
	reportRelayState(onRelayState, "connecting")
	defer reportRelayState(onRelayState, "disconnected")
	var dialOpts *websocket.DialOptions
	if token != "" {
		dialOpts = &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer " + token}}}
	}

	runtimeEnv := acpRuntimeEnvironment(executorPath, agentID)
	ex, err := executor.StartWithOptions(ctx, "", executorPath, executor.StartOptions{WorkingDir: workingDir, Environment: runtimeEnv, ClaudeOAuthToken: claudeOAuthToken})
	if err != nil {
		return err
	}

	// Current executor instance, swapped by superviseExecutor on each restart.
	// The ws→executor write path always writes to whatever instance is
	// current (nil during the brief gap while a fresh one is spawned).
	var exMu sync.Mutex
	var curEx *executor.Executor
	setExecutor := func(e *executor.Executor) { exMu.Lock(); curEx = e; exMu.Unlock() }
	getExecutor := func() *executor.Executor { exMu.Lock(); defer exMu.Unlock(); return curEx }
	setExecutor(ex)

	// Current agent ws, swapped on each (re)connect. The single executor→ws pump
	// below always writes to whatever conn is current.
	var mu sync.Mutex
	var cur *websocket.Conn
	var connWriteMu sync.Mutex
	setConn := func(c *websocket.Conn) { mu.Lock(); cur = c; mu.Unlock() }
	getConn := func() *websocket.Conn { mu.Lock(); defer mu.Unlock(); return cur }
	writeRelay := func(raw []byte) {
		if c := getConn(); c != nil {
			connWriteMu.Lock()
			_ = c.Write(ctx, websocket.MessageText, raw)
			connWriteMu.Unlock()
		}
	}
	replays := newRequestReplay()

	// executor stdout → current relay ws, for one executor instance. Best-
	// effort: events emitted during a brief reconnect gap are dropped rather
	// than tearing down the bridge. Returns once e's events channel closes,
	// i.e. e's process exited.
	pumpEvents := func(e *executor.Executor) {
		for ev := range e.Events() {
			if len(ev.Raw) == 0 {
				continue
			}
			if ev.Type == "done" || ev.Type == "error" || ev.Type == "aborted" {
				log.Printf("응답 완료(%s) → relay 로 전송", ev.Type)
			}
			replays.Observe(ev.Raw, ev.Type == "done" || ev.Type == "error" || ev.Type == "aborted")
			writeRelay(ev.Raw)
		}
	}

	// exDone closes once the executor supervisor stops for good: either a
	// clean shutdown (ctx done) or a fatal crash-loop escalation (fatalCh
	// carries the error in the latter case).
	exDone := make(chan struct{})
	fatalCh := make(chan error, 1)
	go func() {
		defer close(exDone)
		superviseExecutor(ctx, ex, executorPath, workingDir, runtimeEnv, claudeOAuthToken, pumpEvents, setExecutor, replays.ResetActive, fatalCh)
	}()

	defer func() {
		// Ask the currently-supervised instance to exit (mirrors the original
		// single-executor defer ex.Close()), then wait for the supervisor
		// goroutine to fully stop so no goroutine/process outlives Run.
		if e := getExecutor(); e != nil {
			_ = e.Close()
		}
		<-exDone
	}()

	backoff := reconnectBase
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c, resp, derr := websocket.Dial(ctx, relayURL, dialOpts)
		if derr != nil {
			// Auth rejection at the handshake is terminal: retrying with the same
			// token can never succeed, so stop and let the caller prompt re-login
			// instead of spinning a silent reconnect storm.
			if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
				return fmt.Errorf("%w (HTTP %d)", ErrUnauthorized, resp.StatusCode)
			}
			reportRelayState(onRelayState, "reconnecting")
			// Surface WHY we can't connect instead of spinning silently — a
			// stuck-looking `kroot chat start` is almost always an unreachable or
			// misconfigured relay (DNS, TLS trust, proxy 404, relay down).
			delay := jitteredDelay(backoff)
			log.Printf("relay 연결 실패: %s — %v (%s 후 재시도)", relayURL, derr, delay)
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
		// coder/websocket 기본 32KiB 읽기 한도 해제 — 첨부 포함 chat 메시지 등
		// relay→agent 방향도 커질 수 있고, 한도 초과는 연결을 즉사시켜
		// 1초 재접속 폭풍을 만든다.
		c.SetReadLimit(16 << 20)
		setConn(c)
		reportRelayState(onRelayState, "connected")
		log.Printf("relay 연결됨: %s", relayURL)
		backoff = reconnectBase // reset after a successful connect

		// relay ws → executor stdin, until the ws drops. Reads happen on a
		// helper goroutine so this select can also react to fatalCh while the
		// ws is idle — a plain blocking c.Read(ctx) would hide a crash-loop
		// escalation until the connection happened to drop on its own, which
		// could be never for an idle-but-healthy relay session. A write
		// failure just means the CURRENT executor instance is between
		// processes (dead or restarting) — that self-heals via
		// superviseExecutor, so it no longer tears down the ws bridge; the
		// message is dropped, best-effort, like a brief reconnect gap.
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
				if peekMessageType(m.data) == "chat" {
					log.Printf("chat 요청 수신 → executor 로 전달")
					if duplicate, frames := replays.Begin(m.data); duplicate {
						for _, frame := range frames {
							writeRelay(frame)
						}
						continue
					}
				}
				if e := getExecutor(); e != nil {
					if werr := e.WriteRaw(m.data); werr != nil {
						log.Printf("executor 전달 실패(재시작 중일 수 있음): %v", werr)
					}
				} else {
					log.Printf("executor 재시작 중 — 메시지를 건너뜁니다")
				}
			}
		}
		close(connDone) // unblocks the read goroutine's pending/next send, if any
		setConn(nil)
		reportRelayState(onRelayState, "reconnecting")
		_ = c.Close(websocket.StatusNormalClosure, "")
		if earlyExit != nil {
			return earlyExit
		}
		if err := ctx.Err(); err != nil {
			log.Printf("relay 세션 종료: %s", relayURL)
			return err
		}
		delay := jitteredDelay(backoff)
		log.Printf("relay 연결 끊김: %s (%s 후 재접속)", relayURL, delay)

		// Reconnect unless we're shutting down or the executor has crash-looped
		// past its restart budget.
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

func reportRelayState(callback func(string), state string) {
	if callback != nil {
		callback(state)
	}
}

// superviseExecutor bridges the initial executor instance via pump, and on
// unexpected exit (pump returns while ctx is still live) restarts it: fresh
// process, rewired via setCurrent so the ws→executor write path always sees
// the live instance (or nil during the brief respawn gap). Restarts use the
// same jittered backoff progression as the ws reconnect loop. If
// maxChildRestarts happen within childRestartWindow, it gives up and sends the
// escalation error on fatalCh — a genuine crash-loop must still surface and
// end the daemon, but a one-off crash self-heals silently. Returns when ctx is
// done or after sending to fatalCh.
func superviseExecutor(ctx context.Context, first *executor.Executor, executorPath, workingDir string, environment map[string]string, claudeOAuthToken string, pump func(*executor.Executor), setCurrent func(*executor.Executor), resetActive func(), fatalCh chan<- error) {
	tracker := &childRestartTracker{}
	e := first
	backoff := reconnectBase
	for {
		pump(e)
		if ctx.Err() != nil {
			return
		}
		resetActive()
		setCurrent(nil)
		for {
			if !tracker.allow(time.Now()) {
				fatalCh <- fmt.Errorf("executor 프로세스가 반복적으로 종료되었습니다 (%d회/%s 이내) — kroot chat start 를 다시 실행하세요", maxChildRestarts, childRestartWindow)
				return
			}
			if tracker.windowReset {
				backoff = reconnectBase
			}
			delay := jitteredDelay(backoff)
			log.Printf("executor 프로세스 종료 감지 — %s 후 재시작합니다 (%d/%d)", delay, tracker.restarts, maxChildRestarts)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			backoff = nextBackoff(backoff)
			if ctx.Err() != nil {
				return
			}
			ne, serr := executor.StartWithOptions(ctx, "", executorPath, executor.StartOptions{WorkingDir: workingDir, Environment: environment, ClaudeOAuthToken: claudeOAuthToken})
			if serr != nil {
				log.Printf("executor 재시작 실패: %v (다시 시도)", serr)
				continue
			}
			e = ne
			break
		}
		setCurrent(e)
	}
}

func acpRuntimeEnvironment(executorPath, agentID string) map[string]string {
	if agentID != "codex" && !strings.HasPrefix(agentID, "codex:") {
		return nil
	}
	adapter := filepath.Join(filepath.Dir(executorPath), "codex-acp-adapter.mjs")
	args, _ := json.Marshal([]string{adapter})
	return map[string]string{
		"PIE_ACP_AGENT_COMMAND":   "node",
		"PIE_ACP_AGENT_ARGS_JSON": string(args),
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
