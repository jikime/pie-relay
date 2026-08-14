package control

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestPostgresMultiManagerRefreshAndConflict(t *testing.T) {
	dsn := os.Getenv("PIE_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("PIE_TEST_POSTGRES_URL") // backward-compatible local scripts
	}
	if dsn == "" {
		t.Skip("PIE_TEST_POSTGRES_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	storeA, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	storeB, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()
	serviceA, err := NewService(ctx, storeA)
	if err != nil {
		t.Fatal(err)
	}
	serviceB, err := NewService(ctx, storeB)
	if err != nil {
		t.Fatal(err)
	}
	id := "pg-user-" + time.Now().UTC().Format("150405.000000000")
	created, err := serviceA.PutUser(ctx, User{ID: id, Status: "active"}, 0, MutationMeta{ActorUserID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := serviceB.User(id); ok {
		t.Fatal("second Manager observed an unrefreshed record")
	}
	if err := serviceB.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	copyB, ok := serviceB.User(id)
	if !ok || copyB.Version != created.Version {
		t.Fatalf("refreshed user=%+v ok=%t", copyB, ok)
	}
	created.Status = "suspended"
	if _, err := serviceA.PutUser(ctx, created, created.Version, MutationMeta{ActorUserID: "test-a"}); err != nil {
		t.Fatal(err)
	}
	copyB.OrganizationID = "stale-write"
	if _, err := serviceB.PutUser(ctx, copyB, copyB.Version, MutationMeta{ActorUserID: "test-b"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write error=%v", err)
	}
	key := "pg-operation-" + id
	intent := Operation{ActorUserID: "operator", IdempotencyKey: key, Type: "runtime.start", TargetKind: KindRuntime, TargetID: "runtime-a"}
	claimed, duplicate, err := serviceA.BeginOperation(ctx, intent, MutationMeta{ActorUserID: "operator"})
	if err != nil || duplicate {
		t.Fatalf("first operation=%+v duplicate=%t err=%v", claimed, duplicate, err)
	}
	replayed, duplicate, err := serviceB.BeginOperation(ctx, intent, MutationMeta{ActorUserID: "operator"})
	if err != nil || !duplicate || replayed.ID != claimed.ID {
		t.Fatalf("cross-manager operation=%+v duplicate=%t err=%v", replayed, duplicate, err)
	}
	intent.TargetID = "runtime-b"
	if _, _, err := serviceB.BeginOperation(ctx, intent, MutationMeta{ActorUserID: "operator"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-manager idempotency key accepted another intent: %v", err)
	}
	if _, err := serviceA.UpdateOperation(ctx, claimed.ID, "succeeded", 100, map[string]any{"ok": true}, "", MutationMeta{ActorUserID: "test-a"}); err != nil {
		t.Fatal(err)
	}
	if err := serviceB.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	completed, ok := serviceB.Operation(claimed.ID)
	if !ok || completed.Status != "succeeded" {
		t.Fatalf("incrementally refreshed operation=%+v ok=%t", completed, ok)
	}
	removed, err := serviceA.PruneTerminalOperations(ctx, time.Now().Add(time.Second), 100)
	if err != nil || removed < 1 {
		t.Fatalf("pruned=%d err=%v", removed, err)
	}
	if err := serviceB.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := serviceB.Operation(claimed.ID); ok {
		t.Fatal("incremental refresh retained a deleted operation")
	}
	sequence, err := storeA.CurrentSequence(ctx)
	if err != nil || sequence < 1 {
		t.Fatalf("change sequence=%d err=%v", sequence, err)
	}
	var duplicateChangeRows int
	if err := storeA.db.QueryRowContext(ctx, `
SELECT count(*) FROM (
  SELECT kind,id FROM pie_control_changes GROUP BY kind,id HAVING count(*) > 1
) duplicates`).Scan(&duplicateChangeRows); err != nil {
		t.Fatal(err)
	}
	if duplicateChangeRows != 0 {
		t.Fatalf("change cursor table retained %d duplicate record histories", duplicateChangeRows)
	}

	lockKey := "integration-lock-" + id
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	lockErrors := make(chan error, 2)
	go func() {
		lockErrors <- storeA.WithLock(ctx, lockKey, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	select {
	case <-firstEntered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() {
		lockErrors <- storeB.WithLock(ctx, lockKey, func() error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("distributed advisory lock allowed two Manager replicas into the same scheduler section")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondEntered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for range 2 {
		if err := <-lockErrors; err != nil {
			t.Fatal(err)
		}
	}
}
