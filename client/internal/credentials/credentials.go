// Package credentials reads the CLI's stored host token
// (~/.cli-relay/credentials.json, 0600).
//
// This file is now a legacy/manual artifact: the browser-login flow that used
// to write it (and the vibe-canvas refresh path that kept its access token
// fresh) has been removed. The daemon's primary auth is RELAY_TICKET (a host
// token issued by the desktop app's "방 만들기"). When RELAY_TICKET is unset and
// this file exists, the daemon uses its accessToken like a manual ticket. There
// is no refresh: an expired token means the host must re-enroll from the app.
package credentials

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Credentials is the on-disk shape read by the daemon. Only AccessToken is
// load-bearing today; the remaining fields are informational and tolerated for
// backward compatibility with files written by the old login flow.
type Credentials struct {
	AccessToken          string `json:"accessToken"`
	AccessTokenExpiresAt int64  `json:"accessTokenExpiresAt"` // unix seconds
	DeviceID             string `json:"deviceId"`
	UserID               string `json:"userId"`
}

// DefaultPath returns ~/.cli-relay/credentials.json.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cli-relay", "credentials.json"), nil
}

// SaveTo writes credentials as 0600 JSON, creating parent directories. The
// daemon no longer writes credentials.json itself; this remains for callers
// that stage a token file manually.
func SaveTo(path string, c Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// LoadFrom reads and parses a credentials file.
func LoadFrom(path string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return Credentials{}, err
	}
	return c, nil
}
