package usage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
)

var ErrUnavailable = errors.New("LLM usage ledger is unavailable")

type Service struct {
	store *PostgresStore
	now   func() time.Time
}

func NewService(store *PostgresStore) *Service {
	return &Service{store: store, now: time.Now}
}

func (s *Service) Enabled() bool { return s != nil && s.store != nil }

func (s *Service) RecordUsage(ctx context.Context, conversation control.Conversation, requestID string, raw json.RawMessage) error {
	return s.RecordUsageAt(ctx, conversation, requestID, raw, s.now().UTC())
}

// RecordUsageAt is the replay-safe variant used by the journal reconciler.
// Keeping the original Manager receipt time makes daily attribution and the
// selected price version stable even when PostgreSQL recovers much later.
func (s *Service) RecordUsageAt(ctx context.Context, conversation control.Conversation, requestID string, raw json.RawMessage, receivedAt time.Time) error {
	if !s.Enabled() {
		return ErrUnavailable
	}
	if receivedAt.IsZero() {
		return ErrInvalidEvent
	}
	var event Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return ErrInvalidEvent
	}
	attribution := Attribution{
		IntegrationID: conversation.IntegrationID, IntegrationUserID: conversation.IntegrationUserID,
		OwnerUserID: conversation.OwnerUserID, ProjectID: conversation.ProjectID,
		ConversationID: conversation.ID, RequestID: requestID, AgentSessionID: conversation.AgentSessionID,
		ReceivedAt: receivedAt.UTC(),
	}
	return s.store.Record(ctx, attribution, event, raw)
}

func (s *Service) Summary(ctx context.Context, integrationID, integrationUserID string, from, to time.Time) (Summary, error) {
	if !s.Enabled() {
		return Summary{}, ErrUnavailable
	}
	return s.store.Summary(ctx, integrationID, integrationUserID, from, to)
}

func (s *Service) List(ctx context.Context, integrationID, integrationUserID string, from, to time.Time, limit int, cursor string) (ListPage, error) {
	if !s.Enabled() {
		return ListPage{}, ErrUnavailable
	}
	if limit == 0 {
		limit = 30
	}
	if limit < 1 || limit > 100 {
		return ListPage{}, errors.New("usage list limit must be between 1 and 100")
	}
	before, err := decodeListCursor(cursor)
	if err != nil {
		return ListPage{}, err
	}
	items, hasMore, err := s.store.List(ctx, integrationID, integrationUserID, from, to, limit, before)
	if err != nil {
		return ListPage{}, err
	}
	page := ListPage{Items: items}
	if hasMore && len(items) > 0 {
		page.NextCursor = encodeListCursor(items[len(items)-1])
	}
	return page, nil
}

func (s *Service) Ping(ctx context.Context) error {
	if !s.Enabled() {
		return ErrUnavailable
	}
	return s.store.Ping(ctx)
}

func (s *Service) Close() error {
	if !s.Enabled() {
		return nil
	}
	return s.store.Close()
}
