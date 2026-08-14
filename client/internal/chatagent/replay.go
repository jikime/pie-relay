package chatagent

import (
	"encoding/json"
	"sync"
)

const (
	maxReplayRequests = 64
	maxReplayBytes    = 16 << 20
	maxReplayTotal    = 32 << 20
)

// requestReplay prevents a durable Gateway retry from executing the same
// Claude turn twice while clientd is still alive.  It also retains a bounded
// response stream so a freshly reconnected Gateway can recover events that
// were emitted while its WebSocket was unavailable.
type requestReplay struct {
	mu              sync.Mutex
	activeID        string
	activeFrames    [][]byte
	activeBytes     int
	activeTruncated bool
	completed       map[string][][]byte
	completedOrder  []string
	completedBytes  int
}

func newRequestReplay() *requestReplay {
	return &requestReplay{completed: map[string][][]byte{}}
}

// Begin reports whether the chat request has already been accepted.  When it
// has, frames contains the safely replayable response prefix (or the complete
// response for a finished request).
func (r *requestReplay) Begin(raw []byte) (bool, [][]byte) {
	requestID := chatRequestID(raw)
	if requestID == "" {
		return false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if frames, ok := r.completed[requestID]; ok {
		return true, cloneFrames(frames)
	}
	if r.activeID == requestID {
		return true, cloneFrames(r.activeFrames)
	}
	// The public Gateway allows one active turn. Preserve legacy behavior for
	// other callers that send overlapping distinct IDs instead of silently
	// dropping their request, but do not replace the tracked recovery turn.
	if r.activeID != "" {
		return false, nil
	}
	r.activeID = requestID
	r.activeFrames = nil
	r.activeBytes = 0
	r.activeTruncated = false
	return false, nil
}

func (r *requestReplay) Observe(raw []byte, terminal bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeID == "" {
		return
	}
	if !r.activeTruncated && r.activeBytes+len(raw) <= maxReplayBytes {
		r.activeFrames = append(r.activeFrames, append([]byte(nil), raw...))
		r.activeBytes += len(raw)
	} else {
		r.activeTruncated = true
	}
	if !terminal {
		return
	}
	// Even after a very large streamed response, retain the terminal frame so
	// the Gateway can release its active-turn lock on a retry.
	if r.activeTruncated {
		r.activeFrames = [][]byte{append([]byte(nil), raw...)}
	}
	r.completed[r.activeID] = cloneFrames(r.activeFrames)
	r.completedBytes += framesSize(r.activeFrames)
	r.completedOrder = append(r.completedOrder, r.activeID)
	for len(r.completedOrder) > maxReplayRequests || r.completedBytes > maxReplayTotal {
		oldest := r.completedOrder[0]
		r.completedBytes -= framesSize(r.completed[oldest])
		delete(r.completed, oldest)
		r.completedOrder = r.completedOrder[1:]
	}
	r.activeID = ""
	r.activeFrames = nil
	r.activeBytes = 0
	r.activeTruncated = false
}

// ResetActive is called when the executor child exits before a terminal event.
// A later durable retry must be allowed to start a fresh child execution.
func (r *requestReplay) ResetActive() {
	r.mu.Lock()
	r.activeID = ""
	r.activeFrames = nil
	r.activeBytes = 0
	r.activeTruncated = false
	r.mu.Unlock()
}

func chatRequestID(raw []byte) string {
	var value struct {
		Type      string `json:"type"`
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Type != "chat" || value.RequestID == "" || len(value.RequestID) > 160 {
		return ""
	}
	return value.RequestID
}

func cloneFrames(values [][]byte) [][]byte {
	out := make([][]byte, len(values))
	for index, value := range values {
		out[index] = append([]byte(nil), value...)
	}
	return out
}

func framesSize(values [][]byte) int {
	total := 0
	for _, value := range values {
		total += len(value)
	}
	return total
}
