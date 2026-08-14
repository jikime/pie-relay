package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cli-relay/client/internal/sessionmanager"
)

// reconcileDeviceSessions treats Control Plane Session rows as desired state
// for this Host OS device.  All traffic is outbound: the agent fetches its own
// assignments, obtains a short-lived scoped Relay credential, and feeds that
// credential to the loopback-only Session Manager.
func reconcileDeviceSessions(ctx context.Context, control *deviceControlClient, manager *sessionmanager.Manager) error {
	desired, err := control.desiredSessions(ctx)
	if err != nil {
		return err
	}
	local := make(map[string]sessionmanager.Status)
	for _, status := range manager.List() {
		local[status.ID] = status
	}
	var results []error
	for _, session := range desired {
		if session.DeviceID != control.deviceID || session.ExecutionTarget != "local" {
			continue
		}
		status, exists := local[session.ID]
		switch session.Status {
		case "creating", "starting", "provisioning", "ready", "active", "idle", "reconnecting":
			forceRestart := false
			if exists && status.State != "running" && status.State != "starting" {
				forceRestart = true
			}
			if exists && (status.State == "running" || status.State == "starting") {
				// 프로세스가 실행 중이라는 사실만으로 Control 세션을 ready로
				// 바꾸면 안 된다. Relay host WebSocket이 실제로 등록되기 전에
				// participant가 요청을 보내 agent:unavailable을 받는 경합이 생긴다.
				if status.RelayState == "connected" &&
					(session.Status == "creating" || session.Status == "starting" || session.Status == "provisioning" || session.Status == "reconnecting") {
					if err := reportDeviceReady(ctx, control, session.ID); err != nil {
						results = append(results, err)
					}
					continue
				}
				// Wi-Fi/VPN 변경이나 Relay 재배포 뒤에는 기존 세션 자격에
				// 과거 공개 주소가 남을 수 있다. 연결 재시도가 일정 시간 이상
				// 계속되면 Control에서 최신 Relay 주소와 자격을 다시 받는다.
				if session.Status == "reconnecting" ||
					((status.RelayState == "reconnecting" || status.RelayState == "disconnected") &&
						time.Since(status.StartedAt) >= 5*time.Second) {
					forceRestart = true
				} else {
					continue
				}
			}
			if forceRestart && exists {
				removeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := manager.Remove(removeCtx, session.ID)
				cancel()
				if err != nil && !errors.Is(err, sessionmanager.ErrNotFound) {
					results = append(results, reportDeviceFailure(ctx, control, session.ID, err))
					continue
				}
				exists = false
			}
			if exists {
				continue
			}
			credential, err := control.sessionCredential(ctx, session.ID)
			if err != nil {
				results = append(results, reportDeviceFailure(ctx, control, session.ID, err))
				continue
			}
			relayURL, err := relayAgentURL(credential.RelayURL)
			if err != nil {
				results = append(results, reportDeviceFailure(ctx, control, session.ID, err))
				continue
			}
			_, _, err = manager.Start(sessionmanager.Config{
				ID:            session.ID,
				AgentID:       session.AgentID,
				AgentMode:     session.AgentMode,
				RelayURL:      relayURL,
				Token:         credential.Token,
				StreamID:      session.AgentMode,
				InitialDriver: session.DriverUserID,
			})
			if err != nil {
				results = append(results, reportDeviceFailure(ctx, control, session.ID, err))
				continue
			}
			// manager.Start는 실행 goroutine만 예약한다. ready 보고는 다음
			// reconcile에서 RelayState=connected를 확인한 뒤 수행한다.
		case "closing", "closed", "error":
			if exists && (status.State == "running" || status.State == "starting") {
				removeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := manager.Remove(removeCtx, session.ID)
				cancel()
				if err != nil && !errors.Is(err, sessionmanager.ErrNotFound) {
					results = append(results, fmt.Errorf("stop session %s: %w", session.ID, err))
					continue
				}
			}
			if session.Status == "closing" {
				if err := control.reportSession(ctx, session.ID, "closed", ""); err != nil {
					results = append(results, fmt.Errorf("report session %s closed: %w", session.ID, err))
				}
			}
		}
	}
	return errors.Join(results...)
}

func reportDeviceReady(ctx context.Context, control *deviceControlClient, sessionID string) error {
	if err := control.reportSession(ctx, sessionID, "ready", ""); err != nil {
		return fmt.Errorf("report session %s ready: %w", sessionID, err)
	}
	return nil
}

func reportDeviceFailure(ctx context.Context, control *deviceControlClient, sessionID string, cause error) error {
	message := "session start failed"
	if cause != nil {
		message = strings.TrimSpace(cause.Error())
	}
	if len(message) > 2048 {
		message = message[:2048]
	}
	if err := control.reportSession(ctx, sessionID, "error", message); err != nil {
		return fmt.Errorf("session %s failed (%s); report status: %w", sessionID, message, err)
	}
	return fmt.Errorf("session %s failed: %s", sessionID, message)
}
