// Package relay bridges a host daemon websocket and the participant websockets
// sharing its room.
package relay

import (
	"sort"
	"sync"
	"time"
)

// Sender sends one text ws message to a peer.
type Sender interface {
	Send(msg []byte) error
}

// Participant is the verified identity metadata carried alongside a
// participant's Sender: who they are (from 표기) and whether they may see
// permission prompts / drive approvals (Role == RoleHost).
type Participant struct {
	UserID       string
	Role         string
	Access       string
	ConnectionID string
	ConnectedAt  time.Time
}

type ParticipantSnapshot struct {
	UserID       string `json:"userId"`
	Role         string `json:"role"`
	Access       string `json:"access"`
	ConnectionID string `json:"connectionId"`
	ConnectedAt  int64  `json:"connectedAt"`
}

type DriverLease struct {
	UserID     string    `json:"userId"`
	Generation uint64    `json:"generation"`
	ExpiresAt  time.Time `json:"-"`
}

// Registry matches, per ROOM, the room's participants to their host Sender.
// One host per room (the single laptop daemon running claude); MULTIPLE
// participants per room (different people, plus the host operator's own
// connections and reconnect races) — host events fan out to every registered
// participant. A single-participant slot would let a newer connection
// replace-and-orphan an older one: the old socket stays open but never
// receives responses again.
//
// Rooms exist implicitly on first connect and vanish when empty (stateless,
// single-instance — same assumption the invite store makes).
type Registry struct {
	mu           sync.Mutex
	hosts        map[string]Sender
	hostEpochs   map[string]uint64
	participants map[string]map[Sender]Participant
	drivers      map[string]DriverLease
	driverSeq    map[string]uint64
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		hosts:        map[string]Sender{},
		hostEpochs:   map[string]uint64{},
		participants: map[string]map[Sender]Participant{},
		drivers:      map[string]DriverLease{},
		driverSeq:    map[string]uint64{},
	}
}

// RegisterHost registers the room's host and returns a func to unregister it.
func (r *Registry) RegisterHost(room string, s Sender) (unregister func()) {
	_, unregister = r.RegisterHostWithEpoch(room, s)
	return unregister
}

// RegisterHostWithEpoch registers a host and returns the monotonically
// increasing room-local host epoch. A participant can use the epoch to tell a
// reconnect from the previous host process apart from a genuinely new PTY
// producer, without changing the legacy RegisterHost API used by older tests
// and call sites.
func (r *Registry) RegisterHostWithEpoch(room string, s Sender) (epoch uint64, unregister func()) {
	r.mu.Lock()
	r.hostEpochs[room]++
	epoch = r.hostEpochs[room]
	r.hosts[room] = s
	r.mu.Unlock()
	return epoch, func() {
		r.mu.Lock()
		if r.hosts[room] == s {
			delete(r.hosts, room)
		}
		r.mu.Unlock()
	}
}

// HostEpoch returns the most recently assigned host epoch for room. Epochs
// intentionally remain after a room becomes empty so a later host can never
// reuse an old incarnation number during this relay process lifetime.
func (r *Registry) HostEpoch(room string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hostEpochs[room]
}

// RegisterParticipant registers a participant REGARDLESS of host presence (the
// participant stays connected and receives live host:status pushes as the
// laptop daemon comes and goes). Multiple participants may coexist. Returns
// whether a host is currently connected and an unregister func for THIS
// participant only.
func (r *Registry) RegisterParticipant(room string, s Sender, p Participant) (hostConnected bool, unregister func()) {
	hostConnected, unregister, _ = r.TryRegisterParticipant(room, s, p, 0)
	return hostConnected, unregister
}

// TryRegisterParticipant atomically enforces a room participant limit. A
// non-positive limit means unlimited and preserves the legacy API behavior.
func (r *Registry) TryRegisterParticipant(room string, s Sender, p Participant, limit int) (hostConnected bool, unregister func(), ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, exists := r.hosts[room]
	if r.participants[room] == nil {
		r.participants[room] = map[Sender]Participant{}
	}
	if limit > 0 && len(r.participants[room]) >= limit {
		return exists, func() {}, false
	}
	if p.ConnectedAt.IsZero() {
		p.ConnectedAt = time.Now().UTC()
	}
	if p.Access == "" {
		p.Access = AccessControl
	}
	r.participants[room][s] = p
	return exists, func() {
		r.mu.Lock()
		if set := r.participants[room]; set != nil {
			delete(set, s)
			if len(set) == 0 {
				delete(r.participants, room)
			}
		}
		r.mu.Unlock()
	}, true
}

func (r *Registry) ParticipantSnapshots(room string) []ParticipantSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.participants[room]
	out := make([]ParticipantSnapshot, 0, len(set))
	for _, p := range set {
		out = append(out, ParticipantSnapshot{
			UserID: p.UserID, Role: p.Role, Access: p.Access,
			ConnectionID: p.ConnectionID, ConnectedAt: p.ConnectedAt.UnixMilli(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UserID == out[j].UserID {
			return out[i].ConnectionID < out[j].ConnectionID
		}
		return out[i].UserID < out[j].UserID
	})
	return out
}

func (r *Registry) canDriveLocked(room, userID string) bool {
	for _, p := range r.participants[room] {
		if p.UserID == userID && (p.Role == RoleHost || p.Access == AccessControl) {
			return true
		}
	}
	return false
}

func (r *Registry) currentDriverLocked(room string, now time.Time) (DriverLease, bool) {
	lease, ok := r.drivers[room]
	if ok && !lease.ExpiresAt.After(now) {
		delete(r.drivers, room)
		r.driverSeq[room]++
		ok = false
	}
	if !ok {
		return DriverLease{Generation: r.driverSeq[room]}, false
	}
	return lease, ok
}

// AcquireDriverIfFree grants the seat only when no live lease exists. A new
// control participant therefore cannot steal input merely by reconnecting.
func (r *Registry) AcquireDriverIfFree(room, userID string, now time.Time, ttl time.Duration) (DriverLease, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lease, ok := r.currentDriverLocked(room, now); ok {
		return lease, false
	}
	if !r.canDriveLocked(room, userID) {
		return DriverLease{}, false
	}
	r.driverSeq[room]++
	lease := DriverLease{UserID: userID, Generation: r.driverSeq[room], ExpiresAt: now.Add(ttl)}
	r.drivers[room] = lease
	return lease, true
}

// SetDriver is the host-authoritative explicit handoff/revoke operation.
func (r *Registry) SetDriver(room, userID string, now time.Time, ttl time.Duration) (DriverLease, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if userID != "" && !r.canDriveLocked(room, userID) {
		return DriverLease{}, false
	}
	r.driverSeq[room]++
	lease := DriverLease{UserID: userID, Generation: r.driverSeq[room]}
	if userID == "" {
		delete(r.drivers, room)
		return lease, true
	}
	lease.ExpiresAt = now.Add(ttl)
	r.drivers[room] = lease
	return lease, true
}

func (r *Registry) RenewDriver(room, userID string, now time.Time, ttl time.Duration) (DriverLease, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lease, ok := r.currentDriverLocked(room, now)
	if !ok || lease.UserID != userID || !r.canDriveLocked(room, userID) {
		return DriverLease{}, false
	}
	lease.ExpiresAt = now.Add(ttl)
	r.drivers[room] = lease
	return lease, true
}

func (r *Registry) Driver(room string, now time.Time) (DriverLease, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentDriverLocked(room, now)
}

// ReleaseDriverIfAbsent revokes a lease only after the driver's final socket
// disappears (multiple tabs/reconnect overlap remain valid).
func (r *Registry) ReleaseDriverIfAbsent(room, userID string, now time.Time) (DriverLease, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.participants[room] {
		if p.UserID == userID {
			return DriverLease{}, false
		}
	}
	lease, ok := r.currentDriverLocked(room, now)
	if !ok || lease.UserID != userID {
		return DriverLease{}, false
	}
	r.driverSeq[room]++
	delete(r.drivers, room)
	return DriverLease{Generation: r.driverSeq[room]}, true
}

// Counts returns point-in-time gauges for diagnostics/Prometheus exposition.
func (r *Registry) Counts() (rooms, hosts, participants int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hosts = len(r.hosts)
	seen := make(map[string]struct{}, len(r.hosts)+len(r.participants))
	for room := range r.hosts {
		seen[room] = struct{}{}
	}
	for room, set := range r.participants {
		seen[room] = struct{}{}
		participants += len(set)
	}
	return len(seen), hosts, participants
}

// HostFor returns the host Sender for room.
func (r *Registry) HostFor(room string) (Sender, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.hosts[room]
	return s, ok
}

// ParticipantsFor returns a snapshot of EVERY participant Sender in room.
func (r *Registry) ParticipantsFor(room string) []Sender {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.participants[room]
	out := make([]Sender, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	return out
}

// ParticipantsByUserID returns the participant Senders in room whose identity
// matches userID. Used for targeted host→participant frames (a "to" field): a
// terminal host replays its scrollback to a single late-joining viewer without
// duplicating it onto everyone else's screen. A user may have several senders
// (multiple tabs), so this returns a slice.
func (r *Registry) ParticipantsByUserID(room, userID string) []Sender {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.participants[room]
	out := make([]Sender, 0, 1)
	for s, p := range set {
		if p.UserID == userID {
			out = append(out, s)
		}
	}
	return out
}

// HostSendersFor returns only the participant Senders whose role is host —
// the sole connections allowed to see permission_request prompts and to drive
// permission_response/abort. The host operator connects on the participant
// axis with a role=host token to approve tool use in their own room; guests
// (role=participant) are excluded so they never see the approval UI.
func (r *Registry) HostSendersFor(room string) []Sender {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.participants[room]
	out := make([]Sender, 0, len(set))
	for s, p := range set {
		if p.Role == RoleHost {
			out = append(out, s)
		}
	}
	return out
}
