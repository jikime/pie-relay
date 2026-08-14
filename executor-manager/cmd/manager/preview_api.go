package main

import (
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	previewservice "github.com/pielab-ai/pie-relay/executor-manager/internal/preview"
)

func (a api) handleInternalPreviewRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || a.previews == nil || a.previewGatewayToken == "" {
		http.NotFound(w, r)
		return
	}
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(provided) != len(a.previewGatewayToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(a.previewGatewayToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	hostname := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("host")))
	if host, _, err := net.SplitHostPort(hostname); err == nil {
		hostname = host
	}
	route, ok := a.previews.Route(hostname)
	if !ok {
		http.NotFound(w, r)
		return
	}
	write(w, route, http.StatusOK)
}

func (a api) handleIntegrationPreviews(w http.ResponseWriter, r *http.Request, integration control.Integration, binding control.IntegrationUser, bindingExists bool, parts []string, meta control.MutationMeta) bool {
	if len(parts) < 6 || parts[3] != "projects" || parts[5] != "previews" {
		return false
	}
	if a.previews == nil {
		http.Error(w, "preview service unavailable", http.StatusServiceUnavailable)
		return true
	}
	if !bindingExists || binding.Status != "ready" {
		http.Error(w, "integration user is not ready", http.StatusConflict)
		return true
	}
	project, ok := a.control.Project(parts[4])
	if !ok || project.Status != "ready" || project.IntegrationID != integration.ID || project.IntegrationUserID != binding.ID || project.OwnerUserID != binding.OwnerUserID {
		http.Error(w, "project is not ready", http.StatusConflict)
		return true
	}
	if len(parts) == 6 {
		switch r.Method {
		case http.MethodGet:
			values := make([]control.Preview, 0, 20)
			for _, value := range a.control.PreviewsForProject(project.ID, 100) {
				if value.IntegrationUserID == binding.ID {
					values = append(values, value)
				}
			}
			write(w, values, http.StatusOK)
		case http.MethodPost:
			key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
			if key == "" || len(key) > 160 {
				http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
				return true
			}
			r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
			defer r.Body.Close()
			var request struct {
				AppPath    string `json:"appPath,omitempty"`
				Profile    string `json:"profile,omitempty"`
				Visibility string `json:"visibility,omitempty"`
				TTLSeconds int64  `json:"ttlSeconds,omitempty"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if decoder.Decode(&request) != nil || request.TTLSeconds < 0 {
				http.Error(w, "invalid preview request", http.StatusBadRequest)
				return true
			}
			previewID := stableAPIID("preview", integration.ID+"\x00"+binding.ID+"\x00"+project.ID+"\x00"+key)
			launch, duplicate, err := a.previews.Create(r.Context(), previewservice.CreateRequest{
				ID: previewID, Integration: integration, Binding: binding, Project: project,
				AppPath: request.AppPath, Profile: request.Profile, Visibility: request.Visibility, TTL: time.Duration(request.TTLSeconds) * time.Second, Meta: meta,
			})
			status := http.StatusAccepted
			if duplicate {
				status = http.StatusOK
			}
			writeThirdPartyResult(w, launch, status, err)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return true
	}
	previewID := parts[6]
	value, ok := a.control.Preview(previewID)
	if !ok || value.ProjectID != project.ID || value.IntegrationID != integration.ID || value.IntegrationUserID != binding.ID || value.OwnerUserID != binding.OwnerUserID {
		http.NotFound(w, r)
		return true
	}
	if len(parts) == 7 {
		switch r.Method {
		case http.MethodGet:
			write(w, value, http.StatusOK)
		case http.MethodDelete:
			result, err := a.previews.Stop(r.Context(), value, meta)
			writeThirdPartyResult(w, result, http.StatusOK, err)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return true
	}
	if len(parts) == 8 && parts[7] == "access" && r.Method == http.MethodPost {
		launch, err := a.previews.Launch(value)
		writeThirdPartyResult(w, launch, http.StatusOK, err)
		return true
	}
	if len(parts) == 8 && parts[7] == "stop" && r.Method == http.MethodPost {
		result, err := a.previews.Stop(r.Context(), value, meta)
		writeThirdPartyResult(w, result, http.StatusOK, err)
		return true
	}
	if len(parts) == 8 && parts[7] == "record" && r.Method == http.MethodDelete {
		result, err := a.previews.Delete(r.Context(), value, meta)
		writeThirdPartyResult(w, result, http.StatusOK, err)
		return true
	}
	if len(parts) == 8 && parts[7] == "visibility" && r.Method == http.MethodPut {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		defer r.Body.Close()
		var request struct {
			Visibility string `json:"visibility"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil {
			http.Error(w, "invalid preview visibility request", http.StatusBadRequest)
			return true
		}
		launch, err := a.previews.SetVisibility(r.Context(), value, request.Visibility, meta)
		writeThirdPartyResult(w, launch, http.StatusOK, err)
		return true
	}
	if len(parts) == 8 && parts[7] == "restart" && r.Method == http.MethodPost {
		launch, err := a.previews.Restart(r.Context(), value, meta)
		writeThirdPartyResult(w, launch, http.StatusAccepted, err)
		return true
	}
	if len(parts) == 8 && parts[7] == "logs" && r.Method == http.MethodGet {
		limit := 1 << 20
		if raw := r.URL.Query().Get("tailBytes"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 1<<20 {
				http.Error(w, "invalid tailBytes", http.StatusBadRequest)
				return true
			}
			limit = parsed
		}
		logs, err := a.previews.Logs(r.Context(), value)
		if err != nil {
			writeThirdPartyResult(w, nil, 0, err)
			return true
		}
		if len(logs) > limit {
			logs = logs[len(logs)-limit:]
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(logs)
		return true
	}
	http.NotFound(w, r)
	return true
}
