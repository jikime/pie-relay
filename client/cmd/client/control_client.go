package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type deviceControlClient struct {
	base, deviceID, deviceName string
	tokens                     controlTokenSource
	client                     *http.Client
	readiness                  *runtimeReadinessMonitor
}

type controlTokenSource interface {
	Token(context.Context, bool) (string, error)
}

type fixedControlToken string

func (t fixedControlToken) Token(_ context.Context, _ bool) (string, error) {
	if t == "" {
		return "", errors.New("control token is required")
	}
	return string(t), nil
}

type desiredDeviceSession struct {
	ID              string `json:"id"`
	AgentID         string `json:"agentId,omitempty"`
	AgentMode       string `json:"agentMode,omitempty"`
	DeviceID        string `json:"deviceId"`
	ExecutionTarget string `json:"executionTarget"`
	Status          string `json:"status"`
	DriverUserID    string `json:"driverUserId,omitempty"`
}

type deviceSessionCredential struct {
	Token    string `json:"token"`
	RelayURL string `json:"relayUrl"`
}

func newDeviceControlClient(base, token, deviceID, deviceName string) (*deviceControlClient, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("control URL must be an HTTP(S) origin")
	}
	if token == "" || deviceID == "" {
		return nil, errors.New("control token and device id are required")
	}
	if deviceName == "" {
		deviceName = deviceID
	}
	return newDeviceControlClientWithSource(base, fixedControlToken(token), deviceID, deviceName)
}

func newDeviceControlClientWithSource(base string, tokens controlTokenSource, deviceID, deviceName string) (*deviceControlClient, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("control URL must be an HTTP(S) origin")
	}
	if tokens == nil || deviceID == "" {
		return nil, errors.New("control token source and device id are required")
	}
	if deviceName == "" {
		deviceName = deviceID
	}
	return &deviceControlClient{
		base: base, tokens: tokens, deviceID: deviceID, deviceName: deviceName,
		client: &http.Client{Timeout: 8 * time.Second}, readiness: newRuntimeReadinessMonitor(),
	}, nil
}

func (c *deviceControlClient) register(ctx context.Context) error {
	return c.post(ctx, "/v1/control/devices/register", map[string]any{"id": c.deviceID, "name": c.deviceName, "kind": "local", "desiredState": "running", "observedState": "starting", "metadata": c.readiness.metadata(ctx, true)})
}
func (c *deviceControlClient) heartbeat(ctx context.Context, active int, clientOnline, relayConnected bool) error {
	state := "offline"
	if clientOnline {
		state = "online"
	}
	return c.post(ctx, "/v1/control/devices/"+url.PathEscape(c.deviceID)+"/heartbeat", map[string]any{
		"observedState": state, "runtimeRunning": clientOnline,
		"runtimeHealthy":  clientOnline && c.readiness.healthy(ctx),
		"clientConnected": clientOnline, "relayRegistered": clientOnline && relayConnected,
		"activeSessions": active, "metadata": c.readiness.metadata(ctx, false),
	})
}

func (c *deviceControlClient) desiredSessions(ctx context.Context) ([]desiredDeviceSession, error) {
	var sessions []desiredDeviceSession
	path := "/v1/control/devices/" + url.PathEscape(c.deviceID) + "/sessions?limit=256"
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

func (c *deviceControlClient) sessionCredential(ctx context.Context, sessionID string) (deviceSessionCredential, error) {
	var credential deviceSessionCredential
	path := "/v1/control/sessions/" + url.PathEscape(sessionID) + "/credential"
	err := c.requestJSON(ctx, http.MethodPost, path, map[string]any{
		"role": "host", "access": "control", "ttlSeconds": 24 * 60 * 60,
	}, &credential)
	if err != nil {
		return deviceSessionCredential{}, err
	}
	if credential.Token == "" || credential.RelayURL == "" {
		return deviceSessionCredential{}, errors.New("control plane returned an incomplete session credential")
	}
	return credential, nil
}

func (c *deviceControlClient) reportSession(ctx context.Context, sessionID, status, lastError string) error {
	path := "/v1/control/devices/" + url.PathEscape(c.deviceID) + "/sessions/" + url.PathEscape(sessionID) + "/status"
	return c.requestJSON(ctx, http.MethodPost, path, map[string]any{
		"status": status, "selectedTransport": "relay", "lastError": lastError,
	}, nil)
}

func relayAgentURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", errors.New("relay URL is invalid")
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", errors.New("relay URL must use http(s) or ws(s)")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if path == "" {
		path = "/ws/agent"
	} else if !strings.HasSuffix(path, "/ws/agent") {
		path += "/ws/agent"
	}
	parsed.Path, parsed.RawPath = path, ""
	parsed.RawQuery, parsed.Fragment = "", ""
	return parsed.String(), nil
}

func (c *deviceControlClient) post(ctx context.Context, path string, value any) error {
	return c.requestJSON(ctx, http.MethodPost, path, value, nil)
}

func (c *deviceControlClient) requestJSON(ctx context.Context, method, path string, value, output any) error {
	var body []byte
	var err error
	if value != nil {
		body, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, tokenErr := c.tokens.Token(ctx, attempt > 0)
		if tokenErr != nil {
			return tokenErr
		}
		req, requestErr := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
		if requestErr != nil {
			return requestErr
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if value != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, requestErr := c.client.Do(req)
		if requestErr != nil {
			return requestErr
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			message, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			return fmt.Errorf("control plane %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(message)))
		}
		if output != nil {
			if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output); err != nil {
				resp.Body.Close()
				return fmt.Errorf("decode control plane %s: %w", path, err)
			}
		}
		resp.Body.Close()
		return nil
	}
	return errors.New("control plane authentication failed after token refresh")
}
