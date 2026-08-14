package relay

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Invite TTLs. The invite code is short-lived; the guest token it mints lasts
// long enough for a working session but not indefinitely.
const (
	inviteTTL   = 15 * time.Minute
	guestExpTTL = 12 * time.Hour
	// Bounds stale/in-use invite memory even when authorized clients create
	// codes faster than they are redeemed. Expired entries are pruned before
	// capacity is evaluated.
	defaultMaxPendingInvites = 100_000
)

var errInviteCapacity = errors.New("invite capacity reached")

// inviteCodeLen / guestSuffixLen size the two random tokens: an 8-char code a
// host reads out loud, and a 4-char suffix that disambiguates guests who join
// under the same display name.
const (
	inviteCodeLen  = 8
	guestSuffixLen = 4
)

// inviteEntry is a pending invite: which room it admits into, the access grade
// (view/control) it grants, and when it dies.
type inviteEntry struct {
	scope     Identity
	access    string
	expiresAt time.Time
}

// Inviter issues room invite codes and mints participant (guest) tokens. Codes
// live in memory with a TTL — a deliberate, documented break from the relay's
// otherwise stateless design, acceptable under the same single-instance
// assumption the Registry already makes. auth both verifies the host's JWT on
// /rooms/invites and SIGNS the guest JWT on /rooms/join (same shared secret).
type Inviter struct {
	auth JWTAuth

	mu        sync.Mutex
	codes     map[string]inviteEntry
	now       func() time.Time // swappable for tests
	max       int
	lastPrune time.Time
}

// NewInviter returns an Inviter backed by auth for verification and minting.
func NewInviter(auth JWTAuth) *Inviter {
	return &Inviter{auth: auth, codes: map[string]inviteEntry{}, now: time.Now, max: defaultMaxPendingInvites}
}

// createInviteRequest is the OPTIONAL body of POST /rooms/invites. access
// grades the invite: "view" (spectate) or "control" (input; terminal driver
// eligible). Absent/empty defaults to view; unrecognized values fail closed.
type createInviteRequest struct {
	Access string `json:"access"`
}

type createInviteResponse struct {
	Code      string `json:"code"`
	Access    string `json:"access"`    // resolved grade (view|control) — for the UI badge
	ExpiresAt int64  `json:"expiresAt"` // unix seconds
}

// handleCreateInvite: POST /rooms/invites with a host Bearer JWT. Mints an
// invite code for the caller's room (room = token's room, which falls back to
// sub for legacy tokens). Guests (role=participant) may not create invites.
func (in *Inviter) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok := bearerToken(r)
	if tok == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	id, err := in.auth.Verify(tok)
	if err != nil || id.UserID == "" {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	// Only a host (or a legacy token, whose empty role is a host PAT) may
	// invite — a guest must not be able to widen their own room.
	if id.Role == RoleParticipant {
		http.Error(w, "participant may not create invites", http.StatusForbidden)
		return
	}
	if !id.Allows(CapabilityInviteCreate) {
		http.Error(w, "token cannot create invites", http.StatusForbidden)
		return
	}

	// Body is optional, but malformed JSON and unknown access grades must not
	// accidentally mint a control credential.
	var req createInviteRequest
	if err := decodeJSONRequest(w, r, &req, 4<<10, true); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	access := req.Access
	if access == "" {
		access = AccessView
	}
	if access != AccessView && access != AccessControl {
		http.Error(w, "invalid access", http.StatusBadRequest)
		return
	}

	expiresAt := in.now().Add(inviteTTL)
	code, err := in.storeInvite(inviteEntry{scope: id, access: access, expiresAt: expiresAt})
	if errors.Is(err, errInviteCapacity) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "invite capacity reached", http.StatusTooManyRequests)
		return
	}
	if err != nil {
		http.Error(w, "could not generate code", http.StatusInternalServerError)
		return
	}

	writeJSON(w, createInviteResponse{Code: code, Access: access, ExpiresAt: expiresAt.Unix()})
}

func (in *Inviter) storeInvite(entry inviteEntry) (string, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	now := in.now()
	if in.lastPrune.IsZero() || now.Sub(in.lastPrune) >= time.Minute || len(in.codes) >= in.max {
		for code, existing := range in.codes {
			if !now.Before(existing.expiresAt) {
				delete(in.codes, code)
			}
		}
		in.lastPrune = now
	}
	if in.max <= 0 || len(in.codes) >= in.max {
		return "", errInviteCapacity
	}
	for attempts := 0; attempts < 8; attempts++ {
		code, err := randCode(inviteCodeLen)
		if err != nil {
			return "", err
		}
		if _, exists := in.codes[code]; exists {
			continue
		}
		in.codes[code] = entry
		return code, nil
	}
	return "", errors.New("could not allocate unique invite code")
}

type joinRequest struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type joinResponse struct {
	Token           string `json:"token"`
	Room            string `json:"room"`
	DeviceID        string `json:"deviceId,omitempty"`
	SessionID       string `json:"sessionId,omitempty"`
	ExecutionTarget string `json:"executionTarget,omitempty"`
	RelayNode       string `json:"relayNode,omitempty"`
	RelayGeneration int64  `json:"relayGeneration,omitempty"`
}

// handleJoin: POST /rooms/join with {code, name}. Resolves the (unexpired)
// code to its room and mints a participant token whose sub is a fresh guest
// identity (guest:<name>-<rand4>). Codes are multi-use within their TTL — a
// group can share one code — so this does NOT consume the entry.
func (in *Inviter) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req joinRequest
	if err := decodeJSONRequest(w, r, &req, 4<<10, false); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	entry, ok := in.resolve(req.Code)
	if !ok {
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
		return
	}
	suffix, err := randCode(guestSuffixLen)
	if err != nil {
		http.Error(w, "could not mint token", http.StatusInternalServerError)
		return
	}
	sub := "guest:" + sanitizeName(req.Name) + "-" + strings.ToLower(suffix)
	token, err := in.auth.MintInviteParticipant(sub, entry.scope, entry.access, guestExpTTL)
	if err != nil {
		http.Error(w, "could not mint token", http.StatusInternalServerError)
		return
	}
	writeJSON(w, joinResponse{Token: token, Room: entry.scope.Room, DeviceID: entry.scope.DeviceID, SessionID: entry.scope.SessionID, ExecutionTarget: entry.scope.ExecutionTarget, RelayNode: entry.scope.RelayNode, RelayGeneration: entry.scope.RelayGeneration})
}

// resolve returns the room and access grade for a live invite code, dropping it
// if expired.
func (in *Inviter) resolve(code string) (entry inviteEntry, ok bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	e, found := in.codes[code]
	if !found {
		return inviteEntry{}, false
	}
	if in.now().After(e.expiresAt) {
		delete(in.codes, code)
		return inviteEntry{}, false
	}
	return e, true
}

// codeAlphabet excludes visually ambiguous characters (0/O, 1/I/L) so a host
// can read a code aloud without confusion.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// randCode returns an n-char code drawn uniformly from codeAlphabet using
// crypto/rand (rejection sampling avoids modulo bias).
func randCode(n int) (string, error) {
	out := make([]byte, n)
	buf := make([]byte, 1)
	const max = 256 - (256 % len(codeAlphabet))
	for i := 0; i < n; {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		if int(buf[0]) >= max {
			continue // reject the biased tail
		}
		out[i] = codeAlphabet[int(buf[0])%len(codeAlphabet)]
		i++
	}
	return string(out), nil
}

// sanitizeName keeps a display name usable inside a JWT sub: letters, digits,
// '-' and '_' only, lowercased, capped, and never empty.
func sanitizeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
		if b.Len() >= 32 {
			break
		}
	}
	if b.Len() == 0 {
		return "guest"
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
