package previewtoken

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const Audience = "pie-preview"

type Claims struct {
	Audience      string `json:"aud"`
	Type          string `json:"typ"`
	PreviewID     string `json:"previewId"`
	Hostname      string `json:"hostname"`
	AccessVersion int64  `json:"accessVersion,omitempty"`
	Nonce         string `json:"nonce"`
	ExpiresAt     int64  `json:"exp"`
}

func Issue(secret []byte, claims Claims, ttl time.Duration) (string, error) {
	if len(secret) < 32 || ttl <= 0 || claims.PreviewID == "" || claims.Hostname == "" || claims.Type != "launch" && claims.Type != "session" {
		return "", errors.New("invalid preview token configuration")
	}
	claims.Audience = Audience
	claims.Hostname = strings.ToLower(strings.TrimSpace(claims.Hostname))
	claims.ExpiresAt = time.Now().Add(ttl).UTC().Unix()
	if claims.Nonce == "" {
		var nonce [16]byte
		if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
			return "", err
		}
		claims.Nonce = base64.RawURLEncoding.EncodeToString(nonce[:])
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func Verify(secret []byte, token string, expectedType string, now time.Time) (Claims, error) {
	if len(secret) < 32 {
		return Claims{}, errors.New("preview token secret is unavailable")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Claims{}, errors.New("invalid preview token")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("invalid preview token")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return Claims{}, errors.New("invalid preview token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, errors.New("invalid preview token")
	}
	var claims Claims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&claims) != nil || claims.Audience != Audience || claims.Type != expectedType || claims.PreviewID == "" || claims.Hostname == "" || claims.Nonce == "" || claims.ExpiresAt <= now.UTC().Unix() {
		return Claims{}, errors.New("invalid or expired preview token")
	}
	return claims, nil
}
