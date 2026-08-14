package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var ErrUnauthorized = errors.New("unauthorized")

type Principal struct {
	UserID         string
	OrganizationID string
	Admin          bool
	Roles          []string
	DeviceID       string
}

func (p Principal) IsDevice() bool { return p.DeviceID != "" }

func (p Principal) HasRole(want string) bool {
	if p.Admin {
		return true
	}
	for _, role := range p.Roles {
		if role == want {
			return true
		}
	}
	return false
}

func (p Principal) CanViewAdmin() bool {
	return p.Admin || p.HasRole("pie-admin-viewer") || p.HasRole("pie-operator") || p.HasRole("pie-admin") || p.HasRole("pie-security-admin")
}

func (p Principal) CanOperate() bool {
	return p.Admin || p.HasRole("pie-operator") || p.HasRole("pie-admin") || p.HasRole("pie-security-admin")
}

func (p Principal) CanAdminister() bool {
	return p.Admin || p.HasRole("pie-admin") || p.HasRole("pie-security-admin")
}

func (p Principal) CanReportRelayPresence() bool {
	return p.CanOperate() || p.HasRole("pie-relay-presence")
}

type Verifier interface {
	Verify(context.Context, string) (Principal, error)
}

// DeviceJWT verifies the short-lived access token issued by Pie Canvas after
// a clientd device pairing or refresh. The token is deliberately scoped to one
// device and cannot inherit user/operator privileges.
type DeviceJWT struct {
	Secret   []byte
	Issuer   string
	Audience string
	Now      func() time.Time
}

type deviceClaims struct {
	DeviceID       string `json:"deviceId"`
	WorkspaceID    string `json:"workspaceId"`
	OrganizationID string `json:"organizationId"`
	Scope          string `json:"scope"`
	jwt.RegisteredClaims
}

func (v DeviceJWT) Verify(_ context.Context, token string) (Principal, error) {
	if token == "" || len(v.Secret) < 32 {
		return Principal{}, ErrUnauthorized
	}
	issuer := v.Issuer
	if issuer == "" {
		issuer = "pie-canvas"
	}
	audience := v.Audience
	if audience == "" {
		audience = "pie-control-device"
	}
	options := []jwt.ParserOption{
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
	}
	if v.Now != nil {
		options = append(options, jwt.WithTimeFunc(v.Now))
	}
	claims := &deviceClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(parsed *jwt.Token) (any, error) {
		if parsed.Method != jwt.SigningMethodHS256 {
			return nil, ErrUnauthorized
		}
		return v.Secret, nil
	}, options...)
	if err != nil || !parsed.Valid || claims.Subject == "" || claims.DeviceID == "" || claims.Scope != "pie:device" {
		return Principal{}, ErrUnauthorized
	}
	organizationID := claims.OrganizationID
	if organizationID == "" {
		organizationID = claims.WorkspaceID
	}
	if organizationID == "" {
		return Principal{}, ErrUnauthorized
	}
	return Principal{
		UserID:         claims.Subject,
		OrganizationID: organizationID,
		DeviceID:       claims.DeviceID,
	}, nil
}

type Static struct {
	Token     string
	Principal Principal
}

func (s Static) Verify(_ context.Context, token string) (Principal, error) {
	if s.Token == "" || len(token) != len(s.Token) || subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) != 1 {
		return Principal{}, ErrUnauthorized
	}
	p := s.Principal
	if p.UserID == "" && p.OrganizationID == "" && !p.Admin && len(p.Roles) == 0 {
		p.Admin = true
	}
	p.Roles = append([]string(nil), p.Roles...)
	return p, nil
}

type Introspection struct {
	URL          string
	Client       *http.Client
	ClientID     string
	ClientSecret string
}

func (v Introspection) Verify(ctx context.Context, token string) (Principal, error) {
	if token == "" || v.URL == "" {
		return Principal{}, ErrUnauthorized
	}
	form := url.Values{"token": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return Principal{}, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	if v.ClientID != "" {
		req.SetBasicAuth(v.ClientID, v.ClientSecret)
	}
	client := v.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Principal{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Principal{}, ErrUnauthorized
	}
	var out struct {
		Active         bool     `json:"active"`
		Sub            string   `json:"sub"`
		OrganizationID string   `json:"organizationId"`
		Roles          []string `json:"roles"`
		Scope          string   `json:"scope"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil || !out.Active || out.Sub == "" {
		return Principal{}, ErrUnauthorized
	}
	roles := append([]string(nil), out.Roles...)
	for _, scope := range strings.Fields(out.Scope) {
		switch scope {
		case "pie:admin:view":
			roles = append(roles, "pie-admin-viewer")
		case "pie:operate":
			roles = append(roles, "pie-operator")
		case "pie:admin":
			roles = append(roles, "pie-admin")
		case "pie:security":
			roles = append(roles, "pie-security-admin")
		}
	}
	return Principal{UserID: out.Sub, OrganizationID: out.OrganizationID, Roles: uniqueStrings(roles)}, nil
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

type Chain []Verifier

func (c Chain) Verify(ctx context.Context, token string) (Principal, error) {
	for _, v := range c {
		p, err := v.Verify(ctx, token)
		if err == nil {
			return p, nil
		}
	}
	return Principal{}, ErrUnauthorized
}

type cacheEntry struct {
	principal Principal
	err       error
	expires   time.Time
}
type Cached struct {
	Next        Verifier
	TTL         time.Duration
	NegativeTTL time.Duration
	mu          sync.Mutex
	entries     map[[32]byte]cacheEntry
	inflight    map[[32]byte]*verifyCall
	Now         func() time.Time
}

type verifyCall struct {
	done      chan struct{}
	principal Principal
	err       error
}

func (c *Cached) Verify(ctx context.Context, token string) (Principal, error) {
	if c.Next == nil {
		return Principal{}, ErrUnauthorized
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	key := sha256.Sum256([]byte(token))
	current := now()
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && current.Before(e.expires) {
		c.mu.Unlock()
		return e.principal, e.err
	}
	if call := c.inflight[key]; call != nil {
		done := call.done
		c.mu.Unlock()
		select {
		case <-done:
			return call.principal, call.err
		case <-ctx.Done():
			return Principal{}, ctx.Err()
		}
	}
	if c.inflight == nil {
		c.inflight = map[[32]byte]*verifyCall{}
	}
	call := &verifyCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()
	p, err := c.Next.Verify(ctx, token)
	ttl := c.TTL
	if err != nil {
		ttl = c.NegativeTTL
	}
	c.mu.Lock()
	if ttl > 0 && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		if c.entries == nil {
			c.entries = map[[32]byte]cacheEntry{}
		}
		c.entries[key] = cacheEntry{principal: p, err: err, expires: current.Add(ttl)}
		if len(c.entries) > 4096 {
			for k, e := range c.entries {
				if !current.Before(e.expires) {
					delete(c.entries, k)
				}
			}
			for k := range c.entries {
				if len(c.entries) <= 4096 {
					break
				}
				delete(c.entries, k)
			}
		}
	}
	call.principal = p
	call.err = err
	delete(c.inflight, key)
	close(call.done)
	c.mu.Unlock()
	return p, err
}
