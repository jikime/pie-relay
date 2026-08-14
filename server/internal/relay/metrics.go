package relay

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type relayMetrics struct {
	connections     atomic.Int64
	framesIn        atomic.Uint64
	bytesIn         atomic.Uint64
	framesOut       atomic.Uint64
	bytesOut        atomic.Uint64
	rateLimited     atomic.Uint64
	slowPeerEvicted atomic.Uint64
	roomRejected    atomic.Uint64
	presenceDropped atomic.Uint64
}

func (m *relayMetrics) handle(reg *Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		rooms, hosts, participants := reg.Counts()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = fmt.Fprintf(w,
			"# TYPE pie_relay_connections gauge\n"+
				"pie_relay_connections %d\n"+
				"pie_relay_rooms %d\n"+
				"pie_relay_hosts %d\n"+
				"pie_relay_participants %d\n"+
				"pie_relay_frames_in_total %d\n"+
				"pie_relay_bytes_in_total %d\n"+
				"pie_relay_frames_out_total %d\n"+
				"pie_relay_bytes_out_total %d\n"+
				"pie_relay_rate_limited_total %d\n"+
				"pie_relay_slow_peer_evicted_total %d\n"+
				"pie_relay_room_rejected_total %d\n"+
				"pie_relay_presence_dropped_total %d\n",
			m.connections.Load(), rooms, hosts, participants,
			m.framesIn.Load(), m.bytesIn.Load(), m.framesOut.Load(), m.bytesOut.Load(),
			m.rateLimited.Load(), m.slowPeerEvicted.Load(), m.roomRejected.Load(), m.presenceDropped.Load(),
		)
	}
}
