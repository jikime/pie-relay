package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/chatgateway"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/thirdparty"
	usageledger "github.com/pielab-ai/pie-relay/executor-manager/internal/usage"
)

func (a api) handleAdminIntegrations(w http.ResponseWriter, r *http.Request, principalAdmin bool, path string) bool {
	if path == "integration-users" && r.Method == http.MethodGet {
		write(w, limitSlice(a.control.IntegrationUsers(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
		return true
	}
	if path == "conversations" && r.Method == http.MethodGet {
		write(w, limitSlice(a.control.Conversations(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
		return true
	}
	if path == "projects" && r.Method == http.MethodGet {
		write(w, limitSlice(a.control.Projects(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
		return true
	}
	if path == "previews" && r.Method == http.MethodGet {
		write(w, limitSlice(a.control.Previews(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
		return true
	}
	if path == "integrations" {
		switch r.Method {
		case http.MethodGet:
			write(w, limitSlice(a.control.Integrations(), positiveQueryInt(r, "limit", 500, 5000)), http.StatusOK)
		case http.MethodPost:
			if !principalAdmin {
				http.Error(w, "administrator role required", http.StatusForbidden)
				return true
			}
			var value control.Integration
			if !decodeControlJSON(w, r, &value) {
				return true
			}
			registered, err := a.thirdParty.Register(r.Context(), value, control.MutationMeta{ActorUserID: "admin", RequestID: r.Header.Get("X-Request-Id")})
			writeControlResult(w, registered, http.StatusCreated, err)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return true
	}
	if !strings.HasPrefix(path, "integrations/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "integrations/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		http.NotFound(w, r)
		return true
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "rotate-token" && r.Method == http.MethodPost {
		if !principalAdmin {
			http.Error(w, "administrator role required", http.StatusForbidden)
			return true
		}
		token, err := a.thirdParty.RotateToken(r.Context(), id)
		if err != nil {
			writeControlError(w, err)
			return true
		}
		write(w, map[string]string{"integrationId": id, "serviceToken": token}, http.StatusOK)
		return true
	}
	if len(parts) == 2 && parts[1] == "revoke" && r.Method == http.MethodPost {
		if !principalAdmin {
			http.Error(w, "administrator role required", http.StatusForbidden)
			return true
		}
		value, ok := a.control.Integration(id)
		if !ok {
			http.NotFound(w, r)
			return true
		}
		value.Status = "revoked"
		updated, err := a.control.PutIntegration(r.Context(), value, value.Version, control.MutationMeta{ActorUserID: "admin", RequestID: r.Header.Get("X-Request-Id")})
		writeControlResult(w, updated, http.StatusOK, err)
		return true
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return true
	}
	value, ok := a.control.Integration(id)
	if !ok {
		http.NotFound(w, r)
		return true
	}
	switch r.Method {
	case http.MethodGet:
		write(w, value, http.StatusOK)
	case http.MethodPut, http.MethodPatch:
		if !principalAdmin {
			http.Error(w, "administrator role required", http.StatusForbidden)
			return true
		}
		var update control.Integration
		if r.Method == http.MethodPatch {
			update = value
		}
		if !decodeControlJSON(w, r, &update) {
			return true
		}
		if update.ID == "" {
			update.ID = id
		}
		if update.ID != id {
			http.Error(w, "integration id cannot change", http.StatusBadRequest)
			return true
		}
		if update.Version == 0 {
			update.Version = value.Version
		}
		updated, err := a.control.PutIntegration(r.Context(), update, update.Version, control.MutationMeta{ActorUserID: "admin", RequestID: r.Header.Get("X-Request-Id")})
		writeControlResult(w, updated, http.StatusOK, err)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
	return true
}

func (a api) handleIntegrations(w http.ResponseWriter, r *http.Request) {
	if a.thirdParty == nil || a.chat == nil {
		http.Error(w, "integration service unavailable", http.StatusServiceUnavailable)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/integrations/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	integrationID := parts[0]
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	integration, err := a.thirdParty.Authenticate(integrationID, token)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	meta := control.MutationMeta{ActorUserID: "integration:" + integrationID, RequestID: r.Header.Get("X-Request-Id"), SourceIP: r.RemoteAddr, Trusted: true}
	if parts[1] == "conversations" {
		a.handleIntegrationConversation(w, r, integration, parts[2:], meta)
		return
	}
	if len(parts) < 3 || parts[1] != "users" || parts[2] == "" {
		http.NotFound(w, r)
		return
	}
	externalUserID := parts[2]
	bindingID := thirdparty.BindingID(integrationID, externalUserID)
	binding, exists := a.control.IntegrationUser(bindingID)
	if a.handleIntegrationPreviews(w, r, integration, binding, exists, parts, meta) {
		return
	}
	if len(parts) == 3 {
		switch r.Method {
		case http.MethodPut:
			if strings.TrimSpace(r.Header.Get("Idempotency-Key")) == "" {
				http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
			defer r.Body.Close()
			var request struct {
				Credential json.RawMessage `json:"credential"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if decoder.Decode(&request) != nil {
				http.Error(w, "invalid provisioning request", http.StatusBadRequest)
				return
			}
			result, err := a.thirdParty.Provision(r.Context(), integration, externalUserID, request.Credential, meta)
			status := http.StatusCreated
			if exists {
				status = http.StatusOK
			}
			writeThirdPartyResult(w, result, status, err)
		case http.MethodGet:
			if !exists {
				http.NotFound(w, r)
				return
			}
			write(w, binding, http.StatusOK)
		case http.MethodDelete:
			if !exists {
				http.NotFound(w, r)
				return
			}
			conversationIDs := make([]string, 0)
			for _, conversation := range a.control.Conversations() {
				if conversation.IntegrationUserID == binding.ID {
					a.chat.Close(conversation.ID)
					conversationIDs = append(conversationIDs, conversation.ID)
				}
			}
			result, err := a.thirdParty.Suspend(r.Context(), integration, binding, meta)
			if err == nil {
				for _, conversationID := range conversationIDs {
					if removeErr := a.chat.Remove(conversationID); removeErr != nil {
						writeThirdPartyResult(w, nil, 0, removeErr)
						return
					}
				}
			}
			writeThirdPartyResult(w, result, http.StatusAccepted, err)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) == 4 && parts[3] == "credential" && r.Method == http.MethodPut {
		if !exists {
			http.NotFound(w, r)
			return
		}
		if binding.Status != "ready" {
			http.Error(w, "integration user is not ready", http.StatusConflict)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, integration.Credential.MaxBytes+1024)
		defer r.Body.Close()
		var request struct {
			Credential json.RawMessage `json:"credential"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || len(request.Credential) == 0 {
			http.Error(w, "credential is required", http.StatusBadRequest)
			return
		}
		result, err := a.thirdParty.ReplaceCredential(r.Context(), integration, binding, request.Credential, meta)
		writeThirdPartyResult(w, result, http.StatusOK, err)
		return
	}
	if len(parts) == 5 && parts[3] == "usage" && parts[4] == "summary" && r.Method == http.MethodGet {
		if !exists {
			http.NotFound(w, r)
			return
		}
		if a.usage == nil {
			http.Error(w, usageledger.ErrUnavailable.Error(), http.StatusServiceUnavailable)
			return
		}
		from, to, err := usageRange(r, time.Now().UTC())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result, err := a.usage.Summary(r.Context(), integrationID, binding.ID, from, to)
		if err != nil {
			http.Error(w, "usage summary unavailable", http.StatusServiceUnavailable)
			return
		}
		write(w, result, http.StatusOK)
		return
	}
	if len(parts) == 5 && parts[3] == "usage" && parts[4] == "events" && r.Method == http.MethodGet {
		if !exists {
			http.NotFound(w, r)
			return
		}
		if a.usage == nil {
			http.Error(w, usageledger.ErrUnavailable.Error(), http.StatusServiceUnavailable)
			return
		}
		from, to, err := usageRange(r, time.Now().UTC())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		limit, err := usageListLimit(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result, err := a.usage.List(r.Context(), integrationID, binding.ID, from, to, limit, r.URL.Query().Get("cursor"))
		if errors.Is(err, usageledger.ErrInvalidCursor) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, "usage events unavailable", http.StatusServiceUnavailable)
			return
		}
		for index := range result.Items {
			project, ok := a.control.Project(result.Items[index].ProjectID)
			if ok && project.IntegrationID == integrationID && project.IntegrationUserID == binding.ID {
				result.Items[index].ProjectName = project.Name
			}
		}
		write(w, result, http.StatusOK)
		return
	}
	if len(parts) == 4 && parts[3] == "projects" {
		if !exists || binding.Status != "ready" {
			http.Error(w, "integration user is not ready", http.StatusConflict)
			return
		}
		switch r.Method {
		case http.MethodGet:
			projects := make([]control.Project, 0)
			for _, project := range a.control.Projects() {
				if project.IntegrationID == integrationID && project.IntegrationUserID == binding.ID && project.Status != "archived" {
					projects = append(projects, project)
				}
			}
			write(w, projects, http.StatusOK)
		case http.MethodPost:
			key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
			if key == "" || len(key) > 160 {
				http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
			defer r.Body.Close()
			var request struct {
				Name   string `json:"name"`
				Locale string `json:"locale,omitempty"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if decoder.Decode(&request) != nil {
				http.Error(w, "invalid project request", http.StatusBadRequest)
				return
			}
			projectID := stableAPIID("project", integrationID+"\x00"+binding.ID+"\x00"+key)
			_, projectExisted := a.control.Project(projectID)
			projectCtx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
			defer cancel()
			result, err := a.thirdParty.CreateProject(projectCtx, integration, binding, projectID, request.Name, request.Locale, meta)
			status := http.StatusCreated
			if projectExisted {
				status = http.StatusOK
			}
			writeThirdPartyResult(w, result, status, err)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) == 5 && parts[3] == "projects" && r.Method == http.MethodGet {
		if !exists {
			http.NotFound(w, r)
			return
		}
		project, ok := a.control.Project(parts[4])
		if !ok || project.IntegrationID != integrationID || project.IntegrationUserID != binding.ID {
			http.NotFound(w, r)
			return
		}
		write(w, project, http.StatusOK)
		return
	}
	if len(parts) == 7 && parts[3] == "projects" && parts[5] == "workspace" && (parts[6] == "tree" || parts[6] == "file") {
		a.handleIntegrationWorkspace(w, r, integration, binding, exists, parts[4], parts[6])
		return
	}
	if len(parts) == 6 && parts[3] == "projects" && parts[5] == "apps" && r.Method == http.MethodGet {
		if !exists || binding.Status != "ready" {
			http.Error(w, "integration user is not ready", http.StatusConflict)
			return
		}
		discoveryCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		applications, err := a.thirdParty.ProjectApplications(discoveryCtx, integration, binding, parts[4])
		writeThirdPartyResult(w, applications, http.StatusOK, err)
		return
	}
	if len(parts) == 6 && parts[3] == "projects" && parts[5] == "preview-app" && r.Method == http.MethodPut {
		if !exists || binding.Status != "ready" {
			http.Error(w, "integration user is not ready", http.StatusConflict)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		defer r.Body.Close()
		var request struct {
			AppPath string `json:"appPath"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || strings.TrimSpace(request.AppPath) == "" {
			http.Error(w, "appPath is required", http.StatusBadRequest)
			return
		}
		selectionCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		project, err := a.thirdParty.SelectProjectApplication(selectionCtx, integration, binding, parts[4], request.AppPath, meta)
		writeThirdPartyResult(w, project, http.StatusOK, err)
		return
	}
	if len(parts) == 4 && parts[3] == "conversations" && r.Method == http.MethodGet {
		if !exists || binding.Status != "ready" {
			http.Error(w, "integration user is not ready", http.StatusConflict)
			return
		}
		write(w, a.integrationConversationViews(activeIntegrationUserConversations(a.control.Conversations(), binding.ID)), http.StatusOK)
		return
	}
	if len(parts) == 4 && parts[3] == "conversations" && r.Method == http.MethodPost {
		if !a.chat.Enabled() {
			http.Error(w, "chat gateway unavailable", http.StatusServiceUnavailable)
			return
		}
		if !exists || binding.Status != "ready" {
			http.Error(w, "integration user is not ready", http.StatusConflict)
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" || len(key) > 160 {
			http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		defer r.Body.Close()
		var request struct {
			ProjectID string `json:"projectId"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || strings.TrimSpace(request.ProjectID) == "" {
			http.Error(w, "projectId is required", http.StatusBadRequest)
			return
		}
		project, ok := a.control.Project(request.ProjectID)
		if !ok || project.Status != "ready" || project.IntegrationID != integrationID || project.IntegrationUserID != binding.ID || project.OwnerUserID != binding.OwnerUserID {
			http.Error(w, "project is not ready", http.StatusConflict)
			return
		}
		conversationID := stableAPIID("conv", integrationID+"\x00"+binding.ID+"\x00"+key)
		if existing, ok := a.control.Conversation(conversationID); ok {
			if existing.IntegrationID != integrationID || existing.IntegrationUserID != binding.ID || existing.ProjectID != project.ID {
				http.Error(w, "idempotency conflict", http.StatusConflict)
				return
			}
			_ = a.chat.Ensure(existing)
			write(w, a.integrationConversationView(existing), http.StatusOK)
			return
		}
		deviceID := "executor-" + binding.OwnerUserID
		device, ok := a.control.Device(deviceID)
		if !ok || device.OwnerUserID != binding.OwnerUserID || device.Kind != "docker" {
			http.Error(w, "executor device is not ready", http.StatusConflict)
			return
		}
		activeConversations := 0
		for _, candidate := range a.control.Conversations() {
			if candidate.IntegrationUserID == binding.ID && candidate.Status != "closed" && candidate.Status != "deleted" {
				activeConversations++
			}
		}
		if activeConversations >= integration.MaxConversationsPerUser {
			http.Error(w, "conversation quota exceeded", http.StatusTooManyRequests)
			return
		}
		sessionID := stableAPIID("chat", conversationID)
		session, err := a.control.PutSession(r.Context(), control.Session{ID: sessionID, OwnerUserID: binding.OwnerUserID, DeviceID: deviceID, ProjectID: project.ID, Name: project.Name + " · " + integration.DisplayName + " AI Chat", ExecutionTarget: "docker", AgentMode: "chat", AccessMode: "private", TransportMode: "relay", Status: "starting"}, 0, meta)
		if errors.Is(err, control.ErrConflict) {
			var found bool
			session, found = a.control.Session(sessionID)
			if !found || session.OwnerUserID != binding.OwnerUserID || session.DeviceID != deviceID || session.ProjectID != project.ID || session.ExecutionTarget != "docker" || session.AgentMode != "chat" {
				http.Error(w, "conversation session idempotency conflict", http.StatusConflict)
				return
			}
			err = nil
		}
		if err != nil {
			writeThirdPartyResult(w, nil, 0, err)
			return
		}
		conversation, err := a.control.PutConversation(r.Context(), control.Conversation{ID: conversationID, IntegrationID: integrationID, IntegrationUserID: binding.ID, OwnerUserID: binding.OwnerUserID, DeviceID: deviceID, ProjectID: project.ID, SessionID: session.ID, Status: "connecting"}, 0, meta)
		if errors.Is(err, control.ErrConflict) {
			var found bool
			conversation, found = a.control.Conversation(conversationID)
			if !found || conversation.IntegrationID != integrationID || conversation.IntegrationUserID != binding.ID || conversation.OwnerUserID != binding.OwnerUserID || conversation.ProjectID != project.ID {
				http.Error(w, "conversation idempotency conflict", http.StatusConflict)
				return
			}
			err = nil
		}
		if err != nil {
			// Conversation creation is the ownership/quota commit point.  If it
			// fails, do not leave a restartable orphan session behind.
			_ = a.m.StopSession(r.Context(), binding.OwnerUserID, session.ID)
			session.Status, session.LastError = "closed", err.Error()
			_, _ = a.control.PutSession(r.Context(), session, session.Version, meta)
			writeThirdPartyResult(w, nil, 0, err)
			return
		}
		if err := a.chat.Ensure(conversation); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		write(w, a.integrationConversationView(conversation), http.StatusCreated)
		return
	}
	http.NotFound(w, r)
}

const maxWorkspaceFileBytes = 2 << 20

type integrationWorkspaceResult struct {
	Type      string          `json:"type"`
	RequestID string          `json:"requestId"`
	Operation string          `json:"operation"`
	OK        bool            `json:"ok"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     struct {
		Code            string `json:"code"`
		Message         string `json:"message"`
		CurrentRevision string `json:"currentRevision,omitempty"`
	} `json:"error,omitempty"`
}

func (a api) handleIntegrationWorkspace(w http.ResponseWriter, r *http.Request, integration control.Integration, binding control.IntegrationUser, bindingExists bool, projectID, resource string) {
	if !bindingExists || binding.Status != "ready" {
		http.Error(w, "integration user is not ready", http.StatusConflict)
		return
	}
	project, ok := a.control.Project(projectID)
	if !ok || project.Status != "ready" || project.IntegrationID != integration.ID || project.IntegrationUserID != binding.ID || project.OwnerUserID != binding.OwnerUserID || project.WorkingDir != control.ProjectWorkingDir(project.ID) {
		http.NotFound(w, r)
		return
	}
	requestID := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if requestID == "" || len(requestID) > 160 {
		http.Error(w, "a valid Idempotency-Key is required", http.StatusBadRequest)
		return
	}

	operation := "list"
	requestPath := r.URL.Query().Get("path")
	conversationID := r.URL.Query().Get("conversationId")
	message := map[string]any{}
	if resource == "tree" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	} else {
		operation = "read"
		switch r.Method {
		case http.MethodGet:
		case http.MethodPut:
			r.Body = http.MaxBytesReader(w, r.Body, maxWorkspaceFileBytes+16<<10)
			defer r.Body.Close()
			var request struct {
				ConversationID string `json:"conversationId"`
				Path           string `json:"path"`
				Content        string `json:"content"`
				BaseRevision   string `json:"baseRevision"`
				Create         bool   `json:"create,omitempty"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if decoder.Decode(&request) != nil || len(request.Content) > maxWorkspaceFileBytes {
				http.Error(w, "invalid workspace write request", http.StatusBadRequest)
				return
			}
			conversationID, requestPath, operation = request.ConversationID, request.Path, "write"
			message["content"], message["baseRevision"], message["create"] = request.Content, request.BaseRevision, request.Create
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	}
	if len(requestPath) > 4096 || strings.ContainsAny(requestPath, "\\\x00\r\n") || (operation != "list" && strings.TrimSpace(requestPath) == "") {
		http.Error(w, "invalid workspace path", http.StatusBadRequest)
		return
	}
	conversation, ok := a.control.Conversation(conversationID)
	if !ok || conversation.IntegrationID != integration.ID || conversation.IntegrationUserID != binding.ID || conversation.OwnerUserID != binding.OwnerUserID || conversation.ProjectID != project.ID || conversation.Status == "closed" || conversation.Status == "deleted" {
		http.Error(w, "a ready conversation for this project is required", http.StatusConflict)
		return
	}
	message["type"], message["operation"], message["requestId"] = "workspace", operation, requestID
	message["projectPath"], message["path"] = project.WorkingDir, requestPath

	requestCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	event, _, err := a.chat.Request(requestCtx, conversation, requestID, message, "workspace_result")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			http.Error(w, "workspace executor response timed out", http.StatusGatewayTimeout)
			return
		}
		writeChatError(w, err)
		return
	}
	var result integrationWorkspaceResult
	if json.Unmarshal(event.Data, &result) != nil || result.Type != "workspace_result" || result.RequestID != requestID || result.Operation != operation {
		http.Error(w, "invalid workspace executor response", http.StatusBadGateway)
		return
	}
	if !result.OK {
		writeWorkspaceError(w, result)
		return
	}
	if len(result.Data) == 0 || len(result.Data) > maxWorkspaceFileBytes+1<<20 {
		http.Error(w, "invalid workspace executor payload", http.StatusBadGateway)
		return
	}
	write(w, result.Data, http.StatusOK)
}

func writeWorkspaceError(w http.ResponseWriter, result integrationWorkspaceResult) {
	message := strings.TrimSpace(result.Error.Message)
	if message == "" || len(message) > 500 {
		message = "workspace request failed"
	}
	status := http.StatusBadGateway
	switch result.Error.Code {
	case "invalid_path", "invalid_request", "invalid_revision", "not_file", "not_directory", "unsupported_operation":
		status = http.StatusBadRequest
	case "forbidden":
		status = http.StatusForbidden
	case "not_found":
		status = http.StatusNotFound
	case "conflict":
		status = http.StatusConflict
	case "too_large", "too_many_entries":
		status = http.StatusRequestEntityTooLarge
	case "binary":
		status = http.StatusUnsupportedMediaType
	}
	write(w, map[string]string{"error": message, "code": result.Error.Code, "currentRevision": result.Error.CurrentRevision}, status)
}

func usageRange(r *http.Request, now time.Time) (time.Time, time.Time, error) {
	to := now.UTC()
	from := to.AddDate(0, 0, -30)
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days < 1 || days > 366 {
			return time.Time{}, time.Time{}, errors.New("days must be between 1 and 366")
		}
		from = to.AddDate(0, 0, -days)
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("to must be RFC3339")
		}
		to = parsed.UTC()
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("from must be RFC3339")
		}
		from = parsed.UTC()
	}
	if !to.After(from) || to.Sub(from) > 366*24*time.Hour {
		return time.Time{}, time.Time{}, errors.New("usage range must be positive and at most 366 days")
	}
	return from, to, nil
}

func usageListLimit(r *http.Request) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return 30, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 100 {
		return 0, errors.New("limit must be between 1 and 100")
	}
	return limit, nil
}

func activeIntegrationUserConversations(values []control.Conversation, bindingID string) []control.Conversation {
	result := make([]control.Conversation, 0)
	for _, conversation := range values {
		if conversation.IntegrationUserID == bindingID && conversation.Status != "closed" && conversation.Status != "deleted" {
			result = append(result, conversation)
		}
	}
	return result
}

// integrationConversationConnection is a safe, user-facing projection of the
// control-plane records that together make one chat connection. Keeping this
// separate from control.Conversation avoids persisting derived state and gives
// browser clients one authoritative snapshot after a missed SSE event.
type integrationConversationConnection struct {
	RelayAvailable  bool      `json:"relayAvailable"`
	RuntimeRunning  bool      `json:"runtimeRunning"`
	RuntimeHealthy  bool      `json:"runtimeHealthy"`
	ClientConnected bool      `json:"clientConnected"`
	RelayRegistered bool      `json:"relayRegistered"`
	SessionStatus   string    `json:"sessionStatus,omitempty"`
	Reason          string    `json:"reason"`
	LastError       string    `json:"lastError,omitempty"`
	LastHeartbeat   time.Time `json:"lastHeartbeat,omitempty"`
}

type integrationConversationView struct {
	control.Conversation
	Connection integrationConversationConnection `json:"connection"`
}

func (a api) integrationConversationViews(values []control.Conversation) []integrationConversationView {
	result := make([]integrationConversationView, 0, len(values))
	for _, value := range values {
		result = append(result, a.integrationConversationView(value))
	}
	return result
}

func (a api) integrationConversationView(conversation control.Conversation) integrationConversationView {
	connection := integrationConversationConnection{Reason: "connecting"}
	device, deviceExists := a.control.Device(conversation.DeviceID)
	if deviceExists && device.OwnerUserID == conversation.OwnerUserID {
		connection.RuntimeRunning = device.RuntimeRunning
		connection.RuntimeHealthy = device.RuntimeHealthy
		connection.ClientConnected = device.ClientConnected
		connection.RelayRegistered = device.RelayRegistered
		connection.LastHeartbeat = device.LastHeartbeat
	}

	session, sessionExists := a.control.Session(conversation.SessionID)
	if sessionExists && session.OwnerUserID == conversation.OwnerUserID && session.DeviceID == conversation.DeviceID {
		connection.SessionStatus = session.Status
		connection.LastError = strings.TrimSpace(session.LastError)
	}

	if deviceExists && device.RuntimeID != "" {
		if runtime, ok := a.control.Runtime(device.RuntimeID); ok && runtime.OwnerUserID == conversation.OwnerUserID && runtime.DeviceID == device.ID {
			connection.RuntimeRunning = runtime.ObservedState == "running" || runtime.ObservedState == "online"
			connection.RuntimeHealthy = runtime.Health == "healthy"
			if connection.LastError == "" {
				connection.LastError = strings.TrimSpace(runtime.LastError)
			}
		}
	}

	relayNodeID := ""
	if sessionExists {
		relayNodeID = session.RelayNodeID
	}
	if relayNodeID == "" && deviceExists {
		relayNodeID = device.RelayNodeID
	}
	if relayNodeID != "" {
		if node, ok := a.control.Node(relayNodeID); ok {
			connection.RelayAvailable = node.Kind == "relay" && node.Status == "ready" && !node.LastHeartbeat.IsZero() && node.LastHeartbeat.After(time.Now().UTC().Add(-90*time.Second))
		}
	}

	if strings.TrimSpace(conversation.LastError) != "" {
		connection.LastError = strings.TrimSpace(conversation.LastError)
	}
	connection.Reason = integrationConversationReason(conversation, connection)
	return integrationConversationView{Conversation: conversation, Connection: connection}
}

func integrationConversationReason(conversation control.Conversation, connection integrationConversationConnection) string {
	switch conversation.Status {
	case "deleted":
		return "deleted"
	case "closed":
		if strings.EqualFold(strings.TrimSpace(conversation.LastError), "idle timeout") || strings.EqualFold(strings.TrimSpace(connection.LastError), "idle timeout") {
			return "idle_timeout"
		}
		return "closed"
	}
	if !connection.RuntimeRunning {
		return "runtime_stopped"
	}
	if !connection.RuntimeHealthy {
		return "runtime_unhealthy"
	}
	if connection.SessionStatus == "starting" {
		return "session_starting"
	}
	if conversation.Status == "reconnecting" || connection.SessionStatus == "reconnecting" {
		return "client_reconnecting"
	}
	if !connection.ClientConnected {
		return "client_offline"
	}
	if !connection.RelayRegistered {
		return "relay_unregistered"
	}
	if !connection.RelayAvailable {
		return "relay_unavailable"
	}
	if conversation.Status == "ready" && (connection.SessionStatus == "ready" || connection.SessionStatus == "active") {
		return "connected"
	}
	return "connecting"
}

func (a api) handleIntegrationConversation(w http.ResponseWriter, r *http.Request, integration control.Integration, parts []string, meta control.MutationMeta) {
	if len(parts) < 1 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	conversation, ok := a.control.Conversation(parts[0])
	if !ok {
		http.NotFound(w, r)
		return
	}
	if conversation.IntegrationID != integration.ID {
		http.Error(w, "conversation ownership required", http.StatusForbidden)
		return
	}
	binding, ok := a.control.IntegrationUser(conversation.IntegrationUserID)
	if !ok || binding.IntegrationID != integration.ID || binding.OwnerUserID != conversation.OwnerUserID {
		http.Error(w, "conversation ownership is invalid", http.StatusForbidden)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			write(w, a.integrationConversationView(conversation), http.StatusOK)
		case http.MethodDelete:
			a.chat.Close(conversation.ID)
			_ = a.m.StopSession(r.Context(), conversation.OwnerUserID, conversation.SessionID)
			if err := closeIntegrationSession(r.Context(), a.control, conversation.SessionID, meta); err != nil {
				writeThirdPartyResult(w, conversation, http.StatusOK, err)
				return
			}
			updated, err := closeIntegrationConversation(r.Context(), a.control, conversation.ID, meta)
			if err == nil {
				err = a.chat.Remove(conversation.ID)
			}
			if err != nil {
				writeThirdPartyResult(w, updated, http.StatusOK, err)
			} else {
				write(w, a.integrationConversationView(updated), http.StatusOK)
			}
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "retry" && r.Method == http.MethodPost {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" || len(key) > 160 {
			http.Error(w, "a valid Idempotency-Key is required", http.StatusBadRequest)
			return
		}
		if conversation.Status == "deleted" {
			http.Error(w, "conversation is deleted", http.StatusConflict)
			return
		}
		updated, err := a.retryIntegrationConversation(r.Context(), conversation, meta)
		if err != nil {
			writeThirdPartyResult(w, updated, http.StatusAccepted, err)
		} else {
			write(w, a.integrationConversationView(updated), http.StatusAccepted)
		}
		return
	}
	if conversation.Status == "closed" || conversation.Status == "deleted" {
		http.Error(w, "conversation is closed", http.StatusConflict)
		return
	}
	switch {
	case len(parts) == 2 && parts[1] == "messages" && r.Method == http.MethodPost:
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, chatgateway.MaxChatRequestBytes)
		defer r.Body.Close()
		var request struct {
			Prompt string                        `json:"prompt"`
			Images []chatgateway.ImageAttachment `json:"images,omitempty"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil {
			http.Error(w, "invalid chat request", http.StatusBadRequest)
			return
		}
		event, duplicate, err := a.chat.SendChatWithImages(conversation, key, request.Prompt, request.Images)
		if err != nil && !errors.Is(err, chatgateway.ErrDuplicateRequest) {
			writeChatError(w, err)
			return
		}
		if duplicate {
			write(w, map[string]any{"conversationId": conversation.ID, "duplicate": true}, http.StatusOK)
			return
		}
		write(w, publicChatEvent(event), http.StatusAccepted)
	case len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet:
		a.integrationConversationEvents(w, r, conversation)
	case len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost:
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
			return
		}
		event, duplicate, err := a.chat.Send(conversation, key, map[string]string{"type": "abort"})
		if err != nil {
			writeChatError(w, err)
			return
		}
		write(w, map[string]any{"event": event, "duplicate": duplicate}, http.StatusAccepted)
	case len(parts) == 3 && parts[1] == "permissions" && r.Method == http.MethodPost:
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		defer r.Body.Close()
		var request struct {
			Allow        bool `json:"allow"`
			UpdatedInput any  `json:"updatedInput,omitempty"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil {
			http.Error(w, "invalid permission response", http.StatusBadRequest)
			return
		}
		message := map[string]any{"type": "permission_response", "requestId": parts[2], "allow": request.Allow}
		if request.UpdatedInput != nil {
			message["updatedInput"] = request.UpdatedInput
		}
		event, duplicate, err := a.chat.Send(conversation, key, message)
		if err != nil {
			writeChatError(w, err)
			return
		}
		write(w, map[string]any{"event": event, "duplicate": duplicate}, http.StatusAccepted)
	default:
		http.NotFound(w, r)
	}
}

// Closing a conversation races legitimately with the Relay presence heartbeat:
// both mutate the same versioned Session. A single best-effort Put can therefore
// leave a closed conversation with an active Session forever, preventing idle
// Executor cleanup. Reload and retry both records so DELETE is idempotent and
// converges even while the host WebSocket is disconnecting.
func closeIntegrationSession(ctx context.Context, service *control.Service, sessionID string, meta control.MutationMeta) error {
	for range 8 {
		session, ok := service.Session(sessionID)
		if !ok || session.Status == "closed" {
			return nil
		}
		session.Status, session.LastError, session.HostConnectionID = "closed", "", ""
		session.DriverUserID, session.DriverLeaseExpiresAt = "", nil
		if _, err := service.PutSession(ctx, session, session.Version, meta); err == nil {
			return nil
		} else if !errors.Is(err, control.ErrConflict) {
			return err
		}
	}
	return control.ErrConflict
}

func closeIntegrationConversation(ctx context.Context, service *control.Service, conversationID string, meta control.MutationMeta) (control.Conversation, error) {
	for range 8 {
		conversation, ok := service.Conversation(conversationID)
		if !ok {
			return control.Conversation{}, control.ErrNotFound
		}
		if conversation.Status == "closed" {
			return conversation, nil
		}
		conversation.Status, conversation.LastError = "closed", ""
		updated, err := service.PutConversation(ctx, conversation, conversation.Version, meta)
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, control.ErrConflict) {
			return control.Conversation{}, err
		}
	}
	return control.Conversation{}, control.ErrConflict
}

func (a api) retryIntegrationConversation(ctx context.Context, conversation control.Conversation, meta control.MutationMeta) (control.Conversation, error) {
	session, ok := a.control.Session(conversation.SessionID)
	if !ok {
		return control.Conversation{}, control.ErrNotFound
	}
	if session.OwnerUserID != conversation.OwnerUserID || session.DeviceID != conversation.DeviceID || session.ProjectID != conversation.ProjectID || session.ExecutionTarget != "docker" {
		return control.Conversation{}, control.ErrForbidden
	}
	if conversation.Status == "ready" && (session.Status == "ready" || session.Status == "active") {
		if err := a.chat.Ensure(conversation); err != nil {
			return control.Conversation{}, err
		}
		return conversation, nil
	}

	// Closing only the live Gateway peer is safe: the journal and conversation
	// identity remain intact, while a stale WebSocket can no longer race the
	// replacement connection.
	a.chat.Close(conversation.ID)
	if session.Status != "starting" && session.Status != "reconnecting" {
		if err := a.m.StopSession(ctx, session.OwnerUserID, session.ID); err != nil && !errors.Is(err, manager.ErrExecutorNotFound) {
			executor, exists := a.m.Executor(session.OwnerUserID)
			if !exists || executor.Status != "stopped" {
				return control.Conversation{}, err
			}
		}
		var err error
		session, err = resetConversationSession(ctx, a.control, session.ID, meta)
		if err != nil {
			return control.Conversation{}, err
		}
	}

	updated, err := resetConversationState(ctx, a.control, conversation.ID, meta)
	if err != nil {
		return control.Conversation{}, err
	}
	if err := a.chat.Ensure(updated); err != nil {
		return control.Conversation{}, err
	}
	return updated, nil
}

func resetConversationSession(ctx context.Context, service *control.Service, sessionID string, meta control.MutationMeta) (control.Session, error) {
	for range 8 {
		session, ok := service.Session(sessionID)
		if !ok {
			return control.Session{}, control.ErrNotFound
		}
		session.Status, session.LastError, session.HostConnectionID = "starting", "", ""
		session.DriverUserID, session.DriverLeaseExpiresAt = "", nil
		session.StartAttempts = 0
		updated, err := service.PutSession(ctx, session, session.Version, meta)
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, control.ErrConflict) {
			return control.Session{}, err
		}
	}
	return control.Session{}, control.ErrConflict
}

func resetConversationState(ctx context.Context, service *control.Service, conversationID string, meta control.MutationMeta) (control.Conversation, error) {
	for range 8 {
		conversation, ok := service.Conversation(conversationID)
		if !ok {
			return control.Conversation{}, control.ErrNotFound
		}
		conversation.Status, conversation.LastError = "connecting", ""
		conversation.LastActivityAt = time.Now().UTC()
		updated, err := service.PutConversation(ctx, conversation, conversation.Version, meta)
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, control.ErrConflict) {
			return control.Conversation{}, err
		}
	}
	return control.Conversation{}, control.ErrConflict
}

func (a api) integrationConversationEvents(w http.ResponseWriter, r *http.Request, conversation control.Conversation) {
	after := uint64(0)
	if value := r.Header.Get("Last-Event-ID"); value != "" {
		after, _ = strconv.ParseUint(value, 10, 64)
	}
	if value := r.URL.Query().Get("after"); value != "" {
		if parsed, err := strconv.ParseUint(value, 10, 64); err == nil && parsed > after {
			after = parsed
		}
	}
	// A bounded JSON read is useful for server-to-server polling and E2E
	// verification. SSE remains the default and recommended live transport.
	if r.URL.Query().Get("stream") == "false" {
		values, err := a.chat.Events(r.Context(), conversation.ID, after, positiveQueryInt(r, "limit", 200, 1000))
		if err != nil {
			writeChatError(w, err)
			return
		}
		write(w, publicChatEvents(values), http.StatusOK)
		return
	}
	if !acquire(w, a.eventSlots) {
		return
	}
	defer func() { <-a.eventSlots }()
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	changes, unsubscribe := a.chat.Subscribe(conversation.ID)
	defer unsubscribe()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	deadline := time.NewTimer(a.eventLifetime)
	defer deadline.Stop()
	send := func() error {
		return sendIntegrationEventBacklog(r.Context(), a.chat, conversation.ID, &after, w, flusher)
	}
	if err := send(); err != nil {
		return
	}
	if err := sendIntegrationReplayComplete(w, flusher, after); err != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, "event: heartbeat\ndata: {\"at\":%q}\n\n", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return
			}
			flusher.Flush()
		case <-changes:
			if err := send(); err != nil {
				return
			}
		}
	}
}

const integrationEventReplayBatch = 200

// A conversation can contain far more events than one replay page because
// thinking, tool and text deltas are journaled separately.  Draining only one
// page makes an idle SSE connection wait forever with unread history; users
// then appear to recover one page per manual reconnect.  Keep reading until the
// durable journal is caught up, while flushing each bounded page so live UI
// rendering can begin immediately.
func sendIntegrationEventBacklog(ctx context.Context, chat *chatgateway.Gateway, conversationID string, after *uint64, w http.ResponseWriter, flusher http.Flusher) error {
	for {
		values, err := chat.Events(ctx, conversationID, *after, integrationEventReplayBatch)
		if err != nil {
			return err
		}
		for _, event := range publicChatEvents(values) {
			data, _ := json.Marshal(event)
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Sequence, data); err != nil {
				return err
			}
			*after = event.Sequence
		}
		if len(values) > 0 {
			flusher.Flush()
		}
		if len(values) < integrationEventReplayBatch {
			return nil
		}
	}
}

func sendIntegrationReplayComplete(w io.Writer, flusher http.Flusher, lastSequence uint64) error {
	if _, err := fmt.Fprintf(w, "event: replay_complete\ndata: {\"lastSequence\":%d}\n\n", lastSequence); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func publicChatEvents(values []chatgateway.Event) []chatgateway.Event {
	public := make([]chatgateway.Event, len(values))
	for index, event := range values {
		public[index] = publicChatEvent(event)
	}
	return public
}

func publicChatEvent(event chatgateway.Event) chatgateway.Event {
	if event.Type == "control.accepted" && len(event.Data) > 0 {
		var request struct {
			Type      string `json:"type"`
			Operation string `json:"operation"`
			Path      string `json:"path"`
		}
		if json.Unmarshal(event.Data, &request) == nil && request.Type == "workspace" {
			event.Data, _ = json.Marshal(map[string]string{"type": request.Type, "operation": request.Operation, "path": request.Path})
		}
		return event
	}
	if event.Type == "workspace_result" && len(event.Data) > 0 {
		var result integrationWorkspaceResult
		if json.Unmarshal(event.Data, &result) == nil {
			event.Data, _ = json.Marshal(map[string]any{"type": result.Type, "operation": result.Operation, "ok": result.OK})
		}
		return event
	}
	if event.Type != "request.accepted" || len(event.Data) == 0 {
		return event
	}
	var request struct {
		Type   string                        `json:"type"`
		Prompt string                        `json:"prompt"`
		Images []chatgateway.ImageAttachment `json:"images,omitempty"`
	}
	if json.Unmarshal(event.Data, &request) != nil {
		event.Data = nil
		return event
	}
	attachments := make([]map[string]any, 0, len(request.Images))
	for _, image := range request.Images {
		attachments = append(attachments, map[string]any{"name": image.Name, "mimeType": image.MIMEType, "size": image.Size})
	}
	data, _ := json.Marshal(map[string]any{"type": request.Type, "prompt": request.Prompt, "attachments": attachments})
	event.Data = data
	return event
}

func stableAPIID(prefix, value string) string {
	sum := sha256.Sum256([]byte(value))
	return prefix + "-" + hex.EncodeToString(sum[:16])
}

func writeChatError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, chatgateway.ErrBackpressure), errors.Is(err, chatgateway.ErrJournalFull):
		w.Header().Set("Retry-After", "1")
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, chatgateway.ErrTurnActive):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func writeThirdPartyResult(w http.ResponseWriter, value any, status int, err error) {
	if err == nil {
		write(w, value, status)
		return
	}
	switch {
	case errors.Is(err, control.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, control.ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, control.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, control.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, control.ErrQuota):
		w.Header().Set("Retry-After", "1")
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, manager.ErrExecutorCapacity):
		w.Header().Set("Retry-After", "5")
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, manager.ErrExecutorDiskQuota):
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
	case errors.Is(err, manager.ErrExecutorDiskCapacity):
		w.Header().Set("Retry-After", "60")
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, thirdparty.ErrUnauthorized):
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	default:
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
}
