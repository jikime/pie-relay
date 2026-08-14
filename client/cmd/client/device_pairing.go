package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"cli-relay/client/internal/devicecredentials"
)

type pairingExchangeResponse struct {
	DeviceID              string `json:"deviceId"`
	RuntimeDeviceID       string `json:"runtimeDeviceId"`
	DeviceName            string `json:"deviceName"`
	WorkspaceID           string `json:"workspaceId"`
	AccessToken           string `json:"accessToken"`
	AccessTokenExpiresIn  int64  `json:"accessTokenExpiresIn"`
	RefreshToken          string `json:"refreshToken"`
	RefreshTokenExpiresIn int64  `json:"refreshTokenExpiresIn"`
	ControlURL            string `json:"controlUrl"`
	ApplicationID         string `json:"applicationId"`
	PoolID                string `json:"poolId"`
}

func runDevicePair(args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	server := fs.String("server", pairingServiceURL(), "일회용 코드를 발급한 장치 연결 서비스의 HTTP(S) 주소")
	code := fs.String("code", "", "10분 동안 유효한 1회용 연결 코드")
	name := fs.String("name", "", "실행 장치 표시 이름")
	credentialsPath := fs.String("credentials", "", "장치 자격 파일 경로")
	_ = fs.Parse(args)
	path, err := resolveDeviceCredentialsPath(*credentialsPath)
	if err != nil {
		fatalPair(err)
	}
	if err := ensureDeviceRuntimeStopped(path); err != nil {
		fatalPair(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	credentials, err := pairDevice(ctx, http.DefaultClient, *server, *code, *name, path)
	if err != nil {
		fatalPair(err)
	}
	printPairedDevice(credentials, path, false)
}

func pairDevice(
	ctx context.Context,
	client *http.Client,
	server string,
	code string,
	name string,
	credentialsPath string,
) (devicecredentials.Credentials, error) {
	origin, err := normalizedHTTPOrigin(server)
	if err != nil {
		return devicecredentials.Credentials{}, err
	}
	if strings.TrimSpace(code) == "" {
		return devicecredentials.Credentials{}, errors.New("--code 연결 코드가 필요합니다")
	}
	value, err := exchangePairing(ctx, client, origin, map[string]any{
		"code":     code,
		"name":     name,
		"platform": runtime.GOOS + "/" + runtime.GOARCH,
		"version":  currentClientVersion().Version,
	})
	if err != nil {
		return devicecredentials.Credentials{}, err
	}
	now := time.Now().UTC()
	credentials := devicecredentials.Credentials{
		BuilderURL:            origin,
		ControlURL:            value.ControlURL,
		DeviceID:              value.DeviceID,
		RuntimeDeviceID:       value.RuntimeDeviceID,
		WorkspaceID:           value.WorkspaceID,
		ApplicationID:         value.ApplicationID,
		PoolID:                value.PoolID,
		Name:                  value.DeviceName,
		AccessToken:           value.AccessToken,
		AccessTokenExpiresAt:  now.Add(time.Duration(value.AccessTokenExpiresIn) * time.Second),
		RefreshToken:          value.RefreshToken,
		RefreshTokenExpiresAt: now.Add(time.Duration(value.RefreshTokenExpiresIn) * time.Second),
	}
	if err := devicecredentials.Save(credentialsPath, credentials); err != nil {
		return devicecredentials.Credentials{}, fmt.Errorf("장치 자격 저장 실패: %w", err)
	}
	return credentials, nil
}

func printPairedDevice(credentials devicecredentials.Credentials, credentialsPath string, startsNow bool) {
	fmt.Printf("실행 장치를 안전하게 연결했습니다: %s\n", credentials.DeviceID)
	fmt.Printf("자격 파일: %s\n", credentialsPath)
	if startsNow {
		fmt.Println("Pie Client를 시작합니다. 이 터미널을 닫으면 Agent 실행도 중지됩니다.")
		return
	}
	fmt.Println("이제 pie-client start를 실행하면 Access token을 자동 갱신하며 작업을 기다립니다.")
}

func resolveDeviceCredentialsPath(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	return devicecredentials.DefaultPath()
}

func fatalPair(err error) {
	fmt.Fprintln(os.Stderr, "장치 연결 실패:", err)
	os.Exit(1)
}

func exchangePairing(ctx context.Context, client *http.Client, builderURL string, body any) (pairingExchangeResponse, error) {
	var value pairingExchangeResponse
	if err := requestPairingJSON(ctx, client, builderURL+"/api/agent-runtimes/pairings/exchange", body, &value); err != nil {
		return value, err
	}
	if value.DeviceID == "" || value.ControlURL == "" || value.AccessToken == "" || value.RefreshToken == "" {
		return value, errors.New("장치 연결 서비스가 불완전한 장치 자격을 반환했습니다")
	}
	return value, nil
}

func normalizedHTTPOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return "", errors.New("--server에는 일회용 코드를 발급한 서비스의 올바른 HTTP(S) 주소를 입력해 주세요")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func requestPairingJSON(ctx context.Context, client *http.Client, endpoint string, body any, output any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusNotFound {
			return errors.New("장치 연결 API를 찾을 수 없습니다. --server에는 Relay/Manager가 아니라 연결 코드를 발급한 제품 주소를 입력해 주세요")
		}
		var detail struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &detail) == nil && detail.Error != "" {
			return errors.New(detail.Error)
		}
		return fmt.Errorf("장치 연결 서비스 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output)
}

func pairingServiceURL() string {
	if value := strings.TrimSpace(os.Getenv("PIE_PAIRING_URL")); value != "" {
		return value
	}
	// 기존 설치 환경과의 호환을 위해 한동안 예전 변수도 읽는다.
	return strings.TrimSpace(os.Getenv("PIE_CANVAS_URL"))
}

type deviceCredentialSource struct {
	path   string
	client *http.Client
	mu     sync.Mutex
	value  devicecredentials.Credentials
}

func loadDeviceCredentialSource(path string) (*deviceCredentialSource, error) {
	value, err := devicecredentials.Load(path)
	if err != nil {
		return nil, err
	}
	return &deviceCredentialSource{path: path, client: &http.Client{Timeout: 15 * time.Second}, value: value}, nil
}

func (s *deviceCredentialSource) Device() (controlURL, deviceID, deviceName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value.ControlURL, s.value.DeviceID, s.value.Name
}

func (s *deviceCredentialSource) Token(ctx context.Context, force bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !force && s.value.AccessToken != "" && time.Until(s.value.AccessTokenExpiresAt) > 2*time.Minute {
		return s.value.AccessToken, nil
	}
	if s.value.RefreshToken == "" || (!s.value.RefreshTokenExpiresAt.IsZero() && time.Now().After(s.value.RefreshTokenExpiresAt)) {
		return "", errors.New("장치 갱신 자격이 만료되었습니다. pie-client pair를 다시 실행해 주세요")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var refreshed pairingExchangeResponse
	err := requestPairingJSON(ctx, s.client, strings.TrimRight(s.value.BuilderURL, "/")+"/api/agent-runtimes/token/refresh", map[string]string{
		"refreshToken": s.value.RefreshToken,
	}, &refreshed)
	if err != nil {
		return "", fmt.Errorf("장치 Access token 갱신 실패: %w", err)
	}
	now := time.Now().UTC()
	s.value.AccessToken = refreshed.AccessToken
	s.value.AccessTokenExpiresAt = now.Add(time.Duration(refreshed.AccessTokenExpiresIn) * time.Second)
	s.value.RefreshToken = refreshed.RefreshToken
	s.value.RefreshTokenExpiresAt = now.Add(time.Duration(refreshed.RefreshTokenExpiresIn) * time.Second)
	if refreshed.ControlURL != "" {
		s.value.ControlURL = refreshed.ControlURL
	}
	if err := devicecredentials.Save(s.path, s.value); err != nil {
		return "", fmt.Errorf("갱신된 장치 자격 저장 실패: %w", err)
	}
	return s.value.AccessToken, nil
}
