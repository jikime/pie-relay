package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/auth"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/capability"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/store"
)

type apiTestStore struct {
	mu   sync.Mutex
	jobs []manager.Job
}

func (s *apiTestStore) Load(context.Context) ([]manager.Executor, error) { return nil, nil }
func (s *apiTestStore) LoadJobs(context.Context) ([]manager.Job, error) {
	return append([]manager.Job(nil), s.jobs...), nil
}
func (s *apiTestStore) SaveExecutor(context.Context, manager.Executor) error { return nil }
func (s *apiTestStore) SaveJob(_ context.Context, job manager.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, job)
	return nil
}

type apiTestRuntime struct{}

func (apiTestRuntime) Ensure(context.Context, manager.Executor) error { return nil }
func (apiTestRuntime) Run(context.Context, manager.Job) ([]byte, error) {
	return []byte("ok"), nil
}
func (apiTestRuntime) Stop(context.Context, manager.Executor) error { return nil }

type retryProvisionRuntime struct {
	mu          sync.Mutex
	ensureCalls int
}

func (r *retryProvisionRuntime) Ensure(context.Context, manager.Executor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureCalls++
	if r.ensureCalls == 1 {
		return fmt.Errorf("temporary runtime failure")
	}
	return nil
}
func (*retryProvisionRuntime) Run(context.Context, manager.Job) ([]byte, error) {
	return []byte("ok"), nil
}
func (*retryProvisionRuntime) Stop(context.Context, manager.Executor) error { return nil }

func newAPIForTest(t *testing.T, jobs ...manager.Job) (api, *manager.Manager, *store.BlobStore) {
	t.Helper()
	m, err := manager.New(context.Background(), apiTestRuntime{}, &apiTestStore{jobs: jobs}, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	blobs := store.NewBlobStore(t.TempDir(), 1<<20, 2<<20)
	controlService, err := control.NewService(context.Background(), control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlService.Close() })
	return api{m: m, verifier: auth.Static{Token: "test-token"}, blobs: blobs, uploadSlots: make(chan struct{}, 1), eventSlots: make(chan struct{}, 1), eventLifetime: time.Minute, control: controlService, issuer: capability.Issuer{Secret: []byte("01234567890123456789012345678901")}}, m, blobs
}

func authorizedRequest(method, target string, body *bytes.Buffer) *http.Request {
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, body)
	}
	req.Header.Set("authorization", "Bearer test-token")
	return req
}

func TestEventsSendsTerminalSnapshotImmediately(t *testing.T) {
	now := time.Now().UTC()
	a, _, _ := newAPIForTest(t, manager.Job{ID: "job-terminal", UserID: "user", Status: "succeeded", CreatedAt: &now, FinishedAt: &now, Output: []byte("ok")})
	recorder := httptest.NewRecorder()
	a.events(recorder, authorizedRequest(http.MethodGet, "/v1/jobs/job-terminal/events", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `data: {`) || !strings.Contains(body, `"status":"succeeded"`) {
		t.Fatalf("body=%q", body)
	}
}

func TestEventsRejectsConnectionOverflow(t *testing.T) {
	now := time.Now().UTC()
	a, _, _ := newAPIForTest(t, manager.Job{ID: "job-terminal", UserID: "user", Status: "succeeded", CreatedAt: &now, FinishedAt: &now})
	a.eventSlots <- struct{}{}
	recorder := httptest.NewRecorder()
	a.events(recorder, authorizedRequest(http.MethodGet, "/v1/jobs/job-terminal/events", nil))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestUploadDeleteAndMissingBlobSubmission(t *testing.T) {
	a, _, blobs := newAPIForTest(t)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "input.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("payload"))
	_ = w.Close()
	req := authorizedRequest(http.MethodPost, "/v1/users/user/uploads", &body)
	req.Header.Set("content-type", w.FormDataContentType())
	recorder := httptest.NewRecorder()
	a.upload(recorder, req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var uploaded struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &uploaded); err != nil || uploaded.Ref == "" {
		t.Fatalf("response=%s err=%v", recorder.Body.String(), err)
	}
	if err := blobs.ValidateRefs("user", []string{uploaded.Ref}); err != nil {
		t.Fatal(err)
	}

	jobBody := bytes.NewBufferString(`{"command":"echo ok","blobRefs":["user/missing"]}`)
	jobRequest := authorizedRequest(http.MethodPost, "/v1/users/user/jobs", jobBody)
	jobRecorder := httptest.NewRecorder()
	a.submit(jobRecorder, jobRequest)
	if jobRecorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", jobRecorder.Code, jobRecorder.Body.String())
	}

	deleteRequest := authorizedRequest(http.MethodDelete, "/v1/users/user/uploads?ref="+uploaded.Ref, nil)
	deleteRecorder := httptest.NewRecorder()
	a.upload(deleteRecorder, deleteRequest)
	if deleteRecorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	if err := blobs.ValidateRefs("user", []string{uploaded.Ref}); err == nil {
		t.Fatal("deleted blob still exists")
	}
}

func TestControlAPIScopedCredentialAndRelayPresence(t *testing.T) {
	a, _, _ := newAPIForTest(t)
	a.relayPublicURL = "https://relay.cookai.dev"
	requestJSON := func(method, path, body string, handler func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
		req := authorizedRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("content-type", "application/json")
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	user := requestJSON(http.MethodPost, "/v1/admin/users", `{"id":"owner","status":"active"}`, a.handleAdmin)
	if user.Code != http.StatusCreated {
		t.Fatalf("user: %d %s", user.Code, user.Body.String())
	}
	device := requestJSON(http.MethodPost, "/v1/admin/devices", `{"id":"device-a","ownerUserId":"owner","name":"Docker","kind":"docker"}`, a.handleAdmin)
	if device.Code != http.StatusCreated {
		t.Fatalf("device: %d %s", device.Code, device.Body.String())
	}
	session := requestJSON(http.MethodPost, "/v1/admin/sessions", `{"id":"session-a","ownerUserId":"owner","deviceId":"device-a","executionTarget":"docker","accessMode":"shared","transportMode":"relay","status":"active"}`, a.handleAdmin)
	if session.Code != http.StatusCreated {
		t.Fatalf("session: %d %s", session.Code, session.Body.String())
	}
	credential := requestJSON(http.MethodPost, "/v1/control/sessions/session-a/credential", `{"subjectUserId":"owner","role":"host","access":"control"}`, a.handleControl)
	if credential.Code != http.StatusCreated {
		t.Fatalf("credential: %d %s", credential.Code, credential.Body.String())
	}
	var minted capability.Minted
	if err := json.Unmarshal(credential.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	if minted.Token == "" || minted.DeviceID != "device-a" || minted.SessionID != "session-a" || minted.ExecutionTarget != "docker" || minted.Role != "host" {
		t.Fatalf("minted=%+v", minted)
	}
	if !strings.Contains(credential.Body.String(), `"relayUrl":"https://relay.cookai.dev"`) {
		t.Fatalf("credential missing public relay URL: %s", credential.Body.String())
	}
	a.verifier = auth.Static{Token: "test-token", Principal: auth.Principal{Roles: []string{"pie-relay-presence"}}}
	presence := requestJSON(http.MethodPost, "/v1/control/relay/presence", `{"eventId":"event-a","nodeId":"cell-a","publicUrl":"https://relay.cookai.dev","room":"owner","deviceId":"device-a","sessionId":"session-a","relayGeneration":1,"userId":"owner","role":"host","access":"control","connectionId":"connection-a","kind":"host","connected":true,"hostOnline":true,"at":"2026-07-24T12:00:00Z"}`, a.handleControl)
	if presence.Code != http.StatusNoContent {
		t.Fatalf("presence: %d %s", presence.Code, presence.Body.String())
	}
	got, _ := a.control.Device("device-a")
	if !got.ClientConnected || !got.RelayRegistered || got.RelayNodeID != "cell-a" || got.ObservedState != "online" || got.ActiveSessions != 1 {
		t.Fatalf("device presence=%+v", got)
	}
	tracked, _ := a.control.Session("session-a")
	if tracked.Status != "active" || tracked.RelayNodeID != "cell-a" || tracked.HostConnectionID != "connection-a" {
		t.Fatalf("session presence=%+v", tracked)
	}
	node, _ := a.control.Node("cell-a")
	if node.Address != "https://relay.cookai.dev" {
		t.Fatalf("relay node=%+v", node)
	}
}

func TestHostOSDeviceReceivesAndReportsAssignedSession(t *testing.T) {
	a, _, _ := newAPIForTest(t)
	ctx := context.Background()
	if _, err := a.control.PutUser(ctx, control.User{ID: "owner", Status: "active"}, 0, control.MutationMeta{ActorUserID: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.control.RegisterDevice(ctx, control.Device{
		ID:          "host-device",
		OwnerUserID: "owner",
		Name:        "Office Mac",
		Kind:        "local",
	}, control.MutationMeta{ActorUserID: "owner"}); err != nil {
		t.Fatal(err)
	}
	a.verifier = auth.Static{Token: "test-token", Principal: auth.Principal{UserID: "owner"}}

	create := httptest.NewRecorder()
	createRequest := authorizedRequest(http.MethodPost, "/v1/control/sessions", bytes.NewBufferString(`{"id":"host-session","deviceId":"host-device","executionTarget":"local","accessMode":"private","transportMode":"relay","status":"starting"}`))
	createRequest.Header.Set("content-type", "application/json")
	a.handleControl(create, createRequest)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}

	feed := httptest.NewRecorder()
	a.handleControl(feed, authorizedRequest(http.MethodGet, "/v1/control/devices/host-device/sessions", nil))
	if feed.Code != http.StatusOK || !strings.Contains(feed.Body.String(), `"id":"host-session"`) || !strings.Contains(feed.Body.String(), `"status":"starting"`) {
		t.Fatalf("feed status=%d body=%s", feed.Code, feed.Body.String())
	}

	report := httptest.NewRecorder()
	reportRequest := authorizedRequest(http.MethodPost, "/v1/control/devices/host-device/sessions/host-session/status", bytes.NewBufferString(`{"status":"ready","selectedTransport":"relay"}`))
	reportRequest.Header.Set("content-type", "application/json")
	a.handleControl(report, reportRequest)
	if report.Code != http.StatusOK {
		t.Fatalf("report status=%d body=%s", report.Code, report.Body.String())
	}
	updated, ok := a.control.Session("host-session")
	if !ok || updated.Status != "ready" || updated.SelectedTransport != "relay" {
		t.Fatalf("updated session=%+v found=%t", updated, ok)
	}

	a.verifier = auth.Static{Token: "test-token", Principal: auth.Principal{UserID: "guest"}}
	forbidden := httptest.NewRecorder()
	a.handleControl(forbidden, authorizedRequest(http.MethodGet, "/v1/control/devices/host-device/sessions", nil))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("foreign feed status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
}

func TestControlOperatorCanCreateSessionForPairedHostDevice(t *testing.T) {
	a, _, _ := newAPIForTest(t)
	ctx := context.Background()
	if _, err := a.control.PutUser(ctx, control.User{ID: "owner", Status: "active"}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.control.RegisterDevice(ctx, control.Device{ID: "canvas-host", OwnerUserID: "owner", Kind: "local", Name: "MacBook"}, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	a.verifier = auth.Static{Token: "test-token", Principal: auth.Principal{UserID: "pie-canvas-agent", Roles: []string{"pie-operator"}}}

	create := httptest.NewRecorder()
	request := authorizedRequest(http.MethodPost, "/v1/control/sessions", bytes.NewBufferString(`{"id":"host-session","ownerUserId":"owner","deviceId":"canvas-host","executionTarget":"local","agentMode":"acp","accessMode":"private","transportMode":"relay","status":"starting"}`))
	request.Header.Set("content-type", "application/json")
	a.handleControl(create, request)
	if create.Code != http.StatusCreated {
		t.Fatalf("operator session creation status=%d body=%s", create.Code, create.Body.String())
	}
	created, ok := a.control.Session("host-session")
	if !ok || created.OwnerUserID != "owner" || created.DeviceID != "canvas-host" || created.ExecutionTarget != "local" {
		t.Fatalf("unexpected operator-created session: %+v", created)
	}

	list := httptest.NewRecorder()
	a.handleControl(list, authorizedRequest(http.MethodGet, "/v1/control/sessions", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"id":"host-session"`) {
		t.Fatalf("operator session list status=%d body=%s", list.Code, list.Body.String())
	}
}

func TestDevicePrincipalIsRestrictedToItsDevice(t *testing.T) {
	a, _, _ := newAPIForTest(t)
	ctx := context.Background()
	if _, err := a.control.PutUser(ctx, control.User{ID: "owner", Status: "active"}, 0, control.MutationMeta{ActorUserID: "admin"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"device-a", "device-b"} {
		if _, err := a.control.RegisterDevice(ctx, control.Device{ID: id, OwnerUserID: "owner", Kind: "local", Name: id}, control.MutationMeta{ActorUserID: "owner"}); err != nil {
			t.Fatal(err)
		}
	}
	a.verifier = principalVerifier{principal: auth.Principal{UserID: "owner", OrganizationID: "workspace-a", DeviceID: "device-a"}}

	devices := httptest.NewRecorder()
	a.handleControl(devices, authorizedRequest(http.MethodGet, "/v1/control/devices", nil))
	if devices.Code != http.StatusOK || !strings.Contains(devices.Body.String(), `"id":"device-a"`) || strings.Contains(devices.Body.String(), `"id":"device-b"`) {
		t.Fatalf("scoped devices status=%d body=%s", devices.Code, devices.Body.String())
	}

	foreignFeed := httptest.NewRecorder()
	a.handleControl(foreignFeed, authorizedRequest(http.MethodGet, "/v1/control/devices/device-b/sessions", nil))
	if foreignFeed.Code != http.StatusForbidden {
		t.Fatalf("foreign feed status=%d body=%s", foreignFeed.Code, foreignFeed.Body.String())
	}

	createSession := httptest.NewRecorder()
	request := authorizedRequest(http.MethodPost, "/v1/control/sessions", bytes.NewBufferString(`{"id":"forbidden","deviceId":"device-a","executionTarget":"local"}`))
	request.Header.Set("content-type", "application/json")
	a.handleControl(createSession, request)
	if createSession.Code != http.StatusForbidden {
		t.Fatalf("device session creation status=%d body=%s", createSession.Code, createSession.Body.String())
	}
}

func TestCORSAllowsConfiguredDesktopOriginOnly(t *testing.T) {
	handler := withCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), "tauri://localhost,http://localhost:1420")

	allowed := httptest.NewRequest(http.MethodOptions, "/v1/control/me", nil)
	allowed.Header.Set("Origin", "tauri://localhost")
	allowedResponse := httptest.NewRecorder()
	handler.ServeHTTP(allowedResponse, allowed)
	if allowedResponse.Code != http.StatusNoContent || allowedResponse.Header().Get("Access-Control-Allow-Origin") != "tauri://localhost" {
		t.Fatalf("allowed status=%d headers=%v", allowedResponse.Code, allowedResponse.Header())
	}
	if !strings.Contains(allowedResponse.Header().Get("Access-Control-Allow-Headers"), "Cache-Control") {
		t.Fatalf("cache-control request header is not allowed: %v", allowedResponse.Header())
	}

	blocked := httptest.NewRequest(http.MethodGet, "/v1/control/me", nil)
	blocked.Header.Set("Origin", "https://attacker.example")
	blockedResponse := httptest.NewRecorder()
	handler.ServeHTTP(blockedResponse, blocked)
	if blockedResponse.Code != http.StatusForbidden {
		t.Fatalf("blocked status=%d", blockedResponse.Code)
	}

	// ES modules are fetched in CORS mode and include Origin even when served
	// by the same Manager. The Admin UI must not block its own app.js.
	sameOrigin := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:19090/admin/app.js", nil)
	sameOrigin.Host = "127.0.0.1:19090"
	sameOrigin.Header.Set("Origin", "http://127.0.0.1:19090")
	sameOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(sameOriginResponse, sameOrigin)
	if sameOriginResponse.Code != http.StatusOK {
		t.Fatalf("same-origin module status=%d body=%s", sameOriginResponse.Code, sameOriginResponse.Body.String())
	}

	proxiedSameOrigin := httptest.NewRequest(http.MethodGet, "/admin/app.js", nil)
	proxiedSameOrigin.Host = "admin-relay.localhost:18443"
	proxiedSameOrigin.Header.Set("Origin", "https://admin-relay.localhost:18443")
	proxiedSameOrigin.Header.Set("X-Forwarded-Proto", "https")
	proxiedResponse := httptest.NewRecorder()
	handler.ServeHTTP(proxiedResponse, proxiedSameOrigin)
	if proxiedResponse.Code != http.StatusOK {
		t.Fatalf("proxied same-origin module status=%d", proxiedResponse.Code)
	}

	wrongScheme := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:19090/admin/app.js", nil)
	wrongScheme.Host = "127.0.0.1:19090"
	wrongScheme.Header.Set("Origin", "https://127.0.0.1:19090")
	wrongSchemeResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongSchemeResponse, wrongScheme)
	if wrongSchemeResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-scheme origin status=%d", wrongSchemeResponse.Code)
	}

	serverToServer := httptest.NewRecorder()
	handler.ServeHTTP(serverToServer, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if serverToServer.Code != http.StatusOK {
		t.Fatalf("server-to-server status=%d", serverToServer.Code)
	}
}

func TestSignedLifecycleProvisionSuspendAndReplay(t *testing.T) {
	a, m, _ := newAPIForTest(t)
	a.webhookSecret = []byte("01234567890123456789012345678901")
	a.webhookSkew = time.Minute
	baseOccurredAt := time.Now().Add(-5 * time.Minute).UTC()
	call := func(body string) *httptest.ResponseRecorder {
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, a.webhookSecret)
		_, _ = mac.Write([]byte(timestamp + "." + body))
		request := httptest.NewRequest(http.MethodPost, "/v1/hooks/users", strings.NewReader(body))
		request.Header.Set("X-Pie-Timestamp", timestamp)
		request.Header.Set("X-Pie-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
		response := httptest.NewRecorder()
		a.handleUserLifecycle(response, request)
		return response
	}

	created := call(fmt.Sprintf(`{"id":"event-create","type":"user.created","occurredAt":%q,"provision":true,"user":{"id":"customer-a","externalSubject":"external-a","organizationId":"org-a","quota":{"cpus":"1.5","memoryBytes":1073741824,"pids":96,"maxSessions":2,"maxParticipants":4}}}`, baseOccurredAt.Format(time.RFC3339Nano)))
	if created.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	user, ok := a.control.User("customer-a")
	if !ok || user.Status != "active" || user.ExternalSubject != "external-a" {
		t.Fatalf("user=%+v", user)
	}
	if executor, ok := m.Executor("customer-a"); !ok || executor.Status != "ready" || executor.CPUs != "1.5" || executor.MemoryBytes != 1<<30 || executor.PIDsLimit != 96 {
		t.Fatalf("executor=%+v ok=%t", executor, ok)
	}
	if _, err := a.control.RegisterDevice(context.Background(), control.Device{ID: "customer-device", OwnerUserID: "customer-a", Kind: "local"}, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}

	suspendedBody := fmt.Sprintf(`{"id":"event-suspend","type":"user.suspended","occurredAt":%q,"user":{"id":"customer-a"}}`, baseOccurredAt.Add(time.Minute).Format(time.RFC3339Nano))
	suspended := call(suspendedBody)
	if suspended.Code != http.StatusAccepted {
		t.Fatalf("suspend status=%d body=%s", suspended.Code, suspended.Body.String())
	}
	user, _ = a.control.User("customer-a")
	if user.Status != "suspended" {
		t.Fatalf("user=%+v", user)
	}
	if executor, _ := m.Executor("customer-a"); executor.Status != "stopped" {
		t.Fatalf("executor=%+v", executor)
	}
	version := user.Version
	if replay := call(suspendedBody); replay.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	user, _ = a.control.User("customer-a")
	if user.Version != version {
		t.Fatalf("duplicate event changed user version: before=%d after=%d", version, user.Version)
	}
	if operations := a.control.Operations(); len(operations) != 1 {
		t.Fatalf("operations=%+v", operations)
	}
	mutatedReplay := call(fmt.Sprintf(`{"id":"event-suspend","type":"user.suspended","occurredAt":%q,"user":{"id":"customer-a","organizationId":"changed-org"}}`, baseOccurredAt.Add(time.Minute).Format(time.RFC3339Nano)))
	if mutatedReplay.Code != http.StatusConflict {
		t.Fatalf("mutated replay status=%d body=%s", mutatedReplay.Code, mutatedReplay.Body.String())
	}
	stale := call(fmt.Sprintf(`{"id":"late-create","type":"user.created","occurredAt":%q,"provision":true,"user":{"id":"customer-a"}}`, baseOccurredAt.Add(30*time.Second).Format(time.RFC3339Nano)))
	if stale.Code != http.StatusAccepted {
		t.Fatalf("stale status=%d body=%s", stale.Code, stale.Body.String())
	}
	user, _ = a.control.User("customer-a")
	if user.Status != "suspended" {
		t.Fatalf("stale create reactivated user: %+v", user)
	}
	futureOccurredAt := time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)
	future := call(fmt.Sprintf(`{"id":"future-event","type":"user.reactivated","occurredAt":%q,"user":{"id":"customer-a"}}`, futureOccurredAt))
	if future.Code != http.StatusBadRequest {
		t.Fatalf("future status=%d body=%s", future.Code, future.Body.String())
	}
}

func TestLifecycleRejectsInvalidOrStaleSignature(t *testing.T) {
	a, _, _ := newAPIForTest(t)
	a.webhookSecret = []byte("01234567890123456789012345678901")
	a.webhookSkew = time.Minute
	body := `{"id":"event-a","type":"user.created","occurredAt":"2026-07-25T00:00:00Z","user":{"id":"customer-a"}}`
	request := httptest.NewRequest(http.MethodPost, "/v1/hooks/users", strings.NewReader(body))
	request.Header.Set("X-Pie-Timestamp", strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10))
	request.Header.Set("X-Pie-Signature", "v1=00")
	response := httptest.NewRecorder()
	a.handleUserLifecycle(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLifecycleDuplicateRetriesFailedProvisioning(t *testing.T) {
	runtime := &retryProvisionRuntime{}
	m, err := manager.New(context.Background(), runtime, &apiTestStore{}, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	controlService, err := control.NewService(context.Background(), control.NewDirectoryStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlService.Close() })
	a := api{
		m: m, control: controlService,
		webhookSecret: []byte("01234567890123456789012345678901"), webhookSkew: time.Minute,
	}
	body := fmt.Sprintf(`{"id":"retry-create","type":"user.created","occurredAt":%q,"provision":true,"user":{"id":"retry-user"}}`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano))
	call := func() *httptest.ResponseRecorder {
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, a.webhookSecret)
		_, _ = mac.Write([]byte(timestamp + "." + body))
		request := httptest.NewRequest(http.MethodPost, "/v1/hooks/users", strings.NewReader(body))
		request.Header.Set("X-Pie-Timestamp", timestamp)
		request.Header.Set("X-Pie-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
		response := httptest.NewRecorder()
		a.handleUserLifecycle(response, request)
		return response
	}

	first := call()
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	user, ok := a.control.User("retry-user")
	if !ok || user.Status != "active" {
		t.Fatalf("persisted user=%+v ok=%t", user, ok)
	}
	version := user.Version
	second := call()
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"duplicate":true`) {
		t.Fatalf("retry status=%d body=%s", second.Code, second.Body.String())
	}
	user, _ = a.control.User("retry-user")
	if user.Version != version {
		t.Fatalf("retry rewrote user: before=%d after=%d", version, user.Version)
	}
	executor, ok := m.Executor("retry-user")
	if !ok || executor.Status != "ready" {
		t.Fatalf("executor=%+v ok=%t", executor, ok)
	}
	runtime.mu.Lock()
	calls := runtime.ensureCalls
	runtime.mu.Unlock()
	if calls != 2 {
		t.Fatalf("ensure calls=%d", calls)
	}
}

func TestControlAPIListsGrantedResourcesBeforeParticipantConnects(t *testing.T) {
	a, _, _ := newAPIForTest(t)
	ctx := context.Background()
	for _, id := range []string{"owner", "guest"} {
		if _, err := a.control.PutUser(ctx, control.User{ID: id, Status: "active"}, 0, control.MutationMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.control.RegisterDevice(ctx, control.Device{ID: "device-shared", OwnerUserID: "owner", Name: "Shared Linux", Kind: "local"}, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.control.RegisterDevice(ctx, control.Device{ID: "device-private", OwnerUserID: "owner", Name: "Private Linux", Kind: "local"}, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.control.PutSession(ctx, control.Session{ID: "session-shared", OwnerUserID: "owner", DeviceID: "device-shared", Name: "Shared", ExecutionTarget: "local", AccessMode: "shared", TransportMode: "relay", Status: "active"}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.control.PutSession(ctx, control.Session{ID: "session-private", OwnerUserID: "owner", DeviceID: "device-private", Name: "Private", ExecutionTarget: "local", AccessMode: "private", TransportMode: "relay", Status: "active"}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.control.PutGrant(ctx, control.AccessGrant{OwnerUserID: "owner", SubjectUserID: "guest", TargetDeviceID: "device-shared", SessionID: "session-shared", Access: "view", ExpiresAt: time.Now().Add(time.Hour)}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	a.verifier = principalVerifier{principal: auth.Principal{UserID: "guest"}}

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.handleControl(rec, authorizedRequest(http.MethodGet, path, nil))
		return rec
	}
	devices := get("/v1/control/devices")
	if devices.Code != http.StatusOK || !strings.Contains(devices.Body.String(), "device-shared") || strings.Contains(devices.Body.String(), "device-private") {
		t.Fatalf("devices status=%d body=%s", devices.Code, devices.Body.String())
	}
	sessions := get("/v1/control/sessions")
	if sessions.Code != http.StatusOK || !strings.Contains(sessions.Body.String(), "session-shared") || strings.Contains(sessions.Body.String(), "session-private") {
		t.Fatalf("sessions status=%d body=%s", sessions.Code, sessions.Body.String())
	}
	grants := get("/v1/control/grants")
	if grants.Code != http.StatusOK || !strings.Contains(grants.Body.String(), "session-shared") {
		t.Fatalf("grants status=%d body=%s", grants.Code, grants.Body.String())
	}

	view := httptest.NewRecorder()
	viewReq := authorizedRequest(http.MethodPost, "/v1/control/sessions/session-shared/credential", bytes.NewBufferString(`{"role":"participant","access":"view"}`))
	viewReq.Header.Set("content-type", "application/json")
	a.handleControl(view, viewReq)
	if view.Code != http.StatusCreated {
		t.Fatalf("view credential status=%d body=%s", view.Code, view.Body.String())
	}
	controlCredential := httptest.NewRecorder()
	controlReq := authorizedRequest(http.MethodPost, "/v1/control/sessions/session-shared/credential", bytes.NewBufferString(`{"role":"participant","access":"control"}`))
	controlReq.Header.Set("content-type", "application/json")
	a.handleControl(controlCredential, controlReq)
	if controlCredential.Code != http.StatusForbidden {
		t.Fatalf("control credential status=%d body=%s", controlCredential.Code, controlCredential.Body.String())
	}
}

func TestControlAPIAllowsOwnersToCreateAndRevokeGrants(t *testing.T) {
	a, _, _ := newAPIForTest(t)
	ctx := context.Background()
	for _, id := range []string{"owner", "guest"} {
		if _, err := a.control.PutUser(ctx, control.User{ID: id, Status: "active"}, 0, control.MutationMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.control.RegisterDevice(ctx, control.Device{ID: "device-a", OwnerUserID: "owner", Kind: "local"}, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.control.PutSession(ctx, control.Session{ID: "session-a", OwnerUserID: "owner", DeviceID: "device-a", ExecutionTarget: "local", AccessMode: "shared", TransportMode: "relay", Status: "active"}, 0, control.MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	a.verifier = principalVerifier{principal: auth.Principal{UserID: "owner"}}
	body := `{"ownerUserId":"attacker","subjectUserId":"guest","targetDeviceId":"device-a","sessionId":"session-a","access":"control","expiresAt":"2030-01-01T00:00:00Z"}`
	create := httptest.NewRecorder()
	request := authorizedRequest(http.MethodPost, "/v1/control/grants", bytes.NewBufferString(body))
	request.Header.Set("content-type", "application/json")
	a.handleControl(create, request)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var grant control.AccessGrant
	if err := json.Unmarshal(create.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	if grant.OwnerUserID != "owner" || grant.SubjectUserID != "guest" {
		t.Fatalf("grant=%+v", grant)
	}

	a.verifier = principalVerifier{principal: auth.Principal{UserID: "guest"}}
	blocked := httptest.NewRecorder()
	a.handleControl(blocked, authorizedRequest(http.MethodPost, "/v1/control/grants/"+grant.ID+"/revoke", bytes.NewBufferString(`{}`)))
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("guest revoke status=%d body=%s", blocked.Code, blocked.Body.String())
	}

	a.verifier = principalVerifier{principal: auth.Principal{UserID: "owner"}}
	revoked := httptest.NewRecorder()
	a.handleControl(revoked, authorizedRequest(http.MethodPost, "/v1/control/grants/"+grant.ID+"/revoke", bytes.NewBufferString(`{}`)))
	if revoked.Code != http.StatusOK {
		t.Fatalf("owner revoke status=%d body=%s", revoked.Code, revoked.Body.String())
	}
}

type principalVerifier struct{ principal auth.Principal }

func (v principalVerifier) Verify(context.Context, string) (auth.Principal, error) {
	return v.principal, nil
}

func TestAdminViewerCannotMutate(t *testing.T) {
	a, _, _ := newAPIForTest(t)
	a.verifier = principalVerifier{principal: auth.Principal{UserID: "viewer", Roles: []string{"pie-admin-viewer"}}}
	req := authorizedRequest(http.MethodPost, "/v1/admin/users", bytes.NewBufferString(`{"id":"blocked"}`))
	rec := httptest.NewRecorder()
	a.handleAdmin(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEmbeddedAdminHasSecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	adminHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Pie Relay Control") {
		t.Fatalf("status=%d", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("admin security headers missing")
	}

	asset := httptest.NewRecorder()
	adminHandler().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/admin/forms.css", nil))
	if asset.Code != http.StatusOK || !strings.Contains(asset.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("forms.css status=%d content-type=%q", asset.Code, asset.Header().Get("Content-Type"))
	}
	if !strings.Contains(asset.Body.String(), ".entity-fields") {
		t.Fatal("embedded forms.css content missing")
	}

	app := httptest.NewRecorder()
	adminHandler().ServeHTTP(app, httptest.NewRequest(http.MethodGet, "/admin/app.js", nil))
	if app.Code != http.StatusOK || !strings.Contains(app.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("app.js status=%d content-type=%q", app.Code, app.Header().Get("Content-Type"))
	}
	for _, marker := range []string{"data-user-edit", "memoryMiB", "pids", "maxParticipants", "Manager가 이 토큰을 검증하지 못했습니다", "pie:admin:view", "localStorage.setItem('pie-admin-token'"} {
		if !strings.Contains(app.Body.String(), marker) {
			t.Fatalf("embedded app.js is missing quota editor marker %q", marker)
		}
	}
	if strings.Contains(app.Body.String(), "/v1/admin/snapshot") || !strings.Contains(app.Body.String(), "pendingRefresh") {
		t.Fatal("embedded app.js does not use bounded, coalesced view refreshes")
	}
	authCSS := httptest.NewRecorder()
	adminHandler().ServeHTTP(authCSS, httptest.NewRequest(http.MethodGet, "/admin/auth.css", nil))
	if authCSS.Code != http.StatusOK || !strings.Contains(authCSS.Body.String(), ".auth-summary") {
		t.Fatalf("auth.css status=%d body=%s", authCSS.Code, authCSS.Body.String())
	}
}
