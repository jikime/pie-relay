package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
	"time"
)

func TestPostgresRecordIsIdempotentAndSummarizesOneUser(t *testing.T) {
	dsn := os.Getenv("PIE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PIE_TEST_POSTGRES_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	suffix := time.Now().UTC().Format("20060102150405.000000000")
	attribution := Attribution{
		IntegrationID: "integration-" + suffix, IntegrationUserID: "binding-" + suffix,
		OwnerUserID: "owner-" + suffix, ProjectID: "project-" + suffix,
		ConversationID: "conversation-" + suffix, RequestID: "request-" + suffix,
		ReceivedAt: time.Now().UTC(),
	}
	event := Event{Type: "usage", SchemaVersion: 1, ResultID: "result-" + suffix, QueryRunID: "run-" + suffix, SessionID: "session-" + suffix, Subtype: "success", ModelUsage: map[string]ModelMeasurement{
		"claude-test": {Provider: "firstParty", CanonicalModel: "claude-test-v1", InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 50, CacheCreationInputTokens: 10, CostUSD: 0.0123},
	}}
	raw, _ := json.Marshal(event)
	for range 2 {
		if err := store.Record(ctx, attribution, event, raw); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := store.Summary(ctx, attribution.IntegrationID, attribution.IntegrationUserID, attribution.ReceivedAt.Add(-time.Minute), attribution.ReceivedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.Turns != 1 || summary.Totals.TotalTokens != 180 || summary.Totals.CostUSD != 0.0123 {
		t.Fatalf("summary=%+v", summary)
	}
	if len(summary.ByModel) != 1 || summary.ByModel[0].CanonicalModel != "claude-test-v1" || len(summary.Daily) != 1 {
		t.Fatalf("breakdown=%+v daily=%+v", summary.ByModel, summary.Daily)
	}
}

func TestPostgresUsesAndFreezesVersionedModelPrice(t *testing.T) {
	dsn := os.Getenv("PIE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PIE_TEST_POSTGRES_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	suffix := time.Now().UTC().Format("20060102150405.000000000")
	receivedAt := time.Now().UTC()
	provider, canonicalModel := "provider-"+suffix, "canonical-"+suffix
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pie_llm_price_versions (
  provider,canonical_model,effective_from,input_usd_per_million,output_usd_per_million,
  cache_read_usd_per_million,cache_creation_usd_per_million,web_search_usd_per_request,source
) VALUES ($1,$2,$3,1,2,0.5,3,0.01,'integration-test')`, provider, canonicalModel, receivedAt.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	attribution := Attribution{
		IntegrationID: "integration-" + suffix, IntegrationUserID: "binding-" + suffix,
		OwnerUserID: "owner-" + suffix, ProjectID: "project-" + suffix,
		ConversationID: "conversation-" + suffix, RequestID: "request-" + suffix,
		ReceivedAt: receivedAt,
	}
	event := Event{Type: "usage", SchemaVersion: 1, ResultID: "result-" + suffix, QueryRunID: "run-" + suffix, ModelUsage: map[string]ModelMeasurement{
		"reported-model": {
			Provider: provider, CanonicalModel: canonicalModel, InputTokens: 100, OutputTokens: 20,
			CacheReadInputTokens: 50, CacheCreationInputTokens: 10, WebSearchRequests: 2, CostUSD: 9.99,
		},
	}}
	raw, _ := json.Marshal(event)
	if err := store.Record(ctx, attribution, event, raw); err != nil {
		t.Fatal(err)
	}

	var calculated, providerCost float64
	var source string
	var priceVersionID int64
	if err := store.db.QueryRowContext(ctx, `
SELECT calculated_cost_usd::float8,provider_cost_usd::float8,cost_source,price_version_id
FROM pie_llm_usage_events WHERE conversation_id=$1 AND result_id=$2 AND model='reported-model'`,
		attribution.ConversationID, event.ResultID).Scan(&calculated, &providerCost, &source, &priceVersionID); err != nil {
		t.Fatal(err)
	}
	const expected = 0.020195
	if math.Abs(calculated-expected) > 1e-12 || providerCost != 9.99 || source != "manager-price-table" || priceVersionID < 1 {
		t.Fatalf("calculated=%f provider=%f source=%s priceVersionID=%d", calculated, providerCost, source, priceVersionID)
	}

	// Adding a newer price cannot rewrite an already recorded cost snapshot.
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pie_llm_price_versions (
  provider,canonical_model,effective_from,input_usd_per_million,output_usd_per_million,
  cache_read_usd_per_million,cache_creation_usd_per_million,web_search_usd_per_request,source
) VALUES ($1,$2,$3,100,100,100,100,1,'future-test')`, provider, canonicalModel, receivedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT calculated_cost_usd::float8 FROM pie_llm_usage_events WHERE conversation_id=$1 AND result_id=$2`, attribution.ConversationID, event.ResultID).Scan(&calculated); err != nil {
		t.Fatal(err)
	}
	if math.Abs(calculated-expected) > 1e-12 {
		t.Fatalf("stored cost changed after a newer price was added: %f", calculated)
	}
}

func TestPostgresUsageListIsUserScopedAndCursorPaginated(t *testing.T) {
	dsn := os.Getenv("PIE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PIE_TEST_POSTGRES_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := NewService(store)
	suffix := time.Now().UTC().Format("20060102150405.000000000")
	receivedAt := time.Now().UTC().Truncate(time.Microsecond)
	integrationID, bindingID := "integration-list-"+suffix, "binding-list-"+suffix
	record := func(binding, request string, sequence int) {
		t.Helper()
		attribution := Attribution{
			IntegrationID: integrationID, IntegrationUserID: binding, OwnerUserID: "owner-" + binding,
			ProjectID: "project-" + binding, ConversationID: "conversation-" + binding,
			RequestID: request, ReceivedAt: receivedAt,
		}
		event := Event{Type: "usage", SchemaVersion: 1, ResultID: fmt.Sprintf("result-%s-%d", suffix, sequence), QueryRunID: "run-" + suffix, Subtype: "success", ModelUsage: map[string]ModelMeasurement{
			"claude-list": {Provider: "firstParty", InputTokens: int64(sequence), OutputTokens: 1, CostUSD: float64(sequence) / 1000},
		}}
		raw, _ := json.Marshal(event)
		if err := store.Record(ctx, attribution, event, raw); err != nil {
			t.Fatal(err)
		}
	}
	for sequence := 1; sequence <= 3; sequence++ {
		record(bindingID, fmt.Sprintf("request-%d", sequence), sequence)
	}
	record("another-binding-"+suffix, "request-other", 99)

	first, err := service.List(ctx, integrationID, bindingID, receivedAt.Add(-time.Minute), receivedAt.Add(time.Minute), 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextCursor == "" || first.Items[0].RequestID != "request-3" || first.Items[1].RequestID != "request-2" {
		t.Fatalf("first page=%+v", first)
	}
	second, err := service.List(ctx, integrationID, bindingID, receivedAt.Add(-time.Minute), receivedAt.Add(time.Minute), 2, first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.NextCursor != "" || second.Items[0].RequestID != "request-1" {
		t.Fatalf("second page=%+v", second)
	}
}
