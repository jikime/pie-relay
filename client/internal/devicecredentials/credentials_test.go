package devicecredentials

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSaveLoadUsesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "device.json")
	want := Credentials{BuilderURL: "https://canvas.example", ControlURL: "https://control.example", DeviceID: "device-a", WorkspaceID: "workspace-a", RefreshToken: "refresh", AccessTokenExpiresAt: time.Now().UTC().Truncate(time.Second)}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeviceID != want.DeviceID || got.RefreshToken != want.RefreshToken {
		t.Fatalf("unexpected credentials: %#v", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("credential mode = %o", info.Mode().Perm())
		}
	}
}
