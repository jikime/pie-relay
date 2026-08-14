package manager

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJobJSONUsesPublicCamelCaseAndReadsLegacyFields(t *testing.T) {
	encoded, err := json.Marshal(Job{ID: "job-a", UserID: "user-a", ExecutorID: "executor-a", Status: "queued", Err: "boom"})
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	for _, want := range []string{`"id":"job-a"`, `"userId":"user-a"`, `"executorId":"executor-a"`, `"status":"queued"`, `"error":"boom"`} {
		if !strings.Contains(value, want) {
			t.Fatalf("JSON %s is missing %s", value, want)
		}
	}
	var legacy Job
	if err := json.Unmarshal([]byte(`{"ID":"job-old","UserID":"user-old","ExecutorID":"executor-old","Status":"succeeded","Err":"legacy"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.ID != "job-old" || legacy.UserID != "user-old" || legacy.ExecutorID != "executor-old" || legacy.Status != "succeeded" || legacy.Err != "legacy" {
		t.Fatalf("legacy job was not decoded: %+v", legacy)
	}
}
