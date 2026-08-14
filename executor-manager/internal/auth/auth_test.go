package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingVerifier struct{ calls int }

func (v *countingVerifier) Verify(context.Context, string) (Principal, error) {
	v.calls++
	return Principal{UserID: "u"}, nil
}

type blockingVerifier struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (v *blockingVerifier) Verify(context.Context, string) (Principal, error) {
	v.calls.Add(1)
	v.once.Do(func() { close(v.started) })
	<-v.release
	return Principal{UserID: "u"}, nil
}

func TestStatic(t *testing.T) {
	p, err := (Static{Token: "secret"}).Verify(context.Background(), "secret")
	if err != nil || !p.Admin {
		t.Fatalf("principal=%+v err=%v", p, err)
	}
	if _, err = (Static{Token: "secret"}).Verify(context.Background(), "bad"); err == nil {
		t.Fatal("expected rejection")
	}
}

func TestStaticCanBeScopedToRelayPresence(t *testing.T) {
	p, err := (Static{Token: "presence", Principal: Principal{Roles: []string{"pie-relay-presence"}}}).Verify(context.Background(), "presence")
	if err != nil || !p.CanReportRelayPresence() || p.CanOperate() || p.Admin {
		t.Fatalf("principal=%+v err=%v", p, err)
	}
}
func TestIntrospection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("token") != "pat-ok" {
			http.Error(w, "bad", 401)
			return
		}
		_, _ = w.Write([]byte(`{"active":true,"sub":"user-1","organizationId":"org-1"}`))
	}))
	defer srv.Close()
	v := Introspection{URL: srv.URL, Client: &http.Client{Timeout: time.Second}}
	p, err := v.Verify(context.Background(), "pat-ok")
	if err != nil || p.UserID != "user-1" || p.OrganizationID != "org-1" {
		t.Fatalf("principal=%+v err=%v", p, err)
	}
}

func TestIntrospectionMapsRolesAndScopes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"active": true, "sub": "operator-1", "organizationId": "org-1",
			"roles": []string{"pie-admin-viewer", "pie-admin-viewer"},
			"scope": "openid pie:operate",
		})
	}))
	defer server.Close()
	verifier := Introspection{URL: server.URL, Client: server.Client()}
	principal, err := verifier.Verify(context.Background(), "pat")
	if err != nil {
		t.Fatal(err)
	}
	if !principal.CanViewAdmin() || !principal.CanOperate() || principal.CanAdminister() {
		t.Fatalf("unexpected permissions: %#v", principal)
	}
	if len(principal.Roles) != 2 {
		t.Fatalf("roles were not deduplicated: %#v", principal.Roles)
	}
}

func TestPrincipalRoleHierarchy(t *testing.T) {
	if !(Principal{Admin: true}).CanAdminister() {
		t.Fatal("static admin should have every permission")
	}
	viewer := Principal{Roles: []string{"pie-admin-viewer"}}
	if !viewer.CanViewAdmin() || viewer.CanOperate() {
		t.Fatal("viewer role permissions are incorrect")
	}
	admin := Principal{Roles: []string{"pie-admin"}}
	if !admin.CanViewAdmin() || !admin.CanOperate() || !admin.CanAdminister() {
		t.Fatal("admin role hierarchy is incorrect")
	}
}

func TestCachedVerifierDoesNotStoreRawTokenOrRepeatLookup(t *testing.T) {
	next := &countingVerifier{}
	now := time.Unix(1, 0)
	cached := &Cached{Next: next, TTL: time.Minute, Now: func() time.Time { return now }}
	for range 2 {
		p, err := cached.Verify(context.Background(), "secret-pat")
		if err != nil || p.UserID != "u" {
			t.Fatal(err)
		}
	}
	if next.calls != 1 {
		t.Fatalf("calls=%d", next.calls)
	}
	now = now.Add(2 * time.Minute)
	_, _ = cached.Verify(context.Background(), "secret-pat")
	if next.calls != 2 {
		t.Fatalf("calls after expiry=%d", next.calls)
	}
}

func TestCachedVerifierCoalescesConcurrentIntrospection(t *testing.T) {
	next := &blockingVerifier{started: make(chan struct{}), release: make(chan struct{})}
	cached := &Cached{Next: next, TTL: time.Minute}
	const requests = 32
	start := make(chan struct{})
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := cached.Verify(context.Background(), "same-pat")
			errs <- err
		}()
	}
	close(start)
	<-next.started
	time.Sleep(20 * time.Millisecond)
	if calls := next.calls.Load(); calls != 1 {
		t.Fatalf("concurrent upstream calls=%d", calls)
	}
	close(next.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
