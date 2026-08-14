package ptyagent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type fakePTYWriteConn struct {
	mu      sync.Mutex
	writes  [][]byte
	closed  bool
	blocked chan struct{}
}

func (f *fakePTYWriteConn) Write(ctx context.Context, _ websocket.MessageType, msg []byte) error {
	if f.blocked != nil {
		select {
		case <-f.blocked:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	f.writes = append(f.writes, append([]byte(nil), msg...))
	f.mu.Unlock()
	return nil
}

func (f *fakePTYWriteConn) Close(websocket.StatusCode, string) error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func TestBoundedWSWriterPreservesOrder(t *testing.T) {
	f := &fakePTYWriteConn{}
	w := newBoundedWSWriter(context.Background(), f)
	defer w.Stop()
	for _, msg := range []string{"a", "b", "c"} {
		if err := w.Send([]byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	waitUntil(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.writes) == 3
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if string(f.writes[0]) != "a" || string(f.writes[1]) != "b" || string(f.writes[2]) != "c" {
		t.Fatalf("writes out of order: %q", f.writes)
	}
}

func TestBoundedWSWriterByteOverflowClosesConnection(t *testing.T) {
	f := &fakePTYWriteConn{blocked: make(chan struct{})}
	w := newBoundedWSWriter(context.Background(), f)
	if err := w.Send(make([]byte, ptyWriteQueueBytes+1)); err != errPTYWriterClosed {
		t.Fatalf("Send = %v, want errPTYWriterClosed", err)
	}
	waitUntil(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.closed
	})
}

func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met")
}
