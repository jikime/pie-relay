package previewtoken

import (
	"strings"
	"testing"
	"time"
)

func TestIssueAndVerifyBindsPreviewHostAndType(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	token, err := Issue(secret, Claims{Type: "launch", PreviewID: "preview-a", Hostname: "p-example.preview.kroot.io", AccessVersion: 3}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := Verify(secret, token, "launch", time.Now())
	if err != nil || claims.PreviewID != "preview-a" || claims.Hostname != "p-example.preview.kroot.io" || claims.AccessVersion != 3 {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	if _, err := Verify(secret, token, "session", time.Now()); err == nil {
		t.Fatal("launch token was accepted as a session token")
	}
	parts := strings.Split(token, ".")
	parts[1] = strings.Repeat("A", len(parts[1]))
	if _, err := Verify(secret, strings.Join(parts, "."), "launch", time.Now()); err == nil {
		t.Fatal("tampered token was accepted")
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	secret := []byte(strings.Repeat("x", 32))
	token, err := Issue(secret, Claims{Type: "launch", PreviewID: "preview-a", Hostname: "p-example.preview.kroot.io"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(secret, token, "launch", time.Now().Add(2*time.Second)); err == nil {
		t.Fatal("expired token was accepted")
	}
}
