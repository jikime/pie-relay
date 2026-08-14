package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cli-relay/client/internal/devicecredentials"
	"cli-relay/client/internal/previewprocess"
	"cli-relay/client/internal/sessionmanager"
)

type sessionAPI struct {
	manager  *sessionmanager.Manager
	previews *previewprocess.Manager
	token    string
}

func runSessionManager(args []string) {
	fs := flag.NewFlagSet("sessions serve", flag.ExitOnError)
	listen := fs.String("listen", envOr("PIE_SESSION_MANAGER_ADDR", "127.0.0.1:19091"), "session manager listen address")
	authToken := fs.String("auth-token", os.Getenv("PIE_SESSION_MANAGER_TOKEN"), "Bearer token for session manager API")
	ptyHostPath := fs.String("pty-host", envOr("PTY_HOST_PATH", ""), "path to pty-host.mjs")
	executorPath := fs.String("executor", envOr("EXECUTOR_PATH", ""), "path to executor.mjs")
	acpExecutorPath := fs.String("acp-executor", envOr("ACP_EXECUTOR_PATH", ""), "path to acp-executor.mjs")
	maxSessions := fs.Int("max-sessions", 16, "maximum concurrent PTY sessions")
	maxPreviews := fs.Int("max-previews", 8, "maximum concurrent project previews")
	previewRoot := fs.String("preview-root", envOr("PIE_PREVIEW_WORKSPACE_ROOT", "/workspace/projects"), "allowed project preview workspace root")
	controlURL := fs.String("control-url", os.Getenv("PIE_CONTROL_PLANE_URL"), "Pie Control Plane HTTP(S) origin")
	controlToken := fs.String("control-token", os.Getenv("PIE_CONTROL_PLANE_TOKEN"), "user PAT for device registration")
	deviceID := fs.String("device-id", os.Getenv("PIE_DEVICE_ID"), "stable device id")
	deviceName := fs.String("device-name", os.Getenv("PIE_DEVICE_NAME"), "display name")
	deviceCredentials := fs.String("device-credentials", os.Getenv("PIE_DEVICE_CREDENTIALS"), "paired device credentials path")
	controlMode := fs.String("control-mode", "disabled", "Control Plane mode: disabled or device")
	_ = fs.Parse(args)
	if *controlMode != "disabled" && *controlMode != "device" {
		log.Fatal("control-mode must be disabled or device")
	}
	if *ptyHostPath == "" {
		*ptyHostPath = resolvePTYHostPath("")
	}
	if *executorPath == "" {
		*executorPath = resolveExecutorPath("")
	}
	if *acpExecutorPath == "" {
		*acpExecutorPath = resolveACPExecutorPath("")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatalf("session manager listen address: %v", err)
	}
	if *authToken == "" {
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			log.Fatal("PIE_SESSION_MANAGER_TOKEN is required for a non-loopback listener")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	watchParent(ctx, stop)
	manager := sessionmanager.NewWithExecutors(ctx, *ptyHostPath, *executorPath, *acpExecutorPath, *maxSessions, nil, nil)
	previewManager := previewprocess.New(ctx, *previewRoot, *maxPreviews)
	var deviceClient *deviceControlClient
	configuredControl := *controlURL != "" || *controlToken != "" || *deviceID != ""
	if *controlMode == "device" && configuredControl {
		deviceClient, err = newDeviceControlClient(*controlURL, *controlToken, *deviceID, *deviceName)
		if err != nil {
			log.Fatalf("Pie Control device registration: %v", err)
		}
	} else if *controlMode == "device" {
		if *deviceCredentials == "" {
			*deviceCredentials, err = devicecredentials.DefaultPath()
			if err != nil {
				log.Fatalf("Pie Control device credentials: %v", err)
			}
		}
		source, loadErr := loadDeviceCredentialSource(*deviceCredentials)
		if loadErr == nil {
			resolvedControlURL, resolvedDeviceID, resolvedDeviceName := source.Device()
			deviceClient, err = newDeviceControlClientWithSource(resolvedControlURL, source, resolvedDeviceID, resolvedDeviceName)
			if err != nil {
				log.Fatalf("Pie Control paired device: %v", err)
			}
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			log.Fatalf("Pie Control device credentials: %v", loadErr)
		}
	}
	if deviceClient != nil {
		// 최초 등록에는 Claude Code·Codex 준비 상태 탐색(최대 10초)이 포함된다.
		// 탐색 뒤 Control POST까지 완료할 여유를 두되 일반 heartbeat는 계속 8초로 제한한다.
		registerCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		if err := deviceClient.register(registerCtx); err != nil {
			cancel()
			log.Fatalf("Pie Control device registration: %v", err)
		}
		cancel()
		// Registration creates the durable device identity, while heartbeat is
		// the authoritative liveness signal. Report it immediately so Desktop
		// does not have to wait for the first 15-second ticker before assigning
		// a newly started Host OS session.
		heartbeatCtx, heartbeatCancel := context.WithTimeout(ctx, 8*time.Second)
		if err := deviceClient.heartbeat(heartbeatCtx, 0, true, false); err != nil {
			heartbeatCancel()
			log.Fatalf("Pie Control initial heartbeat: %v", err)
		}
		heartbeatCancel()
		reconcileCtx, reconcileCancel := context.WithTimeout(ctx, 15*time.Second)
		if err := reconcileDeviceSessions(reconcileCtx, deviceClient, manager); err != nil && ctx.Err() == nil {
			log.Printf("Pie Control session reconcile: %v", err)
		}
		reconcileCancel()
		go func() {
			heartbeat := time.NewTicker(15 * time.Second)
			reconcile := time.NewTicker(2 * time.Second)
			defer heartbeat.Stop()
			defer reconcile.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-heartbeat.C:
					active := 0
					connected := 0
					for _, session := range manager.List() {
						if session.State == "running" || session.State == "starting" {
							active++
						}
						if session.RelayState == "connected" {
							connected++
						}
					}
					heartbeatCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
					if err := deviceClient.heartbeat(heartbeatCtx, active, true, connected > 0); err != nil && ctx.Err() == nil {
						log.Printf("Pie Control heartbeat: %v", err)
					}
					cancel()
				case <-reconcile.C:
					reconcileCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
					if err := reconcileDeviceSessions(reconcileCtx, deviceClient, manager); err != nil && ctx.Err() == nil {
						log.Printf("Pie Control session reconcile: %v", err)
					}
					cancel()
				}
			}
		}()
	}
	api := sessionAPI{manager: manager, previews: previewManager, token: *authToken}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/v1/sessions", api.authorize(api.sessions))
	mux.HandleFunc("/v1/sessions/", api.authorize(api.session))
	mux.HandleFunc("/v1/previews", api.authorize(api.previewsHandler))
	mux.HandleFunc("/v1/previews/", api.authorize(api.previewHandler))
	mux.HandleFunc("/v1/projects/", api.authorize(api.projectHandler))
	runtimeToken := os.Getenv("PIE_CLIENT_RUNTIME_TOKEN")
	if runtimeToken != "" {
		mux.HandleFunc("/v1/runtime/status", authorizeRuntime(runtimeToken, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writeSessionJSON(w, map[string]any{
				"status": "running",
				"pid":    os.Getpid(),
			}, http.StatusOK)
		}))
		mux.HandleFunc("/v1/runtime/stop", authorizeRuntime(runtimeToken, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writeSessionJSON(w, map[string]string{"status": "stopping"}, http.StatusAccepted)
			go func() {
				time.Sleep(25 * time.Millisecond)
				stop()
			}()
		}))
	}
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	log.Printf("Pie Client Session Manager listening on %s (max sessions=%d)", *listen, *maxSessions)
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("session manager: %v", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	_ = manager.Close(shutdownCtx)
	_ = previewManager.Close(shutdownCtx)
	if deviceClient != nil {
		offlineCtx, offlineCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = deviceClient.heartbeat(offlineCtx, 0, false, false)
		offlineCancel()
	}
}

func (a sessionAPI) projectHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/projects/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "apps" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	applications, err := a.previews.DiscoverApplications(parts[0])
	if err != nil {
		writePreviewError(w, err)
		return
	}
	writeSessionJSON(w, applications, http.StatusOK)
}

func (a sessionAPI) previewsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeSessionJSON(w, a.previews.List(), http.StatusOK)
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		defer r.Body.Close()
		var config previewprocess.Config
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&config) != nil {
			http.Error(w, "invalid preview config", http.StatusBadRequest)
			return
		}
		status, duplicate, err := a.previews.Start(config)
		if err != nil {
			writePreviewError(w, err)
			return
		}
		code := http.StatusCreated
		if duplicate {
			code = http.StatusOK
		}
		writeSessionJSON(w, status, code)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a sessionAPI) previewHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/previews/"), "/")
	if path == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(path, "/")
	id := parts[0]
	if len(parts) == 2 && parts[1] == "logs" && r.Method == http.MethodGet {
		logs, err := a.previews.Logs(id, 1<<20)
		if err != nil {
			writePreviewError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(logs)
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		status, ok := a.previews.Get(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeSessionJSON(w, status, http.StatusOK)
	case http.MethodDelete:
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		status, err := a.previews.Stop(ctx, id)
		if err != nil {
			writePreviewError(w, err)
			return
		}
		writeSessionJSON(w, status, http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writePreviewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, previewprocess.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, previewprocess.ErrExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, previewprocess.ErrLimit):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func authorizeRuntime(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(provided) != len(token) || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (a sessionAPI) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.token != "" {
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if len(provided) != len(a.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(a.token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (a sessionAPI) sessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeSessionJSON(w, a.manager.List(), http.StatusOK)
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		defer r.Body.Close()
		var config sessionmanager.Config
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&config) != nil {
			http.Error(w, "invalid session config", http.StatusBadRequest)
			return
		}
		status, duplicate, err := a.manager.Start(config)
		if err != nil {
			writeSessionError(w, err)
			return
		}
		code := http.StatusCreated
		if duplicate {
			code = http.StatusOK
		}
		writeSessionJSON(w, status, code)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a sessionAPI) session(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		status, ok := a.manager.Get(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeSessionJSON(w, status, http.StatusOK)
	case http.MethodDelete:
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := a.manager.Remove(ctx, id); err != nil {
			writeSessionError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeSessionJSON(w http.ResponseWriter, value any, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sessionmanager.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, sessionmanager.ErrExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, sessionmanager.ErrLimit):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
