package relay

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type mobileStubAuth struct{}

func (mobileStubAuth) AgentUser(_ context.Context, _ string) (Identity, error) {
	return Identity{UserID: "owner", Role: RoleHost}, nil
}

type scopedMobileStubAuth struct{}

func (scopedMobileStubAuth) AgentUser(_ context.Context, _ string) (Identity, error) {
	return Identity{
		UserID: "member-a", Role: RoleHost, ApplicationID: "pie-mobile",
		PoolID: "pie-mobile-seoul", TenantID: "workspace-a",
	}, nil
}

func TestMobileRelayIdentityUsesVerifiedClaims(t *testing.T) {
	mobile, err := NewMobileRelay(MobileRelayOptions{Auth: scopedMobileStubAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/identity", nil)
	request.Header.Set("Authorization", "Bearer opaque-token")
	response := httptest.NewRecorder()
	mobile.handleIdentity(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{`"userId":"member-a"`, `"profileId":"pie-mobile"`, `"organizationId":"workspace-a"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("identity response missing %s: %s", expected, body)
		}
	}
}

func TestNewMobileRelayValidatesPublicOrigin(t *testing.T) {
	for _, invalid := range []string{"ws://relay.cookai.dev", "https://relay.cookai.dev/path", "not-a-url"} {
		if _, err := NewMobileRelay(MobileRelayOptions{Auth: mobileStubAuth{}, PublicURL: invalid}); err == nil {
			t.Fatalf("accepted invalid public URL %q", invalid)
		}
	}
	if _, err := NewMobileRelay(MobileRelayOptions{Auth: mobileStubAuth{}, PublicURL: "https://relay.cookai.dev"}); err != nil {
		t.Fatalf("rejected valid public URL: %v", err)
	}
}

func TestMobileRelayRandomFailureFailsClosed(t *testing.T) {
	relay, err := NewMobileRelay(MobileRelayOptions{Auth: mobileStubAuth{}, Random: failingMobileReader{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay.randomBase64URL(32); err == nil {
		t.Fatal("mobile credential was generated after random source failure")
	}
}

type failingMobileReader struct{}

func (failingMobileReader) Read([]byte) (int, error) {
	return 0, errors.New("random source unavailable")
}

func TestMobileRelayRandomTokenHasCanonicalLength(t *testing.T) {
	relay, err := NewMobileRelay(MobileRelayOptions{
		Auth: mobileStubAuth{}, Random: io.LimitReader(strings.NewReader(strings.Repeat("x", 32)), 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := relay.randomBase64URL(32)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 || len(token) != 43 {
		t.Fatalf("token=%q bytes=%d err=%v", token, len(decoded), err)
	}
}

func TestMobileRelayStateFileModeFailureIsFailClosedByDefault(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "mobile-state.json")
	relay, err := NewMobileRelay(MobileRelayOptions{Auth: mobileStubAuth{}, StateFile: stateFile})
	if err != nil {
		t.Fatal(err)
	}
	relay.credentials["host"] = map[string]*mobileCredential{"device": {CurrentHash: strings.Repeat("a", 43), CurrentVersion: 1}}
	relay.chmodFile = func(string, os.FileMode) error { return os.ErrPermission }

	if err := relay.saveStateLocked(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("saveStateLocked() error = %v, want permission error", err)
	}
	if _, err := os.Stat(stateFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file should not be committed after hardening failure: %v", err)
	}
}

func TestMobileRelayStateFileCanUseExplicitNonPOSIXVolumeMode(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "mobile-state.json")
	relay, err := NewMobileRelay(MobileRelayOptions{
		Auth:                          mobileStubAuth{},
		StateFile:                     stateFile,
		AllowUnsupportedStateFileMode: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	relay.credentials["host"] = map[string]*mobileCredential{"device": {CurrentHash: strings.Repeat("a", 43), CurrentVersion: 1}}
	relay.chmodFile = func(string, os.FileMode) error { return os.ErrPermission }

	if err := relay.saveStateLocked(); err != nil {
		t.Fatalf("saveStateLocked() error = %v", err)
	}
	if _, err := NewMobileRelay(MobileRelayOptions{Auth: mobileStubAuth{}, StateFile: stateFile}); err != nil {
		t.Fatalf("persisted state cannot be loaded: %v", err)
	}
}
