package chatagent

import "testing"

func TestRequestReplayDeduplicatesActiveAndCompletedTurn(t *testing.T) {
	replay := newRequestReplay()
	request := []byte(`{"type":"chat","requestId":"request-a","prompt":"hello"}`)
	if duplicate, _ := replay.Begin(request); duplicate {
		t.Fatal("first request was treated as duplicate")
	}
	replay.Observe([]byte(`{"type":"text","text":"hello"}`), false)
	if duplicate, frames := replay.Begin(request); !duplicate || len(frames) != 1 {
		t.Fatalf("active duplicate=%t frames=%d", duplicate, len(frames))
	}
	replay.Observe([]byte(`{"type":"done"}`), true)
	if duplicate, frames := replay.Begin(request); !duplicate || len(frames) != 2 {
		t.Fatalf("completed duplicate=%t frames=%d", duplicate, len(frames))
	}
}

func TestRequestReplayAllowsRetryAfterExecutorCrash(t *testing.T) {
	replay := newRequestReplay()
	request := []byte(`{"type":"chat","requestId":"request-a"}`)
	replay.Begin(request)
	replay.ResetActive()
	if duplicate, _ := replay.Begin(request); duplicate {
		t.Fatal("request remained locked after executor crash")
	}
}

func TestRequestReplayDoesNotChangeLegacyChatWithoutRequestID(t *testing.T) {
	replay := newRequestReplay()
	request := []byte(`{"type":"chat","prompt":"legacy"}`)
	if duplicate, _ := replay.Begin(request); duplicate {
		t.Fatal("legacy request was treated as duplicate")
	}
	if duplicate, _ := replay.Begin(request); duplicate {
		t.Fatal("legacy retry was treated as duplicate")
	}
}
