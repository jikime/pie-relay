package credentials

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")

	c := Credentials{
		AccessToken:          "at-1",
		AccessTokenExpiresAt: time.Now().Add(time.Hour).Unix(),
		DeviceID:             "device-1",
		UserID:               "user-1",
	}
	if err := SaveTo(path, c); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("got perm %v, want 0600", info.Mode().Perm())
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.AccessToken != c.AccessToken || loaded.UserID != c.UserID {
		t.Fatalf("round-trip mismatch: %+v vs %+v", loaded, c)
	}
}

func TestSaveTo_FixesLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")

	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	c := Credentials{AccessToken: "at-1"}
	if err := SaveTo(path, c); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("got perm %v, want 0600 (SaveTo must tighten pre-existing loose permissions)", info.Mode().Perm())
	}
}

func TestLoadFrom_MissingFile(t *testing.T) {
	_, err := LoadFrom(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestLoadFrom_IgnoresLegacyFields ensures a credentials.json written by the old
// browser-login flow (with vibeUrl/refreshToken keys) still loads — the daemon
// only needs accessToken.
func TestLoadFrom_IgnoresLegacyFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	legacy := `{"vibeUrl":"http://localhost:6556","accessToken":"at-legacy","refreshToken":"rt-1","deviceId":"d1"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}
	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	if loaded.AccessToken != "at-legacy" {
		t.Fatalf("expected accessToken at-legacy, got %s", loaded.AccessToken)
	}
}
