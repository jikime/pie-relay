package ptyagent

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	ptyWriteQueueMessages = 256
	ptyWriteQueueBytes    = 16 << 20
	ptyWriteTimeout       = 10 * time.Second
)

var errPTYWriterClosed = errors.New("pty relay writer closed")

type ptyWriteConn interface {
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
}

// boundedWSWriter isolates pty-host stdout from network latency. Its queue is
// bounded by both messages and bytes, and every actual socket write has a
// deadline. Overflow closes only the relay connection, which makes Run redial
// while the supervised PTY process stays alive and later serves a snapshot.
type boundedWSWriter struct {
	ctx  context.Context
	conn ptyWriteConn
	ch   chan []byte
	done chan struct{}
	once sync.Once

	mu     sync.Mutex
	bytes  int
	closed bool
}

func newBoundedWSWriter(ctx context.Context, conn ptyWriteConn) *boundedWSWriter {
	w := &boundedWSWriter{
		ctx: ctx, conn: conn, ch: make(chan []byte, ptyWriteQueueMessages), done: make(chan struct{}),
	}
	go w.loop()
	return w
}

func (w *boundedWSWriter) Send(msg []byte) error {
	copyOfMsg := append([]byte(nil), msg...)
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return errPTYWriterClosed
	}
	if w.bytes+len(copyOfMsg) > ptyWriteQueueBytes {
		w.mu.Unlock()
		w.fail("pty relay write queue byte limit")
		return errPTYWriterClosed
	}
	w.bytes += len(copyOfMsg)
	w.mu.Unlock()

	select {
	case w.ch <- copyOfMsg:
		return nil
	case <-w.done:
		w.release(len(copyOfMsg))
		return errPTYWriterClosed
	default:
		w.release(len(copyOfMsg))
		w.fail("pty relay write queue message limit")
		return errPTYWriterClosed
	}
}

func (w *boundedWSWriter) release(n int) {
	w.mu.Lock()
	w.bytes -= n
	if w.bytes < 0 {
		w.bytes = 0
	}
	w.mu.Unlock()
}

func (w *boundedWSWriter) loop() {
	for {
		select {
		case msg := <-w.ch:
			writeCtx, cancel := context.WithTimeout(w.ctx, ptyWriteTimeout)
			err := w.conn.Write(writeCtx, websocket.MessageText, msg)
			cancel()
			w.release(len(msg))
			if err != nil {
				w.fail("pty relay write failed")
				return
			}
		case <-w.done:
			return
		case <-w.ctx.Done():
			w.Stop()
			return
		}
	}
}

func (w *boundedWSWriter) fail(reason string) {
	w.once.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		close(w.done)
		_ = w.conn.Close(websocket.StatusPolicyViolation, reason)
	})
}

// Stop ends the writer without closing the socket; Run owns normal socket
// close ordering. Failure/overflow paths use fail and do close it to unblock
// the reader immediately.
func (w *boundedWSWriter) Stop() {
	w.once.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		close(w.done)
	})
}
