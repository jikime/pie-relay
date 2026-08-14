package auth

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestDeviceJWTScopesPrincipal(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	secret := []byte("0123456789abcdef0123456789abcdef")
	claims := deviceClaims{
		DeviceID:       "device-a",
		WorkspaceID:    "workspace-a",
		OrganizationID: "workspace-a",
		Scope:          "pie:device",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "owner-a",
			Issuer:    "pie-canvas",
			Audience:  jwt.ClaimStrings{"pie-control-device"},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := (DeviceJWT{Secret: secret, Now: func() time.Time { return now.Add(time.Minute) }}).Verify(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.UserID != "owner-a" || principal.OrganizationID != "workspace-a" || principal.DeviceID != "device-a" || !principal.IsDevice() {
		t.Fatalf("unexpected principal: %#v", principal)
	}
	if principal.CanOperate() || principal.CanAdminister() {
		t.Fatal("device principal inherited operator privileges")
	}
}

func TestDeviceJWTRejectsWrongAudienceAndExpiry(t *testing.T) {
	now := time.Now().UTC()
	secret := []byte("0123456789abcdef0123456789abcdef")
	for name, test := range map[string]struct {
		audience string
		expires  time.Time
	}{
		"audience": {audience: "other", expires: now.Add(time.Minute)},
		"expired":  {audience: "pie-control-device", expires: now.Add(-time.Minute)},
	} {
		t.Run(name, func(t *testing.T) {
			claims := deviceClaims{DeviceID: "device-a", WorkspaceID: "workspace-a", Scope: "pie:device", RegisteredClaims: jwt.RegisteredClaims{Subject: "owner-a", Issuer: "pie-canvas", Audience: jwt.ClaimStrings{test.audience}, ExpiresAt: jwt.NewNumericDate(test.expires)}}
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (DeviceJWT{Secret: secret, Now: func() time.Time { return now }}).Verify(context.Background(), token); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}
