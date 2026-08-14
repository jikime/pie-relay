package usage

import (
	"encoding/json"
	"time"
)

type ModelMeasurement struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	WebSearchRequests        int64   `json:"webSearchRequests"`
	CostUSD                  float64 `json:"costUSD"`
	ContextWindow            int64   `json:"contextWindow"`
	MaxOutputTokens          int64   `json:"maxOutputTokens"`
	CanonicalModel           string  `json:"canonicalModel,omitempty"`
	Provider                 string  `json:"provider,omitempty"`
}

// Event is emitted once by the managed executor for each Agent SDK result.
// Identity fields are intentionally absent: the Manager adds those from its
// authenticated control-plane conversation rather than trusting the client.
type Event struct {
	Type          string                      `json:"type"`
	SchemaVersion int                         `json:"schemaVersion"`
	ResultID      string                      `json:"resultId"`
	QueryRunID    string                      `json:"queryRunId"`
	SessionID     string                      `json:"sessionId"`
	Subtype       string                      `json:"subtype"`
	ReportedAt    time.Time                   `json:"reportedAt"`
	TotalCostUSD  float64                     `json:"totalCostUsd"`
	Usage         json.RawMessage             `json:"usage"`
	ModelUsage    map[string]ModelMeasurement `json:"modelUsage"`
}

type Attribution struct {
	IntegrationID     string
	IntegrationUserID string
	OwnerUserID       string
	ProjectID         string
	ConversationID    string
	RequestID         string
	AgentSessionID    string
	ReceivedAt        time.Time
}

type Totals struct {
	Turns                    int64   `json:"turns"`
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	WebSearchRequests        int64   `json:"webSearchRequests"`
	TotalTokens              int64   `json:"totalTokens"`
	CostUSD                  float64 `json:"costUsd"`
}

type ModelSummary struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	CanonicalModel string `json:"canonicalModel,omitempty"`
	Totals
}

type DailySummary struct {
	Date string `json:"date"`
	Totals
}

type Summary struct {
	From     time.Time      `json:"from"`
	To       time.Time      `json:"to"`
	Totals   Totals         `json:"totals"`
	ByModel  []ModelSummary `json:"byModel"`
	Daily    []DailySummary `json:"daily"`
	Currency string         `json:"currency"`
	Source   string         `json:"costSource"`
}

// ListItem is one immutable model-level billing observation. A single Claude
// turn may contain more than one row when sub-agents use different models.
// DatabaseID is used only for stable keyset pagination and is never serialized.
type ListItem struct {
	DatabaseID               int64     `json:"-"`
	OccurredAt               time.Time `json:"occurredAt"`
	ProjectID                string    `json:"projectId"`
	ProjectName              string    `json:"projectName,omitempty"`
	ConversationID           string    `json:"conversationId"`
	RequestID                string    `json:"requestId"`
	ResultStatus             string    `json:"resultStatus"`
	Provider                 string    `json:"provider"`
	Model                    string    `json:"model"`
	CanonicalModel           string    `json:"canonicalModel,omitempty"`
	InputTokens              int64     `json:"inputTokens"`
	OutputTokens             int64     `json:"outputTokens"`
	CacheReadInputTokens     int64     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64     `json:"cacheCreationInputTokens"`
	WebSearchRequests        int64     `json:"webSearchRequests"`
	TotalTokens              int64     `json:"totalTokens"`
	CostUSD                  float64   `json:"costUsd"`
	CostSource               string    `json:"costSource"`
}

type ListPage struct {
	Items      []ListItem `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"`
}
