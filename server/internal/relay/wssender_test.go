package relay

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeWSConn is a wsConn test double that lets us simulate a healthy peer
// (Write returns immediately, message recorded) or a stuck/slow peer (Write
// blocks until the test unblocks it, or forever). It never touches a real
// socket, so these tests are fast and deterministic.
type fakeWSConn struct {
	mu     sync.Mutex
	writes [][]byte
	closed bool
	code   websocket.StatusCode
	reason string

	block   bool          // if true, Write blocks until unblock is closed
	unblock chan struct{} // close to release a blocked Write
}

func newFakeWSConn(block bool) *fakeWSConn {
	return &fakeWSConn{block: block, unblock: make(chan struct{})}
}

func (f *fakeWSConn) Write(ctx context.Context, _ websocket.MessageType, p []byte) error {
	if f.block {
		select {
		case <-f.unblock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	f.writes = append(f.writes, append([]byte(nil), p...))
	f.mu.Unlock()
	return nil
}

func (f *fakeWSConn) Close(code websocket.StatusCode, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	f.code = code
	f.reason = reason
	return nil
}

func (f *fakeWSConn) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeWSConn) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakeWSConn) writesSnapshot() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.writes))
	copy(out, f.writes)
	return out
}

// TestWsSender_HealthyDeliversInOrder confirms the redesigned wsSender keeps
// the SAME external behavior as the old synchronous Send for a healthy peer:
// messages arrive, in order, at the underlying connection. A single writer
// goroutine draining sendCh is what preserves ordering.
func TestWsSender_HealthyDeliversInOrder(t *testing.T) {
	fc := newFakeWSConn(false)
	s := newWsSender(context.Background(), fc)

	const n = 50
	for i := 0; i < n; i++ {
		if err := s.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("Send(%d) unexpected error: %v", i, err)
		}
	}

	waitFor(t, func() bool { return fc.writeCount() == n })

	got := fc.writesSnapshot()
	for i, b := range got {
		if len(b) != 1 || b[0] != byte(i) {
			t.Fatalf("message %d out of order/corrupted: got %v, want [%d]", i, b, i)
		}
	}
	if fc.isClosed() {
		t.Fatal("healthy connection should not have been closed")
	}
}

// TestWsSender_OverflowClosesConnWithoutBlocking is the core reliability
// proof: a peer whose underlying socket write never completes (simulating a
// stuck/slow participant) must NOT be able to block the caller of Send. Once
// the bounded queue fills, Send must evict the peer (close the connection)
// and keep returning immediately — never block waiting on the stuck writer.
func TestWsSender_OverflowClosesConnWithoutBlocking(t *testing.T) {
	fc := newFakeWSConn(true) // Write blocks forever (until test ends)
	s := newWsSender(context.Background(), fc)

	start := time.Now()
	var firstErrAt = -1
	// Send well past the queue capacity; every call must return promptly.
	for i := 0; i < sendQueueCap+50; i++ {
		if err := s.Send([]byte{byte(i % 256)}); err != nil {
			if firstErrAt == -1 {
				firstErrAt = i
			}
		}
	}
	elapsed := time.Since(start)

	if firstErrAt == -1 {
		t.Fatal("expected Send to eventually report the connection closed once the queue overflowed")
	}
	// Sanity: overflow should trip at or after the queue capacity is reached
	// (allowing +1 for the message the writer goroutine pulled out to write),
	// not before.
	if firstErrAt < sendQueueCap {
		t.Fatalf("overflow tripped too early at index %d (cap=%d)", firstErrAt, sendQueueCap)
	}
	// The whole burst (way more sends than the buffer holds, against a peer
	// that never drains) must complete quickly — proving Send never blocks
	// the caller on the stuck socket write.
	if elapsed > 2*time.Second {
		t.Fatalf("Send calls took %v, want well under 2s (Send must never block on a stuck peer)", elapsed)
	}

	waitFor(t, fc.isClosed)
	fc.mu.Lock()
	reason := fc.reason
	code := fc.code
	fc.mu.Unlock()
	if code != websocket.StatusPolicyViolation {
		t.Fatalf("close code = %v, want StatusPolicyViolation", code)
	}
	if reason != "send buffer overflow" {
		t.Fatalf("close reason = %q, want %q", reason, "send buffer overflow")
	}

	// Further sends after eviction must keep failing fast, not panic or block.
	if err := s.Send([]byte("after-close")); err != errSenderClosed {
		t.Fatalf("Send after eviction = %v, want errSenderClosed", err)
	}
}

// TestWsSender_StuckPeerDoesNotBlockHealthyPeer mirrors the actual room
// fanout shape (routeFromHost/routeFromParticipant loop over
// ParticipantsFor(room) calling Send once per participant, sequentially, on
// the host's read-loop goroutine). It proves the bug fix directly: fanning
// out the same message to a STUCK participant (Write never returns) and a
// HEALTHY participant in the same sequential loop does not delay the healthy
// participant's delivery — the loop moves past the stuck one immediately
// instead of hanging on its socket write.
func TestWsSender_StuckPeerDoesNotBlockHealthyPeer(t *testing.T) {
	stuckConn := newFakeWSConn(true)
	healthyConn := newFakeWSConn(false)
	stuck := newWsSender(context.Background(), stuckConn)
	healthy := newWsSender(context.Background(), healthyConn)

	room := []Sender{stuck, healthy}

	start := time.Now()
	msg := []byte(`{"type":"chat","from":"host","text":"hello room"}`)
	// Simulate several fanout rounds, as a live room would.
	for i := 0; i < 5; i++ {
		for _, p := range room {
			_ = p.Send(msg)
		}
	}
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("fanout loop took %v, want well under 1s — a stuck participant must not stall delivery to others", elapsed)
	}

	waitFor(t, func() bool { return healthyConn.writeCount() == 5 })

	for _, got := range healthyConn.writesSnapshot() {
		if string(got) != string(msg) {
			t.Fatalf("healthy participant got corrupted message: %s", got)
		}
	}
}

func TestWsSender_ControlOvertakesQueuedOutput(t *testing.T) {
	fc := newFakeWSConn(true)
	s := newWsSender(context.Background(), fc)

	// The first output is pulled by the writer and blocks. The second remains
	// queued, then a control message arrives. Once unblocked, control must be
	// written before the older queued output.
	if err := s.sendClass([]byte("output-1"), trafficOutput); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.output) == 0
	})
	if err := s.sendClass([]byte("output-2"), trafficOutput); err != nil {
		t.Fatal(err)
	}
	if err := s.sendClass([]byte("control"), trafficControl); err != nil {
		t.Fatal(err)
	}
	close(fc.unblock)
	waitFor(t, func() bool { return fc.writeCount() == 3 })

	got := fc.writesSnapshot()
	if string(got[0]) != "output-1" || string(got[1]) != "control" || string(got[2]) != "output-2" {
		t.Fatalf("priority order = %q, %q, %q", got[0], got[1], got[2])
	}
}

func TestWsSender_ByteLimitEvictsSlowPeer(t *testing.T) {
	fc := newFakeWSConn(true)
	s := newWsSender(context.Background(), fc)

	tooLarge := make([]byte, outputQueueByteLimit+1)
	if err := s.sendClass(tooLarge, trafficOutput); err != errSenderClosed {
		t.Fatalf("oversized output Send = %v, want errSenderClosed", err)
	}
	waitFor(t, fc.isClosed)
}

func TestClassifyTraffic(t *testing.T) {
	for _, messageType := range []string{"pty_output", "pty_snapshot", "pty_size", "pty_exit", "done", "error", "aborted"} {
		msg := []byte(`{"type":"` + messageType + `","data":"AA=="}`)
		if got := classifyTraffic(msg); got != trafficOutput {
			t.Fatalf("%s class = %v, want output", messageType, got)
		}
	}
	if got := classifyTraffic([]byte(`{"type":"driver","from":"u"}`)); got != trafficControl {
		t.Fatalf("driver class = %v, want control", got)
	}
}
