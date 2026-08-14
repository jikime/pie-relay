package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTokenBucketBoundsFramesAndRefills(t *testing.T) {
	now := time.Unix(100, 0)
	b := newTokenBucket(Limits{
		FramesPerSecond: 2, FrameBurst: 2,
		BytesPerSecond: 100, ByteBurst: 100,
	}, now)
	if !b.Allow(20, now) {
		t.Fatal("first burst frame should pass")
	}
	if !b.Allow(20, now) {
		t.Fatal("initial burst should pass")
	}
	if b.Allow(1, now) {
		t.Fatal("third frame should exceed frame burst")
	}
	if !b.Allow(50, now.Add(time.Second)) {
		t.Fatal("bucket should refill after one second")
	}
}

func TestTryRegisterParticipantAppliesRoomLimitAtomically(t *testing.T) {
	r := NewRegistry()
	if _, _, ok := r.TryRegisterParticipant("room", &fakeSender{}, Participant{UserID: "a"}, 1); !ok {
		t.Fatal("first participant rejected")
	}
	if _, _, ok := r.TryRegisterParticipant("room", &fakeSender{}, Participant{UserID: "b"}, 1); ok {
		t.Fatal("second participant should be rejected")
	}
	rooms, hosts, participants := r.Counts()
	if rooms != 1 || hosts != 0 || participants != 1 {
		t.Fatalf("counts = rooms:%d hosts:%d participants:%d", rooms, hosts, participants)
	}
}

func TestMetricsEndpointExposesRelayCounters(t *testing.T) {
	s := NewServerOpts(ServerOptions{MetricsToken: "metrics-secret"})
	s.metrics.framesIn.Add(3)
	s.metrics.bytesIn.Add(99)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer metrics-secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	for _, want := range []string{"pie_relay_frames_in_total 3", "pie_relay_bytes_in_total 99", "pie_relay_rooms 0"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("metrics missing %q:\n%s", want, w.Body.String())
		}
	}
}

func TestMetricsEndpointFailsClosedWithoutCredential(t *testing.T) {
	disabled := NewServerOpts(ServerOptions{})
	w := httptest.NewRecorder()
	disabled.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled status = %d", w.Code)
	}

	enabled := NewServerOpts(ServerOptions{MetricsToken: "metrics-secret"})
	w = httptest.NewRecorder()
	enabled.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", w.Code)
	}
}
