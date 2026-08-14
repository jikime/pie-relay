package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/auth"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/claudeauth"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

type claudeAuthMutation struct {
	Label   string `json:"label"`
	Restart *bool  `json:"restart"`
}

type claudeAuthMutationResult struct {
	Version   claudeauth.Version `json:"version"`
	Rollout   claudeauth.Rollout `json:"rollout"`
	Status    claudeauth.Status  `json:"status"`
	Duplicate bool               `json:"duplicate,omitempty"`
}

func (a api) handleAdminClaudeAuth(w http.ResponseWriter, r *http.Request, principal auth.Principal, path string) bool {
	if path != "claude-auth" && !strings.HasPrefix(path, "claude-auth/") {
		return false
	}
	if path == "claude-auth" && r.Method == http.MethodGet {
		if a.claudeAuth == nil {
			http.Error(w, "Claude authentication service unavailable", http.StatusServiceUnavailable)
			return true
		}
		status, err := a.claudeAuth.Status(a.executorUserIDs())
		if err != nil {
			http.Error(w, "Claude authentication status unavailable", http.StatusInternalServerError)
			return true
		}
		write(w, status, http.StatusOK)
		return true
	}
	if !principal.CanAdminister() {
		http.Error(w, "administrator role required", http.StatusForbidden)
		return true
	}
	if a.claudeAuth == nil {
		http.Error(w, "Claude authentication service unavailable", http.StatusServiceUnavailable)
		return true
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	var request claudeAuthMutation
	if !decodeControlJSON(w, r, &request) {
		return true
	}
	restart := request.Restart == nil || *request.Restart
	action := strings.TrimPrefix(path, "claude-auth/")
	if action != "publish" && action != "deploy" && action != "rollback" {
		http.NotFound(w, r)
		return true
	}
	actor := strings.TrimSpace(principal.UserID)
	if actor == "" {
		actor = "admin"
	}
	operation, duplicate, err := a.control.BeginOperation(r.Context(), control.Operation{
		ActorUserID: actor,
		Type:        "claude-auth." + action,
		TargetKind:  "claude-auth",
		TargetID:    "global",
		Request: map[string]any{
			"label":   strings.TrimSpace(request.Label),
			"restart": restart,
		},
		IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")),
	}, requestMeta(r, principal))
	if err != nil {
		writeControlError(w, err)
		return true
	}
	if duplicate {
		status, statusErr := a.claudeAuth.Status(a.executorUserIDs())
		if statusErr != nil {
			http.Error(w, "Claude authentication status unavailable", http.StatusInternalServerError)
			return true
		}
		result := claudeAuthMutationResult{Status: status, Duplicate: true}
		if status.Active != nil {
			result.Version = *status.Active
		}
		write(w, result, http.StatusOK)
		return true
	}
	_, _ = a.control.UpdateOperation(r.Context(), operation.ID, "running", 10, nil, "", requestMeta(r, principal))

	var version claudeauth.Version
	switch action {
	case "publish":
		version, err = a.claudeAuth.PublishFromLogin(r.Context(), request.Label)
	case "rollback":
		version, err = a.claudeAuth.Rollback(r.Context())
	case "deploy":
		status, statusErr := a.claudeAuth.Status(nil)
		if statusErr != nil {
			err = statusErr
		} else if status.Active == nil {
			err = claudeauth.ErrNotConfigured
		} else {
			version = *status.Active
		}
	}
	if err != nil {
		_, _ = a.control.UpdateOperation(context.Background(), operation.ID, "failed", 100, nil, "Claude authentication action failed", requestMeta(r, principal))
		http.Error(w, "Claude authentication action failed", http.StatusBadRequest)
		return true
	}

	rollout, rolloutErr := a.rolloutClaudeAuth(r.Context(), version, restart)
	status, statusErr := a.claudeAuth.Status(a.executorUserIDs())
	if statusErr != nil {
		rolloutErr = errors.Join(rolloutErr, statusErr)
	}
	result := claudeAuthMutationResult{Version: version, Rollout: rollout, Status: status}
	operationResult := map[string]any{
		"version":   version.ID,
		"targets":   rollout.Total,
		"succeeded": rollout.Succeeded,
		"failed":    rollout.Failed,
	}
	if rolloutErr != nil {
		_, _ = a.control.UpdateOperation(context.Background(), operation.ID, "failed", 100, operationResult, "Claude authentication rollout was incomplete", requestMeta(r, principal))
		write(w, result, http.StatusMultiStatus)
		return true
	}
	_, _ = a.control.UpdateOperation(context.Background(), operation.ID, "succeeded", 100, operationResult, "", requestMeta(r, principal))
	write(w, result, http.StatusOK)
	return true
}

func (a api) executorUserIDs() []string {
	executors := a.m.Executors()
	userIDs := make([]string, 0, len(executors))
	for _, executor := range executors {
		userIDs = append(userIDs, executor.UserID)
	}
	return userIDs
}

func (a api) rolloutClaudeAuth(ctx context.Context, version claudeauth.Version, restart bool) (claudeauth.Rollout, error) {
	executors := a.m.Executors()
	userIDs := make([]string, 0, len(executors))
	byUser := make(map[string]manager.Executor, len(executors))
	for _, executor := range executors {
		userIDs = append(userIDs, executor.UserID)
		byUser[executor.UserID] = executor
	}
	rollout, deployErr := a.claudeAuth.Rollout(ctx, userIDs, a.claudeAuthWorkers)
	if !restart {
		return rollout, deployErr
	}
	workers := a.claudeAuthWorkers
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for index := range rollout.Items {
		if rollout.Items[index].Status == "failed" || byUser[rollout.Items[index].UserID].Status != "ready" {
			continue
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				rollout.Items[index].Status = "failed"
				rollout.Items[index].LastError = "verification canceled"
				return
			}
			userID := rollout.Items[index].UserID
			err := a.m.RestartForClaudeAuth(ctx, userID)
			if err != nil {
				rollout.Items[index].Status = "failed"
				rollout.Items[index].LastError = "executor restart failed"
				return
			}
			// The setup-token is not installed in the container, so `claude auth
			// status` outside a chat child is intentionally meaningless. A fresh
			// controller-started chat session performs the real authentication.
			rollout.Items[index].Status = "available"
		}(index)
	}
	wg.Wait()
	rollout.Succeeded, rollout.Failed = 0, 0
	for _, item := range rollout.Items {
		if item.Status == "failed" {
			rollout.Failed++
		} else {
			rollout.Succeeded++
		}
	}
	if rollout.Failed > 0 {
		return rollout, errors.Join(deployErr, errors.New("one or more Claude authentication targets failed"))
	}
	return rollout, deployErr
}
