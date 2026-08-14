package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type PresenceEvent struct {
	EventID         string    `json:"eventId"`
	NodeID          string    `json:"nodeId"`
	ApplicationID   string    `json:"applicationId,omitempty"`
	PoolID          string    `json:"poolId,omitempty"`
	PublicURL       string    `json:"publicUrl,omitempty"`
	ControlURL      string    `json:"controlUrl,omitempty"`
	Room            string    `json:"room"`
	DeviceID        string    `json:"deviceId,omitempty"`
	SessionID       string    `json:"sessionId,omitempty"`
	RelayGeneration int64     `json:"relayGeneration,omitempty"`
	UserID          string    `json:"userId"`
	Role            string    `json:"role"`
	Access          string    `json:"access,omitempty"`
	ConnectionID    string    `json:"connectionId"`
	Kind            string    `json:"kind"`
	Connected       bool      `json:"connected"`
	HostOnline      bool      `json:"hostOnline"`
	Heartbeat       bool      `json:"heartbeat,omitempty"`
	At              time.Time `json:"at"`
}

type PresenceSink interface{ Report(PresenceEvent) bool }

// HTTPPresenceReporter decouples websocket forwarding from Control Plane
// latency. Its bounded queue never blocks a terminal connection; failed
// deliveries retry briefly and are visible through Relay metrics.
type HTTPPresenceReporter struct {
	endpoint string
	token    string
	client   *http.Client
	queue    chan PresenceEvent
	once     sync.Once
	wg       sync.WaitGroup
}

func NewHTTPPresenceReporter(endpoint, token string, capacity int) (*HTTPPresenceReporter, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return nil, errors.New("control plane URL is required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("control plane URL must be an HTTP(S) origin")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("control plane token is required")
	}
	if capacity < 1 {
		capacity = 1024
	}
	r := &HTTPPresenceReporter{endpoint: endpoint + "/v1/control/relay/presence", token: token, client: &http.Client{Timeout: 5 * time.Second}, queue: make(chan PresenceEvent, capacity)}
	r.wg.Add(1)
	go r.loop()
	return r, nil
}

func (r *HTTPPresenceReporter) Report(event PresenceEvent) (accepted bool) {
	defer func() {
		if recover() != nil {
			accepted = false
		}
	}()
	select {
	case r.queue <- event:
		return true
	default:
		return false
	}
}

func (r *HTTPPresenceReporter) Close() {
	if r == nil {
		return
	}
	r.once.Do(func() { close(r.queue); r.wg.Wait() })
}

func (r *HTTPPresenceReporter) loop() {
	defer r.wg.Done()
	for event := range r.queue {
		for attempt := 0; attempt < 3; attempt++ {
			if err := r.post(event); err == nil {
				break
			}
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			}
		}
	}
}

func (r *HTTPPresenceReporter) post(event PresenceEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", event.EventID)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("presence status %d", resp.StatusCode)
	}
	return nil
}
