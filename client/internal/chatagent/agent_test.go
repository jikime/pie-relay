package chatagent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestNextBackoff_DoublesAndCaps pins nextBackoff's exact, deterministic
// doubling — jitter (see jitteredDelay) is applied only at the sleep call
// sites, so the stored accumulator itself must stay predictable.
func TestNextBackoff_DoublesAndCaps(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{1 * time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{4 * time.Second, 8 * time.Second},
		{8 * time.Second, 16 * time.Second},
		{16 * time.Second, 30 * time.Second}, // capped
		{30 * time.Second, 30 * time.Second}, // stays capped
	}
	for _, c := range cases {
		if got := nextBackoff(c.in); got != c.want {
			t.Errorf("nextBackoff(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestJitteredDelay_Bounds verifies the equal-jitter contract: the returned
// sleep duration always falls in [backoff/2, backoff], so many daemons
// reconnecting after the same event (e.g. a relay restart) spread their
// re-dials instead of retrying in lockstep.
func TestJitteredDelay_Bounds(t *testing.T) {
	if got := jitteredDelay(0); got != 0 {
		t.Errorf("jitteredDelay(0) = %s, want 0", got)
	}
	for _, backoff := range []time.Duration{1 * time.Second, 2 * time.Second, 30 * time.Second, 7 * time.Millisecond} {
		half := backoff / 2
		seenNonHalf := false
		for i := 0; i < 200; i++ {
			d := jitteredDelay(backoff)
			if d < half || d > backoff {
				t.Fatalf("jitteredDelay(%s) = %s, want in [%s, %s]", backoff, d, half, backoff)
			}
			if d != half {
				seenNonHalf = true
			}
		}
		if !seenNonHalf {
			t.Errorf("jitteredDelay(%s) never varied across 200 samples — jitter looks broken", backoff)
		}
	}
}

// TestChildRestartTracker_BudgetThenWindowReset verifies the crash-loop guard:
// up to maxChildRestarts attempts are allowed inside childRestartWindow, the
// next one is refused (escalation), and a rollover to a fresh window (after a
// quiet period longer than childRestartWindow) allows attempts again.
func TestChildRestartTracker_BudgetThenWindowReset(t *testing.T) {
	origMax, origWindow := maxChildRestarts, childRestartWindow
	maxChildRestarts = 3
	childRestartWindow = time.Minute
	defer func() { maxChildRestarts, childRestartWindow = origMax, origWindow }()

	tr := &childRestartTracker{}
	base := time.Now()

	for i := 1; i <= maxChildRestarts; i++ {
		if !tr.allow(base.Add(time.Duration(i) * time.Second)) {
			t.Fatalf("attempt %d: expected allow, got refused", i)
		}
	}
	// One more within the same window must be refused (crash-loop budget
	// exhausted) — this is what triggers fatal escalation in superviseExecutor.
	if tr.allow(base.Add(time.Duration(maxChildRestarts+1) * time.Second)) {
		t.Fatal("expected the attempt past maxChildRestarts to be refused")
	}

	// A long quiet period rolls the window over and resets the budget. The
	// window's actual start is base+1s (the first allow() call above), so
	// measure the gap from there, not from base itself.
	later := base.Add(time.Second).Add(childRestartWindow).Add(time.Second)
	if !tr.allow(later) {
		t.Fatal("expected allow after the restart window rolled over")
	}
	if !tr.windowReset {
		t.Fatal("expected windowReset to be true right after a rollover")
	}
}

// --- integration tests: real `node` child, fake relay ws server ---

// crashChildScript is a minimal Node "child" used to simulate a supervised
// process that dies mid-session. On each launch it reads a counter from
// CRASH_COUNTER_FILE; while that counter is below CRASH_TIMES it increments
// the file and exits(1) immediately (simulating a crash); once the budget is
// exhausted it just stays alive, consuming stdin, until killed — modeling a
// child that recovers after N crashes.
const crashChildScript = `
import fs from 'node:fs';
const counterFile = process.env.CRASH_COUNTER_FILE;
const crashTimes = parseInt(process.env.CRASH_TIMES || '0', 10);
let n = 0;
try {
  n = parseInt(fs.readFileSync(counterFile, 'utf8'), 10);
  if (Number.isNaN(n)) n = 0;
} catch (e) {}
if (n < crashTimes) {
  fs.writeFileSync(counterFile, String(n + 1));
  process.exit(1);
}
process.stdin.resume();
process.stdin.on('end', () => process.exit(0));
setInterval(() => {}, 1 << 30);
`

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available in PATH — skipping integration test")
	}
}

// useFastChildTimings shrinks the reconnect/restart timing knobs (package
// vars, not consts, specifically so tests can do this) so restart/escalation
// integration tests run in well under a second instead of tens of seconds of
// real backoff.
func useFastChildTimings(t *testing.T) {
	t.Helper()
	origBase, origMax, origMaxRestarts, origWindow := reconnectBase, reconnectMax, maxChildRestarts, childRestartWindow
	reconnectBase = 20 * time.Millisecond
	reconnectMax = 80 * time.Millisecond
	maxChildRestarts = 3
	childRestartWindow = 2 * time.Second
	t.Cleanup(func() {
		reconnectBase, reconnectMax, maxChildRestarts, childRestartWindow = origBase, origMax, origMaxRestarts, origWindow
	})
}

// writeCrashChildScript writes crashChildScript to a temp file and returns its
// path.
func writeCrashChildScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "crash-child.mjs")
	if err := os.WriteFile(p, []byte(crashChildScript), 0o644); err != nil {
		t.Fatalf("write crash child script: %v", err)
	}
	return p
}

// startFakeRelay runs a local ws server that accepts the agent connection and
// then just idles (reads and discards), like a relay with nothing to say —
// exactly the case that would previously have hidden a fatal escalation
// behind a blocking c.Read.
func startFakeRelay(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// TestRun_ExecutorCrash_RestartsInsteadOfExiting proves Fix 2: a mid-session
// executor crash must NOT make Run return (which previously propagated to
// main.go's log.Fatalf and permanently killed the daemon) — it should be
// restarted in place while the relay session keeps running.
func TestRun_ExecutorCrash_RestartsInsteadOfExiting(t *testing.T) {
	requireNode(t)
	useFastChildTimings(t)

	scriptPath := writeCrashChildScript(t)
	counterFile := filepath.Join(t.TempDir(), "counter")
	t.Setenv("CRASH_COUNTER_FILE", counterFile)
	t.Setenv("CRASH_TIMES", "1") // crash exactly once, then recover

	relayURL := startFakeRelay(t)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, relayURL, scriptPath, "") }()

	// Give it time to: spawn, crash, back off, and respawn.
	select {
	case err := <-errCh:
		cancel()
		t.Fatalf("Run returned early (expected the crash to self-heal via restart): %v", err)
	case <-time.After(1 * time.Second):
	}

	b, err := os.ReadFile(counterFile)
	if err != nil {
		t.Fatalf("counter file was never written — the child never crashed/restarted: %v", err)
	}
	if strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("expected exactly one recorded crash, got %q", string(b))
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("expected clean shutdown after ctx cancel, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

// TestRun_ExecutorCrashLoop_EscalatesToFatal proves the other half of Fix 2:
// a genuine crash-loop (the child dies immediately every single time) must
// still surface as a fatal error once maxChildRestarts is exhausted, instead
// of restarting forever in a silent, invisible loop.
func TestRun_ExecutorCrashLoop_EscalatesToFatal(t *testing.T) {
	requireNode(t)
	useFastChildTimings(t)

	scriptPath := writeCrashChildScript(t)
	counterFile := filepath.Join(t.TempDir(), "counter")
	t.Setenv("CRASH_COUNTER_FILE", counterFile)
	t.Setenv("CRASH_TIMES", "1000") // always crash

	relayURL := startFakeRelay(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, relayURL, scriptPath, "") }()

	select {
	case err := <-errCh:
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("expected a fatal crash-loop escalation error, got %v", err)
		}
		t.Logf("escalated as expected: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not escalate to a fatal error within the restart budget")
	}
}
