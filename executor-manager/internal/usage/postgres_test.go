package usage

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestValidateUsageEvent(t *testing.T) {
	attribution := Attribution{IntegrationID: "integration-a", IntegrationUserID: "binding-a", OwnerUserID: "owner-a", ProjectID: "project-a", ConversationID: "conversation-a", RequestID: "request-a", ReceivedAt: time.Now()}
	event := Event{Type: "usage", SchemaVersion: 1, ResultID: "result-a", QueryRunID: "run-a", ModelUsage: map[string]ModelMeasurement{"claude-test": {InputTokens: 10, OutputTokens: 2, CostUSD: .001}}}
	raw, _ := json.Marshal(event)
	if err := validate(attribution, event, raw); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	event.ModelUsage["claude-test"] = ModelMeasurement{InputTokens: -1}
	if err := validate(attribution, event, raw); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("invalid token count accepted: %v", err)
	}
}

func TestTotalTokensIncludesCacheTraffic(t *testing.T) {
	value := Totals{InputTokens: 10, OutputTokens: 2, CacheReadInputTokens: 30, CacheCreationInputTokens: 4}
	if got := totalTokens(value); got != 46 {
		t.Fatalf("totalTokens=%d", got)
	}
}
