package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var ErrInvalidEvent = errors.New("invalid LLM usage event")

type PostgresStore struct {
	db *sql.DB
}

func OpenPostgres(ctx context.Context, dsn string) (*PostgresStore, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("usage PostgreSQL DSN is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	store := &PostgresStore{db: db}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresStore) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS pie_llm_price_versions (
  id bigserial PRIMARY KEY,
  provider text NOT NULL,
  canonical_model text NOT NULL,
  effective_from timestamptz NOT NULL,
  effective_to timestamptz,
  input_usd_per_million numeric(20,12) NOT NULL CHECK (input_usd_per_million >= 0),
  output_usd_per_million numeric(20,12) NOT NULL CHECK (output_usd_per_million >= 0),
  cache_read_usd_per_million numeric(20,12) NOT NULL CHECK (cache_read_usd_per_million >= 0),
  cache_creation_usd_per_million numeric(20,12) NOT NULL CHECK (cache_creation_usd_per_million >= 0),
  web_search_usd_per_request numeric(20,12) NOT NULL DEFAULT 0 CHECK (web_search_usd_per_request >= 0),
  source text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (effective_to IS NULL OR effective_to > effective_from),
  UNIQUE(provider, canonical_model, effective_from)
);
CREATE INDEX IF NOT EXISTS pie_llm_price_versions_lookup_idx
  ON pie_llm_price_versions(provider, canonical_model, effective_from DESC);

CREATE TABLE IF NOT EXISTS pie_llm_usage_events (
  id bigserial PRIMARY KEY,
  integration_id text NOT NULL,
  integration_user_id text NOT NULL,
  owner_user_id text NOT NULL,
  project_id text NOT NULL,
  conversation_id text NOT NULL,
  request_id text NOT NULL,
  query_run_id text NOT NULL,
  result_id text NOT NULL,
  agent_session_id text NOT NULL,
  result_subtype text NOT NULL,
  provider text NOT NULL,
  model text NOT NULL,
  canonical_model text NOT NULL,
  input_tokens bigint NOT NULL CHECK (input_tokens >= 0),
  output_tokens bigint NOT NULL CHECK (output_tokens >= 0),
  cache_read_input_tokens bigint NOT NULL CHECK (cache_read_input_tokens >= 0),
  cache_creation_input_tokens bigint NOT NULL CHECK (cache_creation_input_tokens >= 0),
  web_search_requests bigint NOT NULL CHECK (web_search_requests >= 0),
  provider_cost_usd numeric(20,12) NOT NULL CHECK (provider_cost_usd >= 0),
  calculated_cost_usd numeric(20,12) NOT NULL CHECK (calculated_cost_usd >= 0),
  price_version_id bigint REFERENCES pie_llm_price_versions(id),
  cost_source text NOT NULL,
  raw_event jsonb NOT NULL,
  reported_at timestamptz,
  received_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(conversation_id, result_id, model)
);
CREATE INDEX IF NOT EXISTS pie_llm_usage_events_user_time_idx
  ON pie_llm_usage_events(integration_id, integration_user_id, received_at DESC);
CREATE INDEX IF NOT EXISTS pie_llm_usage_events_user_time_page_idx
  ON pie_llm_usage_events(integration_id, integration_user_id, received_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS pie_llm_usage_events_project_time_idx
  ON pie_llm_usage_events(project_id, received_at DESC);
CREATE INDEX IF NOT EXISTS pie_llm_usage_events_conversation_time_idx
  ON pie_llm_usage_events(conversation_id, received_at DESC);
`)
	return err
}

func (s *PostgresStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *PostgresStore) Close() error                   { return s.db.Close() }

type price struct {
	id                                      sql.NullInt64
	input, output, cacheRead, cacheCreation float64
	webSearch                               float64
}

func (s *PostgresStore) Record(ctx context.Context, attribution Attribution, event Event, raw json.RawMessage) error {
	if err := validate(attribution, event, raw); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	models := make([]string, 0, len(event.ModelUsage))
	for model := range event.ModelUsage {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		measurement := event.ModelUsage[model]
		provider := normalized(measurement.Provider, "unknown")
		canonicalModel := normalized(measurement.CanonicalModel, model)
		pricing, found, err := activePrice(ctx, tx, provider, canonicalModel, attribution.ReceivedAt)
		if err != nil {
			return err
		}
		calculatedCost, costSource := measurement.CostUSD, "claude-agent-sdk"
		var priceID any
		if found {
			calculatedCost = float64(measurement.InputTokens)*pricing.input/1_000_000 +
				float64(measurement.OutputTokens)*pricing.output/1_000_000 +
				float64(measurement.CacheReadInputTokens)*pricing.cacheRead/1_000_000 +
				float64(measurement.CacheCreationInputTokens)*pricing.cacheCreation/1_000_000 +
				float64(measurement.WebSearchRequests)*pricing.webSearch
			costSource, priceID = "manager-price-table", pricing.id.Int64
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO pie_llm_usage_events (
  integration_id,integration_user_id,owner_user_id,project_id,conversation_id,request_id,
  query_run_id,result_id,agent_session_id,result_subtype,provider,model,canonical_model,
  input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,web_search_requests,
  provider_cost_usd,calculated_cost_usd,price_version_id,cost_source,raw_event,reported_at,received_at
) VALUES (
  $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25
) ON CONFLICT (conversation_id,result_id,model) DO NOTHING`,
			attribution.IntegrationID, attribution.IntegrationUserID, attribution.OwnerUserID, attribution.ProjectID,
			attribution.ConversationID, attribution.RequestID, event.QueryRunID, event.ResultID,
			normalized(event.SessionID, attribution.AgentSessionID), event.Subtype, provider, model, canonicalModel,
			measurement.InputTokens, measurement.OutputTokens, measurement.CacheReadInputTokens,
			measurement.CacheCreationInputTokens, measurement.WebSearchRequests, measurement.CostUSD,
			calculatedCost, priceID, costSource, raw, nullableTime(event.ReportedAt), attribution.ReceivedAt)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func activePrice(ctx context.Context, tx *sql.Tx, provider, model string, at time.Time) (price, bool, error) {
	var value price
	err := tx.QueryRowContext(ctx, `
SELECT id,input_usd_per_million::float8,output_usd_per_million::float8,
       cache_read_usd_per_million::float8,cache_creation_usd_per_million::float8,
       web_search_usd_per_request::float8
FROM pie_llm_price_versions
WHERE provider=$1 AND canonical_model=$2 AND effective_from <= $3
  AND (effective_to IS NULL OR effective_to > $3)
ORDER BY effective_from DESC LIMIT 1`, provider, model, at).Scan(
		&value.id, &value.input, &value.output, &value.cacheRead, &value.cacheCreation, &value.webSearch)
	if errors.Is(err, sql.ErrNoRows) {
		return price{}, false, nil
	}
	return value, err == nil, err
}

func (s *PostgresStore) Summary(ctx context.Context, integrationID, integrationUserID string, from, to time.Time) (Summary, error) {
	if integrationID == "" || integrationUserID == "" || from.IsZero() || !to.After(from) {
		return Summary{}, errors.New("invalid usage summary scope")
	}
	result := Summary{From: from, To: to, Currency: "USD", Source: "SDK cost or versioned Manager price", ByModel: []ModelSummary{}, Daily: []DailySummary{}}
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(DISTINCT result_id),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read_input_tokens),0),COALESCE(SUM(cache_creation_input_tokens),0),
       COALESCE(SUM(web_search_requests),0),COALESCE(SUM(calculated_cost_usd),0)::float8
FROM pie_llm_usage_events
WHERE integration_id=$1 AND integration_user_id=$2 AND received_at >= $3 AND received_at < $4`,
		integrationID, integrationUserID, from, to).Scan(&result.Totals.Turns, &result.Totals.InputTokens,
		&result.Totals.OutputTokens, &result.Totals.CacheReadInputTokens, &result.Totals.CacheCreationInputTokens,
		&result.Totals.WebSearchRequests, &result.Totals.CostUSD); err != nil {
		return Summary{}, err
	}
	result.Totals.TotalTokens = totalTokens(result.Totals)
	rows, err := s.db.QueryContext(ctx, `
SELECT provider,model,canonical_model,COUNT(DISTINCT result_id),SUM(input_tokens),SUM(output_tokens),
       SUM(cache_read_input_tokens),SUM(cache_creation_input_tokens),SUM(web_search_requests),SUM(calculated_cost_usd)::float8
FROM pie_llm_usage_events
WHERE integration_id=$1 AND integration_user_id=$2 AND received_at >= $3 AND received_at < $4
GROUP BY provider,model,canonical_model ORDER BY SUM(calculated_cost_usd) DESC, model`, integrationID, integrationUserID, from, to)
	if err != nil {
		return Summary{}, err
	}
	for rows.Next() {
		var value ModelSummary
		if err := rows.Scan(&value.Provider, &value.Model, &value.CanonicalModel, &value.Turns, &value.InputTokens,
			&value.OutputTokens, &value.CacheReadInputTokens, &value.CacheCreationInputTokens,
			&value.WebSearchRequests, &value.CostUSD); err != nil {
			rows.Close()
			return Summary{}, err
		}
		value.TotalTokens = totalTokens(value.Totals)
		result.ByModel = append(result.ByModel, value)
	}
	if err := rows.Close(); err != nil {
		return Summary{}, err
	}
	rows, err = s.db.QueryContext(ctx, `
SELECT to_char(date_trunc('day',received_at AT TIME ZONE 'UTC'),'YYYY-MM-DD'),COUNT(DISTINCT result_id),
       SUM(input_tokens),SUM(output_tokens),SUM(cache_read_input_tokens),SUM(cache_creation_input_tokens),
       SUM(web_search_requests),SUM(calculated_cost_usd)::float8
FROM pie_llm_usage_events
WHERE integration_id=$1 AND integration_user_id=$2 AND received_at >= $3 AND received_at < $4
GROUP BY 1 ORDER BY 1`, integrationID, integrationUserID, from, to)
	if err != nil {
		return Summary{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var value DailySummary
		if err := rows.Scan(&value.Date, &value.Turns, &value.InputTokens, &value.OutputTokens,
			&value.CacheReadInputTokens, &value.CacheCreationInputTokens, &value.WebSearchRequests, &value.CostUSD); err != nil {
			return Summary{}, err
		}
		value.TotalTokens = totalTokens(value.Totals)
		result.Daily = append(result.Daily, value)
	}
	return result, rows.Err()
}

func (s *PostgresStore) List(ctx context.Context, integrationID, integrationUserID string, from, to time.Time, limit int, before *listCursor) ([]ListItem, bool, error) {
	if integrationID == "" || integrationUserID == "" || from.IsZero() || !to.After(from) || limit < 1 || limit > 100 {
		return nil, false, errors.New("invalid usage list scope")
	}
	hasCursor := before != nil
	beforeAt, beforeID := time.Time{}, int64(0)
	if before != nil {
		beforeAt, beforeID = before.OccurredAt, before.DatabaseID
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id,received_at,project_id,conversation_id,request_id,result_subtype,provider,model,canonical_model,
       input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,web_search_requests,
       calculated_cost_usd::float8,cost_source
FROM pie_llm_usage_events
WHERE integration_id=$1 AND integration_user_id=$2 AND received_at >= $3 AND received_at < $4
  AND ($5 = false OR received_at < $6 OR (received_at = $6 AND id < $7))
ORDER BY received_at DESC,id DESC LIMIT $8`, integrationID, integrationUserID, from, to,
		hasCursor, beforeAt, beforeID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := make([]ListItem, 0, limit+1)
	for rows.Next() {
		var item ListItem
		if err := rows.Scan(&item.DatabaseID, &item.OccurredAt, &item.ProjectID, &item.ConversationID,
			&item.RequestID, &item.ResultStatus, &item.Provider, &item.Model, &item.CanonicalModel,
			&item.InputTokens, &item.OutputTokens, &item.CacheReadInputTokens, &item.CacheCreationInputTokens,
			&item.WebSearchRequests, &item.CostUSD, &item.CostSource); err != nil {
			return nil, false, err
		}
		item.TotalTokens = item.InputTokens + item.OutputTokens + item.CacheReadInputTokens + item.CacheCreationInputTokens
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	return items, hasMore, nil
}

func validate(attribution Attribution, event Event, raw json.RawMessage) error {
	ids := []string{attribution.IntegrationID, attribution.IntegrationUserID, attribution.OwnerUserID, attribution.ProjectID, attribution.ConversationID, attribution.RequestID, event.ResultID, event.QueryRunID}
	for _, value := range ids {
		if strings.TrimSpace(value) == "" || len(value) > 512 {
			return ErrInvalidEvent
		}
	}
	if event.Type != "usage" || event.SchemaVersion != 1 || len(event.ModelUsage) == 0 || len(event.ModelUsage) > 32 || len(raw) == 0 || len(raw) > 1<<20 || attribution.ReceivedAt.IsZero() {
		return ErrInvalidEvent
	}
	for model, value := range event.ModelUsage {
		if strings.TrimSpace(model) == "" || len(model) > 256 || len(value.Provider) > 128 || len(value.CanonicalModel) > 256 ||
			value.InputTokens < 0 || value.OutputTokens < 0 || value.CacheReadInputTokens < 0 || value.CacheCreationInputTokens < 0 || value.WebSearchRequests < 0 ||
			!finiteNonNegative(value.CostUSD) {
			return ErrInvalidEvent
		}
	}
	return nil
}

func finiteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
func normalized(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
func totalTokens(value Totals) int64 {
	return value.InputTokens + value.OutputTokens + value.CacheReadInputTokens + value.CacheCreationInputTokens
}

func (s *PostgresStore) String() string { return fmt.Sprintf("PostgresStore(%p)", s) }
