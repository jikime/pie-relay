package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

const maxLifecycleWebhookBody = 64 << 10

type lifecycleEvent struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurredAt"`
	Provision  bool      `json:"provision,omitempty"`
	User       struct {
		ID              string                 `json:"id"`
		ExternalSubject string                 `json:"externalSubject,omitempty"`
		OrganizationID  string                 `json:"organizationId,omitempty"`
		Quota           *control.ResourceQuota `json:"quota,omitempty"`
	} `json:"user"`
}

func (a api) handleUserLifecycle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(a.webhookSecret) < 32 {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLifecycleWebhookBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid webhook body", http.StatusBadRequest)
		return
	}
	receivedAt := time.Now()
	if !validLifecycleSignature(a.webhookSecret, r.Header.Get("X-Pie-Timestamp"), r.Header.Get("X-Pie-Signature"), body, receivedAt, a.webhookSkew) {
		http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
		return
	}
	var event lifecycleEvent
	if err := json.Unmarshal(body, &event); err != nil || event.ID == "" || event.OccurredAt.IsZero() || !manager.ValidUserID(event.User.ID) {
		http.Error(w, "invalid lifecycle event", http.StatusBadRequest)
		return
	}
	eventSkew := a.webhookSkew
	if eventSkew <= 0 {
		eventSkew = 5 * time.Minute
	}
	if event.OccurredAt.After(receivedAt.Add(eventSkew)) {
		http.Error(w, "lifecycle event occurredAt is in the future", http.StatusBadRequest)
		return
	}
	eventHash := lifecycleEventHash(event)
	status := ""
	switch event.Type {
	case "user.created", "user.reactivated":
		status = "active"
	case "user.updated":
	case "user.suspended":
		status = "suspended"
	case "user.deleted":
		status = "deleted"
	default:
		http.Error(w, "unsupported lifecycle event", http.StatusBadRequest)
		return
	}

	current, exists := a.control.User(event.User.ID)
	duplicate := exists && current.LifecycleEventID == event.ID
	if duplicate && current.LifecycleEventHash != "" && current.LifecycleEventHash != eventHash {
		http.Error(w, "lifecycle event id was reused with a different payload", http.StatusConflict)
		return
	}
	if duplicate && !current.LifecycleOccurredAt.Equal(event.OccurredAt) {
		http.Error(w, "lifecycle event id was reused with a different occurredAt", http.StatusConflict)
		return
	}
	if exists && !duplicate && !current.LifecycleOccurredAt.IsZero() && !event.OccurredAt.After(current.LifecycleOccurredAt) {
		write(w, map[string]any{"eventId": event.ID, "user": current, "ignored": "stale"}, http.StatusAccepted)
		return
	}
	if status == "" {
		if exists && current.Status != "" {
			status = current.Status
		} else {
			status = "active"
		}
	}
	if duplicate && status != current.Status {
		http.Error(w, "lifecycle event id was reused with a different type", http.StatusConflict)
		return
	}
	value := current
	if !duplicate {
		if !exists {
			value.ID = event.User.ID
		}
		value.Status = status
		value.LifecycleEventID, value.LifecycleEventHash, value.LifecycleOccurredAt = event.ID, eventHash, event.OccurredAt.UTC()
		if event.User.ExternalSubject != "" {
			value.ExternalSubject = event.User.ExternalSubject
		} else if value.ExternalSubject == "" {
			value.ExternalSubject = event.User.ID
		}
		if event.User.OrganizationID != "" || !exists {
			value.OrganizationID = event.User.OrganizationID
		}
		if event.User.Quota != nil {
			value.Quota = *event.User.Quota
		}
		if !exists || lifecycleUserChanged(current, value) {
			expected := int64(0)
			if exists {
				expected = current.Version
			}
			value, err = a.control.PutUser(r.Context(), value, expected, control.MutationMeta{ActorUserID: "lifecycle-webhook", RequestID: event.ID, Trusted: true})
			if err != nil {
				writeControlError(w, err)
				return
			}
		}
	}

	result := map[string]any{"eventId": event.ID, "user": value}
	if duplicate {
		result["duplicate"] = true
	}
	if status == "active" && event.Provision {
		if _, err := a.m.EnsureWithLimits(r.Context(), event.User.ID, executorLimits(value.Quota)); err != nil {
			http.Error(w, "user accepted but executor provisioning failed", http.StatusServiceUnavailable)
			return
		}
		result["provisioned"] = true
		write(w, result, http.StatusOK)
		return
	}

	queued := 0
	if status != "active" {
		if _, found := a.m.Executor(event.User.ID); found {
			if _, err := a.m.StopExecutor(r.Context(), event.User.ID); err != nil && !errors.Is(err, manager.ErrExecutorNotFound) {
				http.Error(w, "user updated but executor stop failed", http.StatusServiceUnavailable)
				return
			}
		}
		for _, device := range a.control.Devices() {
			if device.OwnerUserID != event.User.ID || device.DesiredState == "stopped" && device.ObservedState == "stopped" {
				continue
			}
			key := lifecycleIdempotencyKey(event.ID, device.ID)
			if _, _, err := a.control.BeginOperation(r.Context(), control.Operation{
				IdempotencyKey: key, ActorUserID: "lifecycle-webhook", Type: "device.drain",
				TargetKind: control.KindDevice, TargetID: device.ID,
			}, control.MutationMeta{ActorUserID: "lifecycle-webhook", RequestID: event.ID, Trusted: true}); err != nil {
				writeControlError(w, err)
				return
			}
			queued++
		}
	}
	result["drainOperations"] = queued
	responseStatus := http.StatusAccepted
	if duplicate {
		responseStatus = http.StatusOK
	}
	write(w, result, responseStatus)
}

func lifecycleUserChanged(before, after control.User) bool {
	return before.Status != after.Status || before.ExternalSubject != after.ExternalSubject || before.OrganizationID != after.OrganizationID || before.Quota != after.Quota || before.LifecycleEventID != after.LifecycleEventID || before.LifecycleEventHash != after.LifecycleEventHash || !before.LifecycleOccurredAt.Equal(after.LifecycleOccurredAt)
}

func lifecycleEventHash(event lifecycleEvent) string {
	canonical, _ := json.Marshal(event)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func lifecycleIdempotencyKey(eventID, deviceID string) string {
	sum := sha256.Sum256([]byte(eventID + "\x00" + deviceID))
	return "lifecycle-" + hex.EncodeToString(sum[:16])
}

func validLifecycleSignature(secret []byte, rawTimestamp, rawSignature string, body []byte, now time.Time, skew time.Duration) bool {
	if skew <= 0 {
		skew = 5 * time.Minute
	}
	timestamp, err := strconv.ParseInt(strings.TrimSpace(rawTimestamp), 10, 64)
	if err != nil {
		return false
	}
	signedAt := time.Unix(timestamp, 0)
	if now.Sub(signedAt) > skew || signedAt.Sub(now) > skew {
		return false
	}
	provided := strings.TrimPrefix(strings.TrimSpace(rawSignature), "v1=")
	decoded, err := hex.DecodeString(provided)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(mac, rawTimestamp)
	_, _ = io.WriteString(mac, ".")
	_, _ = mac.Write(body)
	return hmac.Equal(decoded, mac.Sum(nil))
}
