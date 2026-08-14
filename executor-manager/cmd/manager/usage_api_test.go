package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestUsageRangeDefaultsAndBounds(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	request := httptest.NewRequest("GET", "/?days=7", nil)
	from, to, err := usageRange(request, now)
	if err != nil || !to.Equal(now) || !from.Equal(now.AddDate(0, 0, -7)) {
		t.Fatalf("from=%s to=%s err=%v", from, to, err)
	}
	for _, rawURL := range []string{"/?days=0", "/?days=367", "/?from=2026-08-05T00:00:00Z&to=2026-08-04T00:00:00Z", "/?from=not-a-date"} {
		if _, _, err := usageRange(httptest.NewRequest("GET", rawURL, nil), now); err == nil {
			t.Fatalf("expected invalid range for %s", rawURL)
		}
	}
}

func TestUsageListLimitDefaultsAndBounds(t *testing.T) {
	for _, test := range []struct {
		rawURL string
		want   int
		valid  bool
	}{
		{"/", 30, true}, {"/?limit=1", 1, true}, {"/?limit=100", 100, true},
		{"/?limit=0", 0, false}, {"/?limit=101", 0, false}, {"/?limit=bad", 0, false},
	} {
		got, err := usageListLimit(httptest.NewRequest("GET", test.rawURL, nil))
		if test.valid && (err != nil || got != test.want) {
			t.Fatalf("%s: got=%d err=%v", test.rawURL, got, err)
		}
		if !test.valid && err == nil {
			t.Fatalf("%s: invalid limit accepted", test.rawURL)
		}
	}
}
