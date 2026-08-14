package usage

import (
	"errors"
	"testing"
	"time"
)

func TestListCursorRoundTripAndRejectsTampering(t *testing.T) {
	at := time.Date(2026, 8, 5, 12, 34, 56, 789, time.UTC)
	raw := encodeListCursor(ListItem{DatabaseID: 42, OccurredAt: at})
	decoded, err := decodeListCursor(raw)
	if err != nil || decoded.DatabaseID != 42 || !decoded.OccurredAt.Equal(at) {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	for _, invalid := range []string{"%bad", raw + "extra", "eyJ2IjoyLCJhdCI6IjIwMjYtMDgtMDVUMTI6MzQ6NTZaIiwiaWQiOjQyfQ"} {
		if _, err := decodeListCursor(invalid); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("invalid cursor accepted: %q (%v)", invalid, err)
		}
	}
}

func TestEmptyListCursorStartsFirstPage(t *testing.T) {
	decoded, err := decodeListCursor("")
	if err != nil || decoded != nil {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
}
