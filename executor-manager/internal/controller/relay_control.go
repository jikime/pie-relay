package controller

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

type DriverResult struct {
	UserID     string    `json:"userId,omitempty"`
	Generation uint64    `json:"generation"`
	ExpiresAt  time.Time `json:"expiresAt,omitempty"`
}

type RelayControl interface {
	DisconnectConnection(context.Context, string, string) error
	SetDriver(context.Context, string, string, string, string, string, int64) (DriverResult, error)
}

type HTTPRelayControl struct {
	Token  string
	Client *http.Client
}

func (c HTTPRelayControl) DisconnectConnection(ctx context.Context, address, connectionID string) error {
	endpoint, err := relayControlEndpoint(address, "/v1/control/connections/"+url.PathEscape(connectionID))
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	return c.do(request, nil)
}

func (c HTTPRelayControl) SetDriver(ctx context.Context, address, room, deviceID, sessionID, userID string, relayGeneration int64) (DriverResult, error) {
	endpoint, err := relayControlEndpoint(address, "/v1/control/driver")
	if err != nil {
		return DriverResult{}, err
	}
	body, err := json.Marshal(map[string]any{"room": room, "deviceId": deviceID, "sessionId": sessionID, "userId": userID, "relayGeneration": relayGeneration})
	if err != nil {
		return DriverResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return DriverResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	var result DriverResult
	if err := c.do(request, &result); err != nil {
		return DriverResult{}, err
	}
	return result, nil
}

func (c HTTPRelayControl) do(request *http.Request, output any) error {
	if strings.TrimSpace(c.Token) == "" {
		return errors.New("Relay control token is not configured")
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("Relay control returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	if output != nil {
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			return err
		}
	}
	return nil
}

func relayControlEndpoint(address, path string) (string, error) {
	address = strings.TrimRight(strings.TrimSpace(address), "/")
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" {
		return "", errors.New("Relay control address is unavailable")
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	case "http", "https":
	default:
		return "", errors.New("Relay control address must use HTTP(S) or WS(S)")
	}
	parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", "", ""
	return strings.TrimRight(parsed.String(), "/") + path, nil
}
