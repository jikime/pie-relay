package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/auth"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/capability"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/chatgateway"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/claudeauth"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/controller"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/credential"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
	previewservice "github.com/pielab-ai/pie-relay/executor-manager/internal/preview"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/runtime"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/store"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/thirdparty"
	usageledger "github.com/pielab-ai/pie-relay/executor-manager/internal/usage"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type api struct {
	m                   *manager.Manager
	verifier            auth.Verifier
	blobs               *store.BlobStore
	uploadSlots         chan struct{}
	eventSlots          chan struct{}
	eventLifetime       time.Duration
	control             *control.Service
	issuer              capability.Issuer
	webhookSecret       []byte
	webhookSkew         time.Duration
	relayPublicURL      string
	thirdParty          *thirdparty.Service
	chat                *chatgateway.Gateway
	previews            *previewservice.Service
	usage               *usageledger.Service
	claudeAuth          *claudeauth.Service
	claudeAuthWorkers   int
	previewGatewayToken string
}

func (a api) principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	p, err := a.verifier.Verify(r.Context(), token)
	if err != nil {
		http.Error(w, "unauthorized", 401)
		return auth.Principal{}, false
	}
	return p, true
}
func (a api) authorizeUser(w http.ResponseWriter, r *http.Request, userID string) bool {
	p, ok := a.principal(w, r)
	if !ok {
		return false
	}
	if !p.Admin && p.UserID != userID {
		http.Error(w, "forbidden", 403)
		return false
	}
	return true
}
func (a api) authorizeJob(w http.ResponseWriter, r *http.Request, job manager.Job) bool {
	p, ok := a.principal(w, r)
	if !ok {
		return false
	}
	if !p.Admin && p.UserID != job.UserID {
		http.Error(w, "forbidden", 403)
		return false
	}
	return true
}
func (a api) ensure(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PUT" {
		http.Error(w, "method not allowed", 405)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/users/"), "/executor")
	if !manager.ValidUserID(id) {
		http.Error(w, "invalid user", 400)
		return
	}
	if !a.authorizeUser(w, r, id) {
		return
	}
	var (
		e   manager.Executor
		err error
	)
	user, exists := a.control.User(id)
	if exists && user.Status != "active" {
		http.Error(w, "user is not active", http.StatusForbidden)
		return
	}
	if exists {
		e, err = a.m.EnsureWithLimits(r.Context(), id, executorLimits(user.Quota))
	} else {
		e, err = a.m.Ensure(r.Context(), id)
	}
	if err != nil {
		switch {
		case errors.Is(err, manager.ErrExecutorCapacity):
			w.Header().Set("Retry-After", "5")
			http.Error(w, err.Error(), http.StatusTooManyRequests)
		case errors.Is(err, manager.ErrExecutorDiskQuota):
			http.Error(w, err.Error(), http.StatusInsufficientStorage)
		case errors.Is(err, manager.ErrExecutorDiskCapacity):
			w.Header().Set("Retry-After", "60")
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	write(w, e, 200)
}

func executorLimits(quota control.ResourceQuota) manager.ExecutorLimits {
	return manager.ExecutorLimits{CPUs: quota.CPUs, MemoryBytes: quota.MemoryBytes, PIDsLimit: quota.PIDs, DiskBytes: quota.DiskBytes}
}
func (a api) submit(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/users/")
	id = strings.TrimSuffix(id, "/jobs")
	if !manager.ValidUserID(id) {
		http.Error(w, "invalid user", 400)
		return
	}
	if !a.authorizeUser(w, r, id) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	var in struct {
		Command  string   `json:"command"`
		BlobRefs []string `json:"blobRefs"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Command == "" {
		http.Error(w, "command is required", 400)
		return
	}
	if a.blobs == nil {
		http.Error(w, "blob store unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := a.blobs.ValidateRefs(id, in.BlobRefs); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	j, err := a.m.Submit(r.Context(), id, []byte(in.Command), in.BlobRefs...)
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	write(w, j, 202)
}
func (a api) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/users/"), "/uploads")
	if !manager.ValidUserID(id) {
		http.Error(w, "invalid user", 400)
		return
	}
	if !a.authorizeUser(w, r, id) {
		return
	}
	if a.blobs == nil {
		http.Error(w, "blob store unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodDelete {
		if err := a.blobs.Delete(id, r.URL.Query().Get("ref")); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !acquire(w, a.uploadSlots) {
		return
	}
	defer func() { <-a.uploadSlots }()
	r.Body = http.MaxBytesReader(w, r.Body, 65<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, "invalid multipart", 400)
		return
	}
	if r.MultipartForm != nil {
		defer func() {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				log.Printf("multipart 임시 파일 정리 실패: %v", err)
			}
		}()
	}
	f, h, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file is required", 400)
		return
	}
	defer f.Close()
	ref, n, err := a.blobs.Save(id, h.Filename, f)
	if err != nil {
		http.Error(w, err.Error(), 413)
		return
	}
	write(w, map[string]any{"userId": id, "ref": ref, "bytes": n}, 201)
}
func (a api) job(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	j, ok := a.m.Job(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !a.authorizeJob(w, r, j) {
		return
	}
	write(w, j, 200)
}
func (a api) cancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), "/cancel")
	j, ok := a.m.Job(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !a.authorizeJob(w, r, j) {
		return
	}
	if err := a.m.Cancel(id); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	write(w, map[string]string{"status": "cancel_requested"}, 202)
}
func (a api) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	id = strings.TrimSuffix(id, "/events")
	initial, found := a.m.Job(id)
	if !found {
		http.NotFound(w, r)
		return
	}
	if !a.authorizeJob(w, r, initial) {
		return
	}
	if !acquire(w, a.eventSlots) {
		return
	}
	defer func() { <-a.eventSlots }()
	initial, changes, unsubscribe, found := a.m.Subscribe(id)
	if !found {
		http.NotFound(w, r)
		return
	}
	defer unsubscribe()
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("x-accel-buffering", "no")
	w.Header().Set("connection", "keep-alive")
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	deadline := time.NewTimer(a.eventLifetime)
	defer deadline.Stop()
	lastStatus := ""
	lastLogCount := 0
	send := func(j manager.Job) (bool, error) {
		if lastLogCount > len(j.Logs) {
			lastLogCount = 0
		}
		if j.Status == lastStatus && len(j.Logs) == lastLogCount {
			return false, nil
		}
		newLogs := append([]string(nil), j.Logs[lastLogCount:]...)
		event := map[string]any{"id": j.ID, "status": j.Status, "logs": newLogs, "logTruncated": j.LogTruncated}
		if j.FinishedAt != nil {
			event["finishedAt"] = j.FinishedAt
			event["output"] = j.Output
			event["error"] = j.Err
		}
		b, err := json.Marshal(event)
		if err != nil {
			return false, err
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
		if _, err = fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false, err
		}
		fl.Flush()
		lastStatus = j.Status
		lastLogCount = len(j.Logs)
		return true, nil
	}
	terminal := func(status string) bool {
		return status == "succeeded" || status == "failed" || status == "canceled" || status == "rejected"
	}
	if _, err := send(initial); err != nil {
		return
	}
	if terminal(initial.Status) {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-heartbeat.C:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-changes:
			j, found := a.m.Job(id)
			if !found {
				return
			}
			if _, err := send(j); err != nil {
				return
			}
			if terminal(j.Status) {
				return
			}
		}
	}
}
func acquire(w http.ResponseWriter, slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		w.Header().Set("retry-after", "1")
		http.Error(w, "too many concurrent requests", http.StatusTooManyRequests)
		return false
	}
}
func write(w http.ResponseWriter, v any, s int) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(s)
	_ = json.NewEncoder(w).Encode(v)
}
func positiveEnv(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		log.Fatalf("%s must be a positive integer", name)
	}
	return parsed
}

func nonNegativeEnv(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		log.Fatalf("%s must be a non-negative integer", name)
	}
	return value
}
func positiveInt64Env(name string, fallback int64) int64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 {
		log.Fatalf("%s must be a positive integer", name)
	}
	return parsed
}
func durationEnv(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		log.Fatalf("%s must be a positive duration", name)
	}
	return parsed
}

func boolEnv(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		log.Fatalf("%s must be a boolean", name)
	}
	return parsed
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func withCORS(next http.Handler, configured string) http.Handler {
	allowed := map[string]struct{}{}
	for _, origin := range strings.Split(configured, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			allowed[origin] = struct{}{}
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Add("Vary", "Origin")
			_, explicitlyAllowed := allowed[origin]
			if !explicitlyAllowed && !isSameOrigin(r, origin) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-Id, Cache-Control, Pragma")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isSameOrigin(r *http.Request, origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Reverse proxies such as Traefik preserve the public Host and report the
	// public scheme here. Only the first value is relevant if multiple proxies
	// appended a comma-separated list.
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	return strings.EqualFold(parsed.Scheme, scheme) && strings.EqualFold(parsed.Host, r.Host)
}

func main() {
	rootCtx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	addr := os.Getenv("PIE_EXECUTOR_MANAGER_ADDR")
	if addr == "" {
		addr = ":19090"
	}
	token := os.Getenv("PIE_EXECUTOR_MANAGER_TOKEN")
	presenceToken := os.Getenv("PIE_RELAY_PRESENCE_TOKEN")
	introspectionURL := os.Getenv("PIE_AUTH_INTROSPECTION_URL")
	deviceAuthSecret := os.Getenv("PIE_DEVICE_AUTH_SECRET")
	if token == "" && introspectionURL == "" && deviceAuthSecret == "" {
		log.Fatal("PIE_EXECUTOR_MANAGER_TOKEN, PIE_AUTH_INTROSPECTION_URL, or PIE_DEVICE_AUTH_SECRET is required")
	}
	verifiers := auth.Chain{}
	if token != "" {
		verifiers = append(verifiers, auth.Static{Token: token})
	}
	if presenceToken != "" {
		verifiers = append(verifiers, auth.Static{Token: presenceToken, Principal: auth.Principal{Roles: []string{"pie-relay-presence"}}})
	}
	if deviceAuthSecret != "" {
		if len(deviceAuthSecret) < 32 {
			log.Fatal("PIE_DEVICE_AUTH_SECRET must be at least 32 bytes")
		}
		verifiers = append(verifiers, auth.DeviceJWT{Secret: []byte(deviceAuthSecret)})
	}
	if introspectionURL != "" {
		introspection := auth.Introspection{
			URL:          introspectionURL,
			Client:       &http.Client{Timeout: durationEnv("PIE_AUTH_HTTP_TIMEOUT", 5*time.Second)},
			ClientID:     os.Getenv("PIE_AUTH_CLIENT_ID"),
			ClientSecret: os.Getenv("PIE_AUTH_CLIENT_SECRET"),
		}
		verifiers = append(verifiers, &auth.Cached{
			Next:        introspection,
			TTL:         durationEnv("PIE_AUTH_CACHE_TTL", 30*time.Second),
			NegativeTTL: durationEnv("PIE_AUTH_NEGATIVE_CACHE_TTL", 5*time.Second),
		})
	}
	legacyStatePath := os.Getenv("PIE_EXECUTOR_MANAGER_STATE")
	registryRoot := os.Getenv("PIE_EXECUTOR_REGISTRY_DIR")
	if registryRoot == "" {
		registryRoot = filepath.Join("var", "registry")
	}
	image := os.Getenv("PIE_EXECUTOR_IMAGE")
	if image == "" {
		image = "pie-relay-client:latest"
	}
	blobRoot := os.Getenv("PIE_EXECUTOR_BLOB_DIR")
	if blobRoot == "" {
		blobRoot = filepath.Join("var", "blobs")
	}
	workRoot := os.Getenv("PIE_EXECUTOR_WORK_DIR")
	if workRoot == "" {
		workRoot = filepath.Join("var", "workspaces")
	}
	stateRoot := os.Getenv("PIE_EXECUTOR_STATE_DIR")
	if stateRoot == "" {
		stateRoot = filepath.Join("var", "executor-state")
	}
	managerID := os.Getenv("PIE_EXECUTOR_MANAGER_ID")
	if managerID == "" {
		managerID = "default"
	}
	if !manager.ValidUserID(managerID) {
		log.Fatal("PIE_EXECUTOR_MANAGER_ID must contain only letters, numbers, dot, underscore, or dash")
	}
	var err error
	blobRoot, err = filepath.Abs(blobRoot)
	if err != nil {
		log.Fatal(err)
	}
	workRoot, err = filepath.Abs(workRoot)
	if err != nil {
		log.Fatal(err)
	}
	stateRoot, err = filepath.Abs(stateRoot)
	if err != nil {
		log.Fatal(err)
	}
	var s manager.Store
	if legacyStatePath != "" {
		log.Printf("using legacy JSON registry %s; PIE_EXECUTOR_REGISTRY_DIR is recommended", legacyStatePath)
		s = store.New(legacyStatePath)
	} else if _, statErr := os.Stat(filepath.Join("var", "manager.json")); statErr == nil && os.Getenv("PIE_EXECUTOR_REGISTRY_DIR") == "" {
		legacyStatePath = filepath.Join("var", "manager.json")
		log.Printf("existing legacy JSON registry detected at %s; continuing without destructive migration", legacyStatePath)
		s = store.New(legacyStatePath)
	} else {
		s = store.NewDirectory(registryRoot)
	}
	containerUser := os.Getenv("PIE_EXECUTOR_CONTAINER_USER")
	if containerUser == "" {
		if os.Geteuid() == 0 {
			containerUser = "10001:10001"
		} else {
			containerUser = fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid())
		}
	}
	permissionMode := strings.TrimSpace(os.Getenv("PIE_EXECUTOR_PERMISSION_MODE"))
	if err := runtime.ValidatePermissionMode(permissionMode); err != nil {
		log.Fatalf("PIE_EXECUTOR_PERMISSION_MODE: %v", err)
	}
	credentialStore, err := credential.New(stateRoot, containerUser)
	if err != nil {
		log.Fatalf("initialize credential store: %v", err)
	}
	claudeAuthRoot := envOr("PIE_CLAUDE_AUTH_DIR", filepath.Join(filepath.Dir(stateRoot), "claude-auth"))
	claudeLoginRoot := envOr("PIE_CLAUDE_AUTH_LOGIN_DIR", filepath.Join(claudeAuthRoot, "login"))
	claudeAuthService, err := claudeauth.Open(claudeAuthRoot, claudeLoginRoot, credentialStore, boolEnv("PIE_CLAUDE_AUTH_REQUIRED", false))
	if err != nil {
		log.Fatalf("initialize Claude authentication store: %v", err)
	}
	dockerRuntime := runtime.Docker{
		Image: image, Prefix: "pie-" + managerID + "-", Scope: managerID,
		User: containerUser, PermissionMode: permissionMode,
		AllowUserNamespaces: boolEnv("PIE_EXECUTOR_ALLOW_USER_NAMESPACES", false),
		KrootAutoLink:       boolEnv("PIE_EXECUTOR_KROOT_AUTO_LINK", false),
		BlobRoot:            blobRoot, WorkRoot: workRoot, StateRoot: stateRoot,
		StateSeedRoot:            os.Getenv("PIE_EXECUTOR_STATE_SEED_DIR"),
		KrootCommonBundleRoot:    os.Getenv("PIE_KROOT_COMMON_BUNDLE_DIR"),
		KrootCommonBundleVersion: os.Getenv("PIE_KROOT_COMMON_BUNDLE_VERSION"),
		Network:                  os.Getenv("PIE_EXECUTOR_NETWORK"),
		CPUs:                     os.Getenv("PIE_EXECUTOR_CPUS"), Memory: os.Getenv("PIE_EXECUTOR_MEMORY"),
		MemorySwap: os.Getenv("PIE_EXECUTOR_MEMORY_SWAP"), PIDsLimit: os.Getenv("PIE_EXECUTOR_PIDS_LIMIT"),
		DiskQuotaBytes:          positiveInt64Env("PIE_EXECUTOR_DISK_QUOTA_BYTES", 20<<30),
		DiskHeadroomBytes:       positiveInt64Env("PIE_EXECUTOR_DISK_HEADROOM_BYTES", 5<<30),
		PreviewGatewayContainer: os.Getenv("PIE_PREVIEW_GATEWAY_CONTAINER"),
		CredentialProvisioner:   claudeAuthService,
	}
	if boolEnv("PIE_EXECUTOR_REQUIRE_ISOLATED_NETWORK", false) {
		validationCtx, cancel := context.WithTimeout(rootCtx, 10*time.Second)
		defer cancel()
		if err := dockerRuntime.ValidateExecutorNetwork(validationCtx); err != nil {
			log.Fatalf("PIE_EXECUTOR_NETWORK isolation validation failed: %v", err)
		}
	}
	jobTimeout := 30 * time.Minute
	if raw := os.Getenv("PIE_EXECUTOR_JOB_TIMEOUT"); raw != "" {
		jobTimeout, err = time.ParseDuration(raw)
		if err != nil || jobTimeout <= 0 {
			log.Fatal("PIE_EXECUTOR_JOB_TIMEOUT must be a positive duration")
		}
	}
	m, err := manager.NewWithOptions(rootCtx, dockerRuntime, s, manager.Options{
		QueueCapacity:        positiveEnv("PIE_EXECUTOR_QUEUE_CAPACITY", 64),
		Workers:              positiveEnv("PIE_EXECUTOR_WORKERS", 4),
		JobTimeout:           jobTimeout,
		ProvisionConcurrency: positiveEnv("PIE_EXECUTOR_PROVISION_CONCURRENCY", 4),
		RetainedJobs:         positiveEnv("PIE_EXECUTOR_RETAINED_JOBS", 1000),
		MaxExecutors:         positiveEnv("PIE_EXECUTOR_MAX_EXECUTORS", 64),
		StorageScanInterval:  durationEnv("PIE_EXECUTOR_DISK_SCAN_INTERVAL", time.Minute),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer m.Close()
	controlRoot := os.Getenv("PIE_CONTROL_REGISTRY_DIR")
	if controlRoot == "" {
		controlRoot = filepath.Join("var", "control")
	}
	var controlStore control.Store
	if dsn := os.Getenv("PIE_CONTROL_DATABASE_URL"); dsn != "" {
		controlStore, err = control.OpenPostgres(rootCtx, dsn)
		if err != nil {
			log.Fatalf("open Pie Control PostgreSQL: %v", err)
		}
		log.Printf("Pie Control Plane uses PostgreSQL")
	} else {
		controlStore = control.NewDirectoryStore(controlRoot)
		log.Printf("Pie Control Plane uses standalone directory registry %s", controlRoot)
	}
	controlService, err := control.NewService(rootCtx, controlStore)
	if err != nil {
		log.Fatalf("initialize Pie Control Plane: %v", err)
	}
	defer controlService.Close()
	controlService.SetDefaultQuota(control.ResourceQuota{
		DiskBytes:       positiveInt64Env("PIE_EXECUTOR_DISK_QUOTA_BYTES", 20<<30),
		MaxSessions:     positiveEnv("PIE_DEFAULT_MAX_SESSIONS", 8),
		MaxParticipants: positiveEnv("PIE_DEFAULT_MAX_PARTICIPANTS", 32),
	})
	if err := controlService.SetDefaultRelayContext(
		envOr("PIE_RELAY_DEFAULT_APPLICATION_ID", "pie-control"),
		envOr("PIE_RELAY_DEFAULT_POOL_ID", envOr("RELAY_POOL_ID", "pie-relay-default")),
	); err != nil {
		log.Fatal("PIE_RELAY_DEFAULT_APPLICATION_ID and PIE_RELAY_DEFAULT_POOL_ID must be valid and configured together")
	}
	thirdPartyService := &thirdparty.Service{Control: controlService, Executors: m, Credentials: credentialStore}
	if err := dockerRuntime.CleanupOrphans(context.Background(), m.Executors()); err != nil {
		log.Printf("docker orphan cleanup: %v", err)
	}
	relayCapabilitySecret := os.Getenv("PIE_RELAY_JWT_SECRET")
	if relayCapabilitySecret == "" {
		relayCapabilitySecret = os.Getenv("RELAY_JWT_SECRET")
	}
	issuer := capability.Issuer{
		Secret:        []byte(relayCapabilitySecret),
		RoutingSecret: []byte(os.Getenv("PIE_RELAY_ROUTING_SECRET")),
	}
	if !issuer.Enabled() {
		log.Print("Pie Relay capability 발급 비활성화: PIE_RELAY_JWT_SECRET를 32바이트 이상으로 설정하세요")
	}
	if len(issuer.RoutingSecret) < 32 {
		log.Fatal("PIE_RELAY_ROUTING_SECRET must be at least 32 bytes when managed Relay pool scoping is enabled")
	}
	relayControlToken := strings.TrimSpace(os.Getenv("PIE_RELAY_CONTROL_TOKEN"))
	var relayController controller.RelayControl
	if relayControlToken != "" {
		relayController = controller.HTTPRelayControl{Token: relayControlToken}
	} else {
		log.Print("Pie Relay 운영 제어 비활성화: PIE_RELAY_CONTROL_TOKEN을 Relay와 동일하게 설정하세요")
	}
	relayURL, err := resolveRelayAgentURL(os.Getenv("PIE_RELAY_URL"))
	if err != nil {
		log.Fatalf("Pie Relay URL: %v", err)
	}
	controlController, err := controller.New(rootCtx, m, controlService, controller.Options{
		NodeID:                managerID,
		Image:                 image,
		ReconcileInterval:     durationEnv("PIE_CONTROL_RECONCILE_INTERVAL", 10*time.Second),
		ReconcileConcurrency:  positiveEnv("PIE_CONTROL_RECONCILE_CONCURRENCY", 8),
		HeartbeatTimeout:      durationEnv("PIE_CONTROL_HEARTBEAT_TIMEOUT", 45*time.Second),
		RelayHeartbeatTimeout: durationEnv("PIE_RELAY_HEARTBEAT_TIMEOUT", 90*time.Second),
		OperationConcurrency:  positiveEnv("PIE_CONTROL_OPERATION_CONCURRENCY", 4),
		OperationTimeout:      durationEnv("PIE_CONTROL_OPERATION_TIMEOUT", 2*time.Minute),
		MaintenanceInterval:   durationEnv("PIE_CONTROL_MAINTENANCE_INTERVAL", time.Hour),
		OperationRetention:    durationEnv("PIE_CONTROL_OPERATION_RETENTION", 7*24*time.Hour),
		RelayURL:              relayURL,
		RelayControlURL:       os.Getenv("PIE_RELAY_CONTROL_URL"),
		RelayControl:          relayController,
		Issuer:                issuer,
		ClaudeOAuth:           claudeAuthService,
	})
	if err != nil {
		log.Fatalf("start Pie Control controller: %v", err)
	}
	defer controlController.Close()
	eventLifetime := 30 * time.Minute
	if raw := os.Getenv("PIE_EXECUTOR_SSE_LIFETIME"); raw != "" {
		eventLifetime, err = time.ParseDuration(raw)
		if err != nil || eventLifetime <= 0 {
			log.Fatal("PIE_EXECUTOR_SSE_LIFETIME must be a positive duration")
		}
	}
	blobs := store.NewBlobStore(blobRoot, 64<<20, positiveInt64Env("PIE_EXECUTOR_USER_BLOB_QUOTA_BYTES", 1<<30))
	chatJournalRoot := os.Getenv("PIE_CHAT_JOURNAL_DIR")
	if chatJournalRoot == "" {
		chatJournalRoot = filepath.Join(registryRoot, "chat-journal")
	}
	chatJournal, err := chatgateway.NewJournal(chatJournalRoot, positiveInt64Env("PIE_CHAT_JOURNAL_MAX_BYTES", 64<<20), positiveInt64Env("PIE_CHAT_EVENT_MAX_BYTES", 8<<20))
	if err != nil {
		log.Fatalf("initialize chat journal: %v", err)
	}
	chatGateway, err := chatgateway.New(rootCtx, controlService, issuer, relayURL, chatJournal)
	if err != nil {
		log.Fatalf("initialize chat gateway: %v", err)
	}
	chatGateway.SetTurnLimit(positiveEnv("PIE_CLAUDE_SUBSCRIPTION_MAX_CONCURRENT_TURNS", 4))
	var usageService *usageledger.Service
	usageDSN := strings.TrimSpace(os.Getenv("PIE_USAGE_DATABASE_URL"))
	if usageDSN == "" {
		usageDSN = strings.TrimSpace(os.Getenv("PIE_CONTROL_DATABASE_URL"))
	}
	if usageDSN != "" {
		usageStore, usageErr := usageledger.OpenPostgres(rootCtx, usageDSN)
		if usageErr != nil {
			log.Fatalf("initialize LLM usage ledger: %v", usageErr)
		}
		usageService = usageledger.NewService(usageStore)
		defer usageService.Close()
		chatGateway.SetUsageRecorder(usageService)
		go runUsageReconciler(rootCtx, usageService, controlService, chatJournal, durationEnv("PIE_USAGE_RECONCILE_INTERVAL", time.Minute))
	} else {
		log.Print("LLM 사용량 원장 비활성화: PIE_USAGE_DATABASE_URL 또는 PIE_CONTROL_DATABASE_URL을 설정하세요")
	}
	for _, conversation := range controlService.Conversations() {
		if conversation.Status != "closed" && conversation.Status != "deleted" {
			_ = chatGateway.Ensure(conversation)
		}
	}
	chatLifecycle := newChatLifecycle(m, controlService, chatGateway, chatLifecycleOptions{
		ScanInterval:        durationEnv("PIE_CHAT_IDLE_SCAN_INTERVAL", time.Minute),
		SessionIdleTimeout:  durationEnv("PIE_CHAT_SESSION_IDLE_TIMEOUT", 15*time.Minute),
		ExecutorIdleTimeout: durationEnv("PIE_EXECUTOR_IDLE_TIMEOUT", time.Hour),
	})
	go chatLifecycle.Run(rootCtx)
	var previewService *previewservice.Service
	previewDomain := strings.TrimSpace(os.Getenv("PIE_PREVIEW_DOMAIN"))
	previewGatewayContainer := strings.TrimSpace(os.Getenv("PIE_PREVIEW_GATEWAY_CONTAINER"))
	previewAccessSecret := []byte(os.Getenv("PIE_PREVIEW_ACCESS_SECRET"))
	previewGatewayToken := strings.TrimSpace(os.Getenv("PIE_PREVIEW_GATEWAY_TOKEN"))
	previewConfigured := previewDomain != "" || previewGatewayContainer != "" || len(previewAccessSecret) > 0 || previewGatewayToken != ""
	if previewConfigured {
		if len(previewGatewayToken) < 32 {
			log.Fatal("PIE_PREVIEW_GATEWAY_TOKEN must be at least 32 bytes when previews are enabled")
		}
		previewService, err = previewservice.New(rootCtx, controlService, m, previewservice.Options{
			Domain: previewDomain, PublicScheme: strings.TrimSpace(os.Getenv("PIE_PREVIEW_PUBLIC_SCHEME")), PublicPort: nonNegativeEnv("PIE_PREVIEW_PUBLIC_PORT", 0), GatewayContainer: previewGatewayContainer, AccessSecret: previewAccessSecret,
			ReconcileEvery: durationEnv("PIE_PREVIEW_RECONCILE_INTERVAL", 2*time.Second),
			DefaultTTL:     durationEnv("PIE_PREVIEW_DEFAULT_TTL", 4*time.Hour), MaxTTL: durationEnv("PIE_PREVIEW_MAX_TTL", 24*time.Hour),
			LaunchTTL: durationEnv("PIE_PREVIEW_LAUNCH_TTL", 2*time.Minute),
		})
		if err != nil {
			log.Fatalf("initialize preview service: %v", err)
		}
		defer previewService.Close()
	}
	a := api{m: m, verifier: verifiers, blobs: blobs, uploadSlots: make(chan struct{}, positiveEnv("PIE_EXECUTOR_UPLOAD_CONCURRENCY", 8)), eventSlots: make(chan struct{}, positiveEnv("PIE_EXECUTOR_SSE_CONNECTIONS", 256)), eventLifetime: eventLifetime, control: controlService, issuer: issuer, webhookSecret: []byte(os.Getenv("PIE_USER_WEBHOOK_SECRET")), webhookSkew: durationEnv("PIE_USER_WEBHOOK_MAX_SKEW", 5*time.Minute), relayPublicURL: strings.TrimRight(os.Getenv("PIE_RELAY_PUBLIC_URL"), "/"), thirdParty: thirdPartyService, chat: chatGateway, previews: previewService, usage: usageService, claudeAuth: claudeAuthService, claudeAuthWorkers: positiveEnv("PIE_CLAUDE_AUTH_ROLLOUT_CONCURRENCY", 4), previewGatewayToken: previewGatewayToken}
	httpStats := newHTTPMetrics()
	mux := http.NewServeMux()
	mux.Handle("/admin/", adminHandler())
	mux.Handle("/admin", adminHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := dockerRuntime.Health(ctx); err != nil {
			http.Error(w, "docker unavailable", 503)
			return
		}
		if err := controlService.Ping(ctx); err != nil {
			http.Error(w, "control store unavailable", 503)
			return
		}
		if a.usage != nil {
			if err := a.usage.Ping(ctx); err != nil {
				http.Error(w, "usage ledger unavailable", 503)
				return
			}
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := a.principal(w, r)
		if !ok {
			return
		}
		if !principal.CanViewAdmin() {
			http.Error(w, "admin viewer role required", http.StatusForbidden)
			return
		}
		s := m.Stats()
		w.Header().Set("content-type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "pie_executor_manager_executors %d\npie_executor_manager_active_executors %d\npie_executor_manager_executor_capacity %d\npie_executor_manager_queue_depth %d\npie_executor_manager_queue_capacity %d\npie_executor_manager_running %d\npie_executor_manager_active_users %d\npie_executor_manager_provisioning %d\n", s.Executors, s.ActiveExecutors, s.ExecutorCapacity, s.QueueDepth, s.QueueCapacity, s.Running, s.ActiveUsers, s.Provisioning)
		fmt.Fprintf(w, "pie_executor_manager_disk_used_bytes %d\npie_executor_manager_disk_quota_bytes %d\npie_executor_manager_disk_free_bytes %d\npie_executor_manager_disk_quota_exceeded %d\npie_executor_manager_disk_scan_errors_total %d\n", s.DiskUsedBytes, s.DiskQuotaBytes, s.DiskFreeBytes, s.DiskQuotaExceeded, s.DiskScanErrors)
		for status, count := range s.Jobs {
			fmt.Fprintf(w, "pie_executor_manager_jobs{status=%q} %d\n", status, count)
		}
		fmt.Fprintf(w, "pie_executor_manager_sse_connections %d\npie_executor_manager_uploads_active %d\n", len(a.eventSlots), len(a.uploadSlots))
		o := controlService.Overview()
		fmt.Fprintf(w, "pie_control_users %d\npie_control_online_devices %d\npie_control_running_runtimes %d\npie_control_active_sessions %d\npie_control_participants %d\npie_control_relay_connections %d\npie_control_integrations %d\npie_control_integration_users %d\npie_control_projects %d\npie_control_previews %d\npie_control_active_conversations %d\n", o.Users, o.OnlineDevices, o.RunningRuntimes, o.ActiveSessions, o.Participants, o.RelayConnections, o.Integrations, o.IntegrationUsers, o.Projects, o.Previews, o.ActiveConversations)
		for status, count := range o.IntegrationUsersByState {
			fmt.Fprintf(w, "pie_control_integration_users_by_state{status=%q} %d\n", status, count)
		}
		for status, count := range o.ConversationsByState {
			fmt.Fprintf(w, "pie_control_conversations_by_state{status=%q} %d\n", status, count)
		}
		chatStats := chatGateway.Stats()
		fmt.Fprintf(w, "pie_chat_gateway_peers %d\npie_chat_gateway_subscribers %d\npie_chat_gateway_queue_depth %d\npie_chat_gateway_active_turns %d\npie_chat_gateway_queued_turns %d\npie_chat_gateway_requests_started_total %d\npie_chat_gateway_requests_finished_total %d\npie_chat_gateway_requests_failed_total %d\n", chatStats.Peers, chatStats.Subscribers, chatStats.QueueDepth, chatStats.ActiveTurns, chatStats.QueuedTurns, chatStats.Started, chatStats.Finished, chatStats.Failed)
		lifecycleStats := chatLifecycle.Stats()
		fmt.Fprintf(w, "pie_chat_idle_sessions_closed_total %d\npie_chat_idle_executors_stopped_total %d\npie_chat_idle_errors_total %d\n", lifecycleStats.SessionsClosed, lifecycleStats.ExecutorsStopped, lifecycleStats.Errors)
		httpStats.writePrometheus(w)
	})
	mux.HandleFunc("/v1/admin/", a.handleAdmin)
	mux.HandleFunc("/v1/control/", a.handleControl)
	mux.HandleFunc("/v1/hooks/users", a.handleUserLifecycle)
	mux.HandleFunc("/v1/integrations/", a.handleIntegrations)
	mux.HandleFunc("/v1/internal/previews/route", a.handleInternalPreviewRoute)
	mux.HandleFunc("/v1/users/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/executor") {
			a.ensure(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/jobs") {
			a.submit(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/uploads") {
			a.upload(w, r)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			a.events(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			a.cancel(w, r)
			return
		}
		a.job(w, r)
	})
	handler := withRequestObservability(withCORS(mux, os.Getenv("PIE_CORS_ALLOWED_ORIGINS")), httpStats)
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 2 * time.Minute, WriteTimeout: 0, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	log.Printf("pie executor manager listening on %s", addr)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-rootCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
		m.Close()
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}
}
