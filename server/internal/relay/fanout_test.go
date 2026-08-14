package relay

import (
	"sync/atomic"
	"testing"
)

type countingSender struct{ count atomic.Uint64 }

func (s *countingSender) Send([]byte) error {
	s.count.Add(1)
	return nil
}

func TestFanoutToManyParticipants(t *testing.T) {
	s := NewServerOpts(ServerOptions{})
	const participants = 128
	peers := make([]*countingSender, 0, participants)
	for i := 0; i < participants; i++ {
		peer := &countingSender{}
		peers = append(peers, peer)
		_, _ = s.reg.RegisterParticipant("load-room", peer, Participant{UserID: "viewer", Access: AccessView})
	}
	for i := 0; i < 200; i++ {
		s.routeFromHost("load-room", "pty_output", []byte(`{"type":"pty_output","data":"eA=="}`))
	}
	for i, peer := range peers {
		if got := peer.count.Load(); got != 200 {
			t.Fatalf("participant %d received %d/200 frames", i, got)
		}
	}
}

func BenchmarkFanoutTerminalOutput(b *testing.B) {
	s := NewServerOpts(ServerOptions{})
	for i := 0; i < 64; i++ {
		_, _ = s.reg.RegisterParticipant("bench-room", &countingSender{}, Participant{UserID: "viewer", Access: AccessView})
	}
	payload := make([]byte, 16*1024)
	copy(payload, `{"type":"pty_output","data":"`)
	b.SetBytes(int64(len(payload) * 64))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.routeFromHost("bench-room", "pty_output", payload)
	}
}
