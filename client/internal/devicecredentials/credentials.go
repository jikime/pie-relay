package devicecredentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Credentials struct {
	BuilderURL            string    `json:"builderUrl"`
	ControlURL            string    `json:"controlUrl"`
	DeviceID              string    `json:"deviceId"`
	RuntimeDeviceID       string    `json:"runtimeDeviceId,omitempty"`
	WorkspaceID           string    `json:"workspaceId"`
	ApplicationID         string    `json:"applicationId"`
	PoolID                string    `json:"poolId"`
	Name                  string    `json:"name,omitempty"`
	AccessToken           string    `json:"accessToken"`
	AccessTokenExpiresAt  time.Time `json:"accessTokenExpiresAt"`
	RefreshToken          string    `json:"refreshToken"`
	RefreshTokenExpiresAt time.Time `json:"refreshTokenExpiresAt"`
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("사용자 홈 디렉터리를 찾을 수 없습니다")
	}
	return filepath.Join(home, ".cli-relay", "device-credentials.json"), nil
}

func Load(path string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	var value Credentials
	if err := json.Unmarshal(data, &value); err != nil {
		return Credentials{}, fmt.Errorf("장치 자격 파일을 읽을 수 없습니다: %w", err)
	}
	if value.BuilderURL == "" || value.ControlURL == "" || value.DeviceID == "" || value.RefreshToken == "" {
		return Credentials{}, errors.New("장치 자격 파일에 필수 정보가 없습니다")
	}
	return value, nil
}

func Save(path string, value Credentials) error {
	if path == "" {
		return errors.New("장치 자격 파일 경로가 비어 있습니다")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".device-credentials-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func Remove(path string) error {
	if path == "" {
		return errors.New("장치 자격 파일 경로가 비어 있습니다")
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
