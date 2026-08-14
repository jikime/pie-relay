package chatgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestJournalPersistsSequenceAndIdempotency(t *testing.T) {
	root := t.TempDir()
	journal, err := NewJournal(root, 1<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	first, err := journal.Append(context.Background(), "conversation-a", "request.accepted", "request-a", json.RawMessage(`{"prompt":"hello"}`))
	if err != nil || first.Sequence != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err := journal.Append(context.Background(), "conversation-a", "request.accepted", "request-a", json.RawMessage(`{}`)); !errors.Is(err, ErrDuplicateRequest) {
		t.Fatalf("duplicate err=%v", err)
	}
	if _, err := journal.Append(context.Background(), "conversation-a", "text", "", json.RawMessage(`{"text":"world"}`)); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewJournal(root, 1<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := reloaded.HasRequest("conversation-a", "request-a")
	if err != nil || !duplicate {
		t.Fatalf("persisted duplicate=%t err=%v", duplicate, err)
	}
	last, err := reloaded.LastSequence("conversation-a")
	if err != nil || last != 2 {
		t.Fatalf("last=%d err=%v", last, err)
	}
	events, err := reloaded.Events(context.Background(), "conversation-a", 1, 10)
	if err != nil || len(events) != 1 || events[0].Type != "text" || events[0].Sequence != 2 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestJournalFindsDurableControlResponseByRequest(t *testing.T) {
	journal, err := NewJournal(t.TempDir(), 1<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), "conversation-a", "control.accepted", "workspace-a", json.RawMessage(`{"type":"workspace"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), "conversation-a", "workspace_result", "workspace-a", json.RawMessage(`{"type":"workspace_result","ok":true}`)); err != nil {
		t.Fatal(err)
	}
	event, found, err := journal.EventByRequestType(context.Background(), "conversation-a", "workspace-a", "workspace_result")
	if err != nil || !found || event.RequestID != "workspace-a" || event.Type != "workspace_result" {
		t.Fatalf("event=%+v found=%t err=%v", event, found, err)
	}
}

func TestJournalConcurrentAppendHasStrictSequence(t *testing.T) {
	journal, err := NewJournal(t.TempDir(), 8<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	const count = 64
	var group sync.WaitGroup
	errorsCh := make(chan error, count)
	for index := range count {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, err := journal.Append(context.Background(), "conversation-a", "text", "", json.RawMessage(fmt.Sprintf(`{"index":%d}`, index)))
			errorsCh <- err
		}(index)
	}
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := journal.Events(context.Background(), "conversation-a", 0, count)
	if err != nil || len(events) != count {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	for index, event := range events {
		if event.Sequence != uint64(index+1) {
			t.Fatalf("sequence[%d]=%d", index, event.Sequence)
		}
	}
}

func TestJournalEnforcesLimitsAndSafeIDs(t *testing.T) {
	journal, err := NewJournal(t.TempDir(), 1100, 32)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), "../escape", "text", "", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unsafe conversation ID was accepted")
	}
	if _, err := journal.Append(context.Background(), "safe", "text", "", make([]byte, 33)); err == nil {
		t.Fatal("oversized event was accepted")
	}
	if _, err := journal.Append(context.Background(), "safe", "text", "", json.RawMessage(`{"value":"one"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), "safe", "text", "", json.RawMessage(`{"value":"two"}`)); !errors.Is(err, ErrJournalFull) {
		t.Fatalf("journal limit err=%v", err)
	}
}

func TestJournalRecoversPendingRequestAndRemovesConversation(t *testing.T) {
	root := t.TempDir()
	journal, err := NewJournal(root, 1<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"type":"chat","prompt":"recover me"}`)
	if _, err := journal.Append(context.Background(), "conversation-a", "request.accepted", "request-a", payload); err != nil {
		t.Fatal(err)
	}
	pending, ok, err := journal.PendingChatRequest("conversation-a")
	if err != nil || !ok || pending.RequestID != "request-a" || string(pending.Data) != string(payload) {
		t.Fatalf("pending=%+v ok=%t err=%v", pending, ok, err)
	}
	if _, err := journal.Append(context.Background(), "conversation-a", "request.completed", "request-a", json.RawMessage(`{"terminalType":"done"}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := journal.PendingChatRequest("conversation-a"); err != nil || ok {
		t.Fatalf("completed request still pending: ok=%t err=%v", ok, err)
	}
	if duplicate, err := journal.HasRequest("conversation-a", "request-a"); err != nil || !duplicate {
		t.Fatalf("completion lost idempotency: duplicate=%t err=%v", duplicate, err)
	}
	if err := journal.Remove("conversation-a"); err != nil {
		t.Fatal(err)
	}
	events, err := journal.Events(context.Background(), "conversation-a", 0, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("removed events=%+v err=%v", events, err)
	}
	if err := journal.Remove("conversation-a"); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
}
