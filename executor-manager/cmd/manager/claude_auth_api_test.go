package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/auth"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/claudeauth"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/credential"
)

func TestClaudeAuthAdminPublishAndStatusDoNotLeakCredential(t *testing.T) {
	a, _, _ := newAPIForTest(t)
	root := t.TempDir()
	targets, err := credential.New(filepath.Join(root, "state"), "")
	if err != nil {
		t.Fatal(err)
	}
	service, err := claudeauth.Open(filepath.Join(root, "auth"), filepath.Join(root, "login"), targets, true)
	if err != nil {
		t.Fatal(err)
	}
	secret := "sk-ant-oat-never-return-this-subscription-token-000000001"
	if err := os.WriteFile(filepath.Join(service.LoginDirectory(), "setup-token"), []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	a.claudeAuth, a.claudeAuthWorkers = service, 2

	publish := httptest.NewRecorder()
	request := authorizedRequest(http.MethodPost, "/v1/admin/claude-auth/publish", bytes.NewBufferString(`{"label":"server-login","restart":false}`))
	request.Header.Set("content-type", "application/json")
	a.handleAdmin(publish, request)
	if publish.Code != http.StatusOK {
		t.Fatalf("publish status=%d body=%s", publish.Code, publish.Body.String())
	}
	if strings.Contains(publish.Body.String(), "never-return-this") {
		t.Fatal("publish response leaked credential")
	}
	var result claudeAuthMutationResult
	if err := json.Unmarshal(publish.Body.Bytes(), &result); err != nil || result.Version.ID == "" || !result.Status.Configured {
		t.Fatalf("publish response=%s err=%v", publish.Body.String(), err)
	}

	status := httptest.NewRecorder()
	a.handleAdmin(status, authorizedRequest(http.MethodGet, "/v1/admin/claude-auth", nil))
	if status.Code != http.StatusOK || strings.Contains(status.Body.String(), "never-return-this") {
		t.Fatalf("status=%d body=%s", status.Code, status.Body.String())
	}
}

func TestClaudeAuthAdminRequiresAdministerRole(t *testing.T) {
	a, _, _ := newAPIForTest(t)
	a.verifier = principalVerifier{principal: auth.Principal{UserID: "viewer", Roles: []string{"pie-admin-viewer"}}}
	request := authorizedRequest(http.MethodPost, "/v1/admin/claude-auth/deploy", bytes.NewBufferString(`{}`))
	request.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	a.handleAdmin(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
