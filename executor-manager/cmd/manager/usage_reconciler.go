package main

import (
	"context"
	"log"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/chatgateway"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	usageledger "github.com/pielab-ai/pie-relay/executor-manager/internal/usage"
)

// runUsageReconciler turns the fsync'd chat journal into a small durable
// outbox. Direct recording is the fast path; this worker repairs transient DB
// failures and Manager restarts. The SQL uniqueness key makes replay harmless.
func runUsageReconciler(ctx context.Context, usage *usageledger.Service, controlService *control.Service, journal *chatgateway.Journal, interval time.Duration) {
	if usage == nil || controlService == nil || journal == nil {
		return
	}
	cursors := map[string]uint64{}
	reconcile := func() {
		for _, conversation := range controlService.Conversations() {
			for {
				events, err := journal.Events(ctx, conversation.ID, cursors[conversation.ID], 1000)
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("usage_reconcile_read_failed conversation_id=%s error=%v", conversation.ID, err)
					}
					break
				}
				if len(events) == 0 {
					break
				}
				failed := false
				for _, event := range events {
					if event.Type == "usage" {
						if err := usage.RecordUsageAt(ctx, conversation, event.RequestID, event.Data, event.At); err != nil {
							if ctx.Err() == nil {
								log.Printf("usage_reconcile_write_failed conversation_id=%s request_id=%s error=%v", conversation.ID, event.RequestID, err)
							}
							failed = true
							break
						}
					}
					cursors[conversation.ID] = event.Sequence
				}
				if failed || len(events) < 1000 {
					break
				}
			}
		}
	}
	reconcile()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}
