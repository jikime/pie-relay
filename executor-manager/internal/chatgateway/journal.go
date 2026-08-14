package chatgateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Sequence       uint64          `json:"sequence"`
	ConversationID string          `json:"conversationId"`
	Type           string          `json:"type"`
	RequestID      string          `json:"requestId,omitempty"`
	Data           json.RawMessage `json:"data,omitempty"`
	At             time.Time       `json:"at"`
}

// PendingRequest is a chat request that was durably accepted but for which no
// terminal executor event has been durably recorded yet.  The payload is kept
// in the journal so a Manager restart can safely resume delivery.
type PendingRequest struct {
	RequestID string
	Data      json.RawMessage
	Sequence  uint64
}

type Journal struct {
	root       string
	maxBytes   int64
	maxEvent   int64
	mu         sync.Mutex
	sequences  map[string]uint64
	requestIDs map[string]map[string]struct{}
}

func NewJournal(root string, maxBytes, maxEventBytes int64) (*Journal, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("chat journal root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	if maxEventBytes <= 0 {
		maxEventBytes = 8 << 20
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	return &Journal{root: absolute, maxBytes: maxBytes, maxEvent: maxEventBytes, sequences: map[string]uint64{}, requestIDs: map[string]map[string]struct{}{}}, nil
}

func (j *Journal) Append(ctx context.Context, conversationID, eventType, requestID string, data json.RawMessage) (Event, error) {
	if !safeJournalID(conversationID) || eventType == "" || len(eventType) > 96 || int64(len(data)) > j.maxEvent {
		return Event{}, errors.New("invalid chat event")
	}
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.loadIndexLocked(conversationID); err != nil {
		return Event{}, err
	}
	if requestID != "" && isIdempotencyEvent(eventType) {
		if _, duplicate := j.requestIDs[conversationID][requestID]; duplicate {
			return Event{}, ErrDuplicateRequest
		}
	}
	path := j.path(conversationID)
	// Completion markers are tiny recovery metadata.  Always allow them even
	// when the user-visible event budget has just been exhausted; otherwise a
	// completed turn could remain permanently locked as "active".
	if info, err := os.Stat(path); err == nil && eventType != "request.completed" && info.Size()+int64(len(data))+1024 > j.maxBytes {
		return Event{}, ErrJournalFull
	}
	event := Event{Sequence: j.sequences[conversationID] + 1, ConversationID: conversationID, Type: eventType, RequestID: requestID, Data: append(json.RawMessage(nil), data...), At: time.Now().UTC()}
	encoded, err := json.Marshal(event)
	if err != nil {
		return Event{}, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return Event{}, err
	}
	if _, err = file.Write(append(encoded, '\n')); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return Event{}, err
	}
	j.sequences[conversationID] = event.Sequence
	if requestID != "" && isIdempotencyEvent(eventType) {
		j.requestIDs[conversationID][requestID] = struct{}{}
	}
	return event, nil
}

// PendingChatRequest returns the single unfinished chat request, if any.  A
// conversation is deliberately single-turn-at-a-time, so multiple unfinished
// requests indicate a corrupt or manually modified journal and are rejected.
func (j *Journal) PendingChatRequest(conversationID string) (PendingRequest, bool, error) {
	if !safeJournalID(conversationID) {
		return PendingRequest{}, false, errors.New("invalid conversation id")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	file, err := os.Open(j.path(conversationID))
	if errors.Is(err, os.ErrNotExist) {
		return PendingRequest{}, false, nil
	}
	if err != nil {
		return PendingRequest{}, false, err
	}
	defer file.Close()
	pending := map[string]PendingRequest{}
	decoder := json.NewDecoder(file)
	for {
		var event Event
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return PendingRequest{}, false, fmt.Errorf("decode chat journal: %w", err)
		}
		switch event.Type {
		case "request.accepted":
			pending[event.RequestID] = PendingRequest{RequestID: event.RequestID, Data: append(json.RawMessage(nil), event.Data...), Sequence: event.Sequence}
		case "request.completed":
			delete(pending, event.RequestID)
		}
	}
	if len(pending) == 0 {
		return PendingRequest{}, false, nil
	}
	if len(pending) != 1 {
		return PendingRequest{}, false, errors.New("chat journal contains multiple unfinished requests")
	}
	for _, value := range pending {
		return value, true, nil
	}
	panic("unreachable")
}

// Remove permanently deletes a conversation's event journal and in-memory
// indexes.  It is idempotent and is used by the privacy/lifecycle delete path.
func (j *Journal) Remove(conversationID string) error {
	if !safeJournalID(conversationID) {
		return errors.New("invalid conversation id")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	err := os.Remove(j.path(conversationID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(j.sequences, conversationID)
	delete(j.requestIDs, conversationID)
	return nil
}

func (j *Journal) Events(ctx context.Context, conversationID string, after uint64, limit int) ([]Event, error) {
	if !safeJournalID(conversationID) {
		return nil, errors.New("invalid conversation id")
	}
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	file, err := os.Open(j.path(conversationID))
	if errors.Is(err, os.ErrNotExist) {
		return []Event{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), int(j.maxEvent+1024))
	values := make([]Event, 0, min(limit, 64))
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("decode chat journal: %w", err)
		}
		if event.Sequence > after {
			values = append(values, event)
			if len(values) == limit {
				break
			}
		}
	}
	return values, scanner.Err()
}

func (j *Journal) HasRequest(conversationID, requestID string) (bool, error) {
	if requestID == "" {
		return false, nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.loadIndexLocked(conversationID); err != nil {
		return false, err
	}
	_, ok := j.requestIDs[conversationID][requestID]
	return ok, nil
}

func (j *Journal) LastSequence(conversationID string) (uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.loadIndexLocked(conversationID); err != nil {
		return 0, err
	}
	return j.sequences[conversationID], nil
}

// EventByRequestType finds a durable response for an idempotent control
// request. It deliberately scans the journal instead of relying on the latest
// page so a retried HTTP request can recover the original result even after
// many chat events were appended.
func (j *Journal) EventByRequestType(ctx context.Context, conversationID, requestID, eventType string) (Event, bool, error) {
	if !safeJournalID(conversationID) || requestID == "" || eventType == "" {
		return Event{}, false, errors.New("invalid chat event lookup")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	file, err := os.Open(j.path(conversationID))
	if errors.Is(err, os.ErrNotExist) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	for {
		if err := ctx.Err(); err != nil {
			return Event{}, false, err
		}
		var event Event
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			return Event{}, false, nil
		} else if err != nil {
			return Event{}, false, fmt.Errorf("decode chat journal: %w", err)
		}
		if event.RequestID == requestID && event.Type == eventType {
			return event, true, nil
		}
	}
}

func (j *Journal) loadIndexLocked(conversationID string) error {
	if _, ok := j.requestIDs[conversationID]; ok {
		return nil
	}
	j.requestIDs[conversationID] = map[string]struct{}{}
	file, err := os.Open(j.path(conversationID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	for {
		var event Event
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if event.Sequence > j.sequences[conversationID] {
			j.sequences[conversationID] = event.Sequence
		}
		if event.RequestID != "" && isIdempotencyEvent(event.Type) {
			j.requestIDs[conversationID][event.RequestID] = struct{}{}
		}
	}
	return nil
}

func isIdempotencyEvent(eventType string) bool {
	return eventType == "request.accepted" || eventType == "control.accepted"
}

func (j *Journal) path(id string) string { return filepath.Join(j.root, id+".jsonl") }

func safeJournalID(value string) bool {
	if value == "" || len(value) > 160 {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}

var (
	ErrDuplicateRequest = errors.New("duplicate chat request")
	ErrJournalFull      = errors.New("chat event journal is full")
)
