package relay

import (
	"sync"
	"testing"
	"time"
)

type fakeSender struct {
	mu  sync.Mutex
	got [][]byte
}

func TestRegistryDriverLeaseCannotBeStolenByReconnect(t *testing.T) {
	r := NewRegistry()
	_, _ = r.RegisterParticipant("room", &fakeSender{}, Participant{UserID: "alice", Access: AccessControl})
	_, _ = r.RegisterParticipant("room", &fakeSender{}, Participant{UserID: "bob", Access: AccessControl})
	now := time.Unix(100, 0)
	first, ok := r.AcquireDriverIfFree("room", "alice", now, 20*time.Second)
	if !ok || first.UserID != "alice" {
		t.Fatalf("alice did not acquire: %#v %t", first, ok)
	}
	current, changed := r.AcquireDriverIfFree("room", "bob", now.Add(time.Second), 20*time.Second)
	if changed || current.UserID != "alice" {
		t.Fatalf("bob stole live lease: %#v changed=%t", current, changed)
	}
	afterExpiry, changed := r.AcquireDriverIfFree("room", "bob", now.Add(21*time.Second), 20*time.Second)
	if !changed || afterExpiry.UserID != "bob" {
		t.Fatalf("bob should acquire expired lease: %#v changed=%t", afterExpiry, changed)
	}
	if afterExpiry.Generation != first.Generation+2 {
		t.Fatalf("expiry and reassignment generations = %d, want %d", afterExpiry.Generation, first.Generation+2)
	}
}

func TestRegistryIsolatesDeviceSessions(t *testing.T) {
	r := NewRegistry()
	a := Identity{Room: "shared", DeviceID: "device-a", SessionID: "session-a"}
	b := Identity{Room: "shared", DeviceID: "device-a", SessionID: "session-b"}
	hostA, hostB := &fakeSender{}, &fakeSender{}
	r.RegisterHost(a.RoutingKey(), hostA)
	r.RegisterHost(b.RoutingKey(), hostB)
	viewerA, viewerB := &fakeSender{}, &fakeSender{}
	r.RegisterParticipant(a.RoutingKey(), viewerA, Participant{UserID: "alice", Access: AccessControl})
	r.RegisterParticipant(b.RoutingKey(), viewerB, Participant{UserID: "bob", Access: AccessControl})
	if got, _ := r.HostFor(a.RoutingKey()); got != hostA {
		t.Fatal("session A host mismatch")
	}
	if got, _ := r.HostFor(b.RoutingKey()); got != hostB {
		t.Fatal("session B host mismatch")
	}
	if len(r.ParticipantsFor(a.RoutingKey())) != 1 || r.ParticipantsFor(a.RoutingKey())[0] != viewerA {
		t.Fatal("session A participants leaked")
	}
	if lease, ok := r.SetDriver(a.RoutingKey(), "alice", time.Now(), time.Minute); !ok || lease.UserID != "alice" {
		t.Fatal("session A driver missing")
	}
	if _, ok := r.Driver(b.RoutingKey(), time.Now()); ok {
		t.Fatal("driver leaked into session B")
	}
}

func TestRegistryDriverExplicitHandoffRequiresControlParticipant(t *testing.T) {
	r := NewRegistry()
	_, _ = r.RegisterParticipant("room", &fakeSender{}, Participant{UserID: "viewer", Access: AccessView})
	_, _ = r.RegisterParticipant("room", &fakeSender{}, Participant{UserID: "controller", Access: AccessControl})
	now := time.Now()
	if _, ok := r.SetDriver("room", "viewer", now, time.Minute); ok {
		t.Fatal("view participant received driver lease")
	}
	if lease, ok := r.SetDriver("room", "controller", now, time.Minute); !ok || lease.UserID != "controller" {
		t.Fatalf("control participant handoff failed: %#v %t", lease, ok)
	}
}

func (f *fakeSender) Send(m []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, append([]byte(nil), m...))
	return nil
}
func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()

	// Participant registers even with no host (hostConnected=false) — it stays
	// connected and waits for the daemon.
	p := &fakeSender{}
	up, earlyUnreg := r.RegisterParticipant("room", p, Participant{UserID: "u", Role: RoleParticipant})
	if up {
		t.Fatal("hostConnected should be false with no host")
	}
	if len(r.ParticipantsFor("room")) != 1 {
		t.Fatal("participant should be registered even without a host")
	}
	earlyUnreg()
	if len(r.ParticipantsFor("room")) != 0 {
		t.Fatal("participant should be gone after early unregister")
	}

	host := &fakeSender{}
	hostUnreg := r.RegisterHost("room", host)
	up, pUnreg := r.RegisterParticipant("room", p, Participant{UserID: "u", Role: RoleParticipant})
	if !up {
		t.Fatal("hostConnected should be true once the host is registered")
	}

	// Multiple participants coexist (people, reconnects) — a second
	// registration must NOT replace/orphan the first, and unregistering one
	// leaves the other.
	p2 := &fakeSender{}
	_, p2Unreg := r.RegisterParticipant("room", p2, Participant{UserID: "v", Role: RoleParticipant})
	if got := len(r.ParticipantsFor("room")); got != 2 {
		t.Fatalf("expected 2 participants, got %d", got)
	}
	p2Unreg()
	if got := len(r.ParticipantsFor("room")); got != 1 {
		t.Fatalf("expected 1 participant after second unregisters, got %d", got)
	}

	if s, ok := r.HostFor("room"); !ok || s != host {
		t.Errorf("HostFor(room) mismatch")
	}
	if _, ok := r.HostFor("other"); ok {
		t.Errorf("HostFor(other) should be false")
	}

	pUnreg()
	if len(r.ParticipantsFor("room")) != 0 {
		t.Errorf("ParticipantsFor(room) should be empty after unregister")
	}
	if _, ok := r.HostFor("room"); !ok {
		t.Errorf("HostFor(room) should remain")
	}

	hostUnreg()
	if _, ok := r.HostFor("room"); ok {
		t.Errorf("HostFor(room) should be gone")
	}
}

// TestRegistry_RoomsAreIsolated confirms participants and hosts are keyed by
// room — a message in one room never sees another room's members.
func TestRegistry_RoomsAreIsolated(t *testing.T) {
	r := NewRegistry()
	a := &fakeSender{}
	b := &fakeSender{}
	_, _ = r.RegisterParticipant("A", a, Participant{UserID: "a", Role: RoleParticipant})
	_, _ = r.RegisterParticipant("B", b, Participant{UserID: "b", Role: RoleParticipant})
	if got := len(r.ParticipantsFor("A")); got != 1 {
		t.Fatalf("room A should have 1 participant, got %d", got)
	}
	if got := len(r.ParticipantsFor("B")); got != 1 {
		t.Fatalf("room B should have 1 participant, got %d", got)
	}
}

// TestRegistry_HostSendersFor confirms only role=host participants are returned
// — the gate that keeps permission prompts away from guests.
func TestRegistry_HostSendersFor(t *testing.T) {
	r := NewRegistry()
	guest := &fakeSender{}
	operator := &fakeSender{}
	_, _ = r.RegisterParticipant("room", guest, Participant{UserID: "guest:bob-x7k2", Role: RoleParticipant})
	_, _ = r.RegisterParticipant("room", operator, Participant{UserID: "alice", Role: RoleHost})

	hosts := r.HostSendersFor("room")
	if len(hosts) != 1 || hosts[0] != operator {
		t.Fatalf("HostSendersFor should return only the role=host operator, got %d senders", len(hosts))
	}
	if len(r.ParticipantsFor("room")) != 2 {
		t.Fatalf("ParticipantsFor should still return everyone")
	}
}
