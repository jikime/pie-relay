package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"cli-relay/client/internal/devicecredentials"
)

const runtimeStateVersion = 1

type runtimeState struct {
	Version         int       `json:"version"`
	PID             int       `json:"pid"`
	StartedAt       time.Time `json:"startedAt"`
	ListenAddress   string    `json:"listenAddress"`
	ControlToken    string    `json:"controlToken"`
	CredentialsPath string    `json:"credentialsPath,omitempty"`
}

type runtimeStatus struct {
	Running   bool      `json:"running"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	Address   string    `json:"address,omitempty"`
}

func runDeviceConnect(args []string) {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	server := fs.String("server", pairingServiceURL(), "일회용 코드를 발급한 장치 연결 서비스의 HTTP(S) 주소")
	code := fs.String("code", "", "10분 동안 유효한 1회용 연결 코드")
	name := fs.String("name", "", "실행 장치 표시 이름")
	credentialsPath := fs.String("credentials", "", "장치 자격 파일 경로")
	_ = fs.Parse(args)
	path, err := resolveDeviceCredentialsPath(*credentialsPath)
	if err != nil {
		fatalLifecycle(err)
	}
	if err := ensureDeviceRuntimeStopped(path); err != nil {
		fatalLifecycle(err)
	}
	if err := prepareACPRuntime(); err != nil {
		fatalLifecycle(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	credentials, err := pairDevice(ctx, http.DefaultClient, *server, *code, *name, path)
	cancel()
	if err != nil {
		fatalLifecycle(err)
	}
	printPairedDevice(credentials, path, true)
	runManagedSessionManager([]string{"--control-mode", "device", "--device-credentials", path}, path)
}

func ensureDeviceRuntimeStopped(credentialsPath string) error {
	status, _ := readRuntimeStatus(credentialsPath)
	if status.Running {
		return fmt.Errorf("Pie Client가 이미 실행 중입니다 (PID %d). 재연결하려면 먼저 pie-client stop을 실행해 주세요", status.PID)
	}
	return nil
}

func runDeviceStart(args []string) {
	sessionArgs, credentialsPath, err := normalizeStartArgs(args)
	if err != nil {
		fatalLifecycle(err)
	}
	if !usesDirectControlCredentials() {
		if _, err := devicecredentials.Load(credentialsPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fatalLifecycle(errors.New("연결된 장치가 없습니다. 먼저 pie-client connect를 실행해 주세요"))
			}
			fatalLifecycle(err)
		}
	}
	if err := prepareACPRuntime(); err != nil {
		fatalLifecycle(err)
	}
	runManagedSessionManager(sessionArgs, credentialsPath)
}

func runDeviceStop(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	credentialsPath := fs.String("credentials", "", "장치 자격 파일 경로")
	wait := fs.Duration("wait", 8*time.Second, "정상 종료 대기 시간")
	_ = fs.Parse(args)
	path, err := resolveDeviceCredentialsPath(*credentialsPath)
	if err != nil {
		fatalLifecycle(err)
	}
	stopped, err := stopManagedRuntime(path, *wait)
	if err != nil {
		fatalLifecycle(err)
	}
	if stopped {
		fmt.Println("Pie Client를 안전하게 중지했습니다.")
		return
	}
	fmt.Println("Pie Client가 실행 중이 아닙니다.")
}

func runDeviceStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	credentialsPath := fs.String("credentials", "", "장치 자격 파일 경로")
	asJSON := fs.Bool("json", false, "JSON 형식으로 출력")
	_ = fs.Parse(args)
	path, err := resolveDeviceCredentialsPath(*credentialsPath)
	if err != nil {
		fatalLifecycle(err)
	}
	status, _ := readRuntimeStatus(path)
	credentials, credentialErr := devicecredentials.Load(path)
	if *asJSON {
		payload := map[string]any{
			"connected": credentialErr == nil,
			"running":   status.Running,
			"pid":       status.PID,
			"startedAt": status.StartedAt,
			"address":   status.Address,
		}
		if credentialErr == nil {
			payload["deviceId"] = credentials.DeviceID
			payload["deviceName"] = credentials.Name
			payload["workspaceId"] = credentials.WorkspaceID
			payload["builderUrl"] = credentials.BuilderURL
		}
		_ = json.NewEncoder(os.Stdout).Encode(payload)
		return
	}
	if credentialErr == nil {
		fmt.Printf("연결: 연결됨 · %s (%s)\n", fallback(credentials.Name, "이름 없는 장치"), credentials.DeviceID)
		fmt.Printf("워크스페이스: %s\n", credentials.WorkspaceID)
		fmt.Printf("장치 연결 서비스: %s\n", credentials.BuilderURL)
	} else if usesDirectControlCredentials() {
		fmt.Println("연결: 환경변수로 관리되는 실행 장치")
	} else {
		fmt.Println("연결: 연결되지 않음")
	}
	if status.Running {
		fmt.Printf("실행: 실행 중 · PID %d · %s\n", status.PID, status.StartedAt.Local().Format("2006-01-02 15:04:05"))
		return
	}
	fmt.Println("실행: 중지됨")
}

func runDeviceDisconnect(args []string) {
	fs := flag.NewFlagSet("disconnect", flag.ExitOnError)
	credentialsPath := fs.String("credentials", "", "장치 자격 파일 경로")
	localOnly := fs.Bool("local-only", false, "서버 장치는 유지하고 이 컴퓨터의 자격만 삭제")
	_ = fs.Parse(args)
	path, err := resolveDeviceCredentialsPath(*credentialsPath)
	if err != nil {
		fatalLifecycle(err)
	}
	_, _ = stopManagedRuntime(path, 8*time.Second)
	credentials, err := devicecredentials.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("연결된 장치가 없습니다.")
			return
		}
		fatalLifecycle(err)
	}
	if !*localOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err = disconnectRemoteDevice(ctx, http.DefaultClient, credentials)
		cancel()
		if err != nil {
			fatalLifecycle(fmt.Errorf("서버 장치 연결 해제 실패: %w (로컬 자격만 지우려면 --local-only 사용)", err))
		}
	}
	if err := devicecredentials.Remove(path); err != nil {
		fatalLifecycle(fmt.Errorf("로컬 장치 자격 삭제 실패: %w", err))
	}
	_ = os.Remove(runtimeStatePath(path))
	fmt.Println("Pie Client 연결과 로컬 자격을 해제했습니다.")
}

func disconnectRemoteDevice(ctx context.Context, client *http.Client, credentials devicecredentials.Credentials) error {
	endpoint := strings.TrimRight(credentials.BuilderURL, "/") + "/api/agent-runtimes/disconnect"
	body, err := json.Marshal(map[string]string{"refreshToken": credentials.RefreshToken})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
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
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("장치 연결 서비스 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	return nil
}

func runManagedSessionManager(args []string, credentialsPath string) {
	state, cleanup, err := acquireRuntimeState(credentialsPath, sessionManagerListenAddress(args))
	if err != nil {
		fatalLifecycle(err)
	}
	defer cleanup()
	previous := os.Getenv("PIE_CLIENT_RUNTIME_TOKEN")
	_ = os.Setenv("PIE_CLIENT_RUNTIME_TOKEN", state.ControlToken)
	defer func() {
		if previous == "" {
			_ = os.Unsetenv("PIE_CLIENT_RUNTIME_TOKEN")
		} else {
			_ = os.Setenv("PIE_CLIENT_RUNTIME_TOKEN", previous)
		}
	}()
	runSessionManager(args)
}

func acquireRuntimeState(credentialsPath, listen string) (runtimeState, func(), error) {
	path := runtimeStatePath(credentialsPath)
	if current, err := loadRuntimeState(path); err == nil {
		if current.PID > 0 && pidAlive(current.PID) && (runtimeEndpointAlive(current) || runtimeIsStarting(current)) {
			return runtimeState{}, nil, fmt.Errorf("Pie Client가 이미 실행 중입니다 (PID %d)", current.PID)
		}
		_ = os.Remove(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return runtimeState{}, nil, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return runtimeState{}, nil, err
	}
	state := runtimeState{
		Version:         runtimeStateVersion,
		PID:             os.Getpid(),
		StartedAt:       time.Now().UTC(),
		ListenAddress:   normalizeRuntimeAddress(listen),
		ControlToken:    base64.RawURLEncoding.EncodeToString(tokenBytes),
		CredentialsPath: credentialsPath,
	}
	if err := saveRuntimeState(path, state); err != nil {
		return runtimeState{}, nil, err
	}
	cleanup := func() {
		current, err := loadRuntimeState(path)
		if err == nil && current.PID == state.PID && current.ControlToken == state.ControlToken {
			_ = os.Remove(path)
		}
	}
	return state, cleanup, nil
}

func readRuntimeStatus(credentialsPath string) (runtimeStatus, error) {
	path := runtimeStatePath(credentialsPath)
	state, err := loadRuntimeState(path)
	if err != nil {
		return runtimeStatus{}, err
	}
	if state.PID <= 0 || !pidAlive(state.PID) {
		_ = os.Remove(path)
		return runtimeStatus{}, nil
	}
	if !runtimeEndpointAlive(state) && !runtimeIsStarting(state) {
		_ = os.Remove(path)
		return runtimeStatus{}, nil
	}
	return runtimeStatus{Running: true, PID: state.PID, StartedAt: state.StartedAt, Address: state.ListenAddress}, nil
}

func stopManagedRuntime(credentialsPath string, wait time.Duration) (bool, error) {
	path := runtimeStatePath(credentialsPath)
	state, err := loadRuntimeState(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state.PID <= 0 || !pidAlive(state.PID) || !runtimeEndpointAlive(state) {
		_ = os.Remove(path)
		return false, nil
	}
	endpoint := runtimeEndpoint(state, "/v1/runtime/stop")
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+state.ControlToken)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("Pie Client 종료 요청이 HTTP %d를 반환했습니다", resp.StatusCode)
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			return true, nil
		}
		if !runtimeEndpointAlive(state) && !pidAlive(state.PID) {
			_ = os.Remove(path)
			return true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if runtimeEndpointAlive(state) {
		return false, errors.New("Pie Client가 제한 시간 안에 종료되지 않았습니다")
	}
	_ = os.Remove(path)
	return true, nil
}

func runtimeEndpointAlive(state runtimeState) bool {
	client := &http.Client{Timeout: 750 * time.Millisecond}
	req, err := http.NewRequest(http.MethodGet, runtimeEndpoint(state, "/v1/runtime/status"), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+state.ControlToken)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var payload struct {
		PID int `json:"pid"`
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&payload) == nil && payload.PID == state.PID
}

func runtimeIsStarting(state runtimeState) bool {
	return !state.StartedAt.IsZero() && time.Since(state.StartedAt) < 15*time.Second
}

func runtimeEndpoint(state runtimeState, path string) string {
	return (&url.URL{Scheme: "http", Host: normalizeRuntimeAddress(state.ListenAddress), Path: path}).String()
}

func normalizeStartArgs(args []string) ([]string, string, error) {
	credentialsPath := strings.TrimSpace(os.Getenv("PIE_DEVICE_CREDENTIALS"))
	forwarded := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		value := args[i]
		switch {
		case value == "--credentials" || value == "--device-credentials":
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("%s 값이 필요합니다", value)
			}
			i++
			credentialsPath = args[i]
			forwarded = append(forwarded, "--device-credentials", credentialsPath)
		case strings.HasPrefix(value, "--credentials="):
			credentialsPath = strings.TrimPrefix(value, "--credentials=")
			forwarded = append(forwarded, "--device-credentials", credentialsPath)
		case strings.HasPrefix(value, "--device-credentials="):
			credentialsPath = strings.TrimPrefix(value, "--device-credentials=")
			forwarded = append(forwarded, value)
		default:
			forwarded = append(forwarded, value)
		}
	}
	path, err := resolveDeviceCredentialsPath(credentialsPath)
	if err != nil {
		return nil, "", err
	}
	if !containsDeviceCredentialsFlag(forwarded) {
		forwarded = append(forwarded, "--device-credentials", path)
	}
	if !containsControlModeFlag(forwarded) {
		forwarded = append(forwarded, "--control-mode", "device")
	}
	return forwarded, path, nil
}

func containsControlModeFlag(args []string) bool {
	for _, value := range args {
		if value == "--control-mode" || strings.HasPrefix(value, "--control-mode=") {
			return true
		}
	}
	return false
}

func containsDeviceCredentialsFlag(args []string) bool {
	for _, value := range args {
		if value == "--device-credentials" || strings.HasPrefix(value, "--device-credentials=") {
			return true
		}
	}
	return false
}

func sessionManagerListenAddress(args []string) string {
	value := envOr("PIE_SESSION_MANAGER_ADDR", "127.0.0.1:19091")
	for i := 0; i < len(args); i++ {
		if args[i] == "--listen" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], "--listen=") {
			return strings.TrimPrefix(args[i], "--listen=")
		}
	}
	return value
}

func normalizeRuntimeAddress(value string) string {
	host, port, err := net.SplitHostPort(value)
	if err != nil || port == "" {
		return value
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "localhost" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func runtimeStatePath(credentialsPath string) string {
	if configured := strings.TrimSpace(os.Getenv("PIE_CLIENT_RUNTIME_STATE")); configured != "" {
		return configured
	}
	return filepath.Join(filepath.Dir(credentialsPath), "pie-client-runtime.json")
}

func saveRuntimeState(path string, value runtimeState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pie-client-runtime-*")
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
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func loadRuntimeState(path string) (runtimeState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return runtimeState{}, err
	}
	var value runtimeState
	if err := json.Unmarshal(data, &value); err != nil {
		return runtimeState{}, fmt.Errorf("Pie Client 실행 상태 파일을 읽을 수 없습니다: %w", err)
	}
	if value.Version != runtimeStateVersion || value.PID <= 0 || value.ListenAddress == "" || value.ControlToken == "" {
		return runtimeState{}, errors.New("Pie Client 실행 상태 파일이 올바르지 않습니다")
	}
	return value, nil
}

func usesDirectControlCredentials() bool {
	return strings.TrimSpace(os.Getenv("PIE_CONTROL_PLANE_URL")) != "" ||
		strings.TrimSpace(os.Getenv("PIE_CONTROL_PLANE_TOKEN")) != "" ||
		strings.TrimSpace(os.Getenv("PIE_DEVICE_ID")) != ""
}

func prepareACPRuntime() error {
	if _, err := exec.LookPath("node"); err != nil {
		return errors.New("Node.js 22 이상이 필요합니다")
	}
	acpExecutor := resolveACPExecutorPath(os.Getenv("ACP_EXECUTOR_PATH"))
	if _, err := os.Stat(acpExecutor); err != nil {
		return fmt.Errorf("ACP 실행기를 찾을 수 없습니다: %s", acpExecutor)
	}
	if strings.TrimSpace(os.Getenv("ACP_EXECUTOR_PATH")) == "" {
		_ = os.Setenv("ACP_EXECUTOR_PATH", acpExecutor)
	}
	command := strings.TrimSpace(os.Getenv("PIE_ACP_AGENT_COMMAND"))
	if command == "" {
		name := "claude-agent-acp"
		if filepath.Ext(os.Args[0]) == ".exe" {
			name += ".cmd"
		}
		candidate := filepath.Join(filepath.Dir(acpExecutor), "node_modules", ".bin", name)
		if _, err := os.Stat(candidate); err == nil {
			command = candidate
			_ = os.Setenv("PIE_ACP_AGENT_COMMAND", command)
		}
	}
	if command == "" {
		if resolved, err := exec.LookPath("claude-agent-acp"); err == nil {
			command = resolved
			_ = os.Setenv("PIE_ACP_AGENT_COMMAND", command)
		}
	}
	if command == "" {
		codexAdapter := filepath.Join(filepath.Dir(acpExecutor), "codex-acp-adapter.mjs")
		_, adapterErr := os.Stat(codexAdapter)
		_, codexErr := exec.LookPath("codex")
		if adapterErr != nil || codexErr != nil {
			return errors.New("Claude Code 또는 Codex 실행기를 찾을 수 없습니다. 설치 패키지를 다시 준비해 주세요")
		}
	}
	return nil
}

func fallback(value, defaultValue string) string {
	if strings.TrimSpace(value) == "" {
		return defaultValue
	}
	return value
}

func fatalLifecycle(err error) {
	fmt.Fprintln(os.Stderr, "Pie Client:", err)
	os.Exit(1)
}
