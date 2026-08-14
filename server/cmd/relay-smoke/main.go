// Command relay-smoke validates a deployed standalone Pie Relay without a
// Control Plane. Credentials stay in memory and are never printed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type enrollResponse struct {
	Token string `json:"token"`
	Room  string `json:"room"`
}

type inviteResponse struct {
	Code string `json:"code"`
}

type joinResponse struct {
	Token string `json:"token"`
}

type assignmentResponse struct {
	V       int    `json:"v"`
	CellURL string `json:"cellUrl"`
	Lease   string `json:"lease"`
}

func main() {
	relayURL := flag.String("relay-url", strings.TrimSpace(os.Getenv("PIE_RELAY_SMOKE_URL")), "deployed Relay HTTPS origin")
	enrollSecret := flag.String("enroll-secret", strings.TrimSpace(os.Getenv("PIE_RELAY_SMOKE_ENROLL_SECRET")), "host enrollment secret")
	flag.Parse()

	if *relayURL == "" || *enrollSecret == "" {
		fatal(errors.New("PIE_RELAY_SMOKE_URL and PIE_RELAY_SMOKE_ENROLL_SECRET are required"))
	}
	origin, err := url.Parse(strings.TrimRight(*relayURL, "/"))
	if err != nil || origin.Scheme != "https" || origin.Host == "" {
		fatal(errors.New("relay URL must be an absolute HTTPS origin"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 8 * time.Second}
	for _, path := range []string{"/healthz", "/readyz"} {
		if err := expectStatus(ctx, client, origin.String()+path, http.MethodGet, nil, nil, http.StatusOK); err != nil {
			fatal(err)
		}
	}
	if err := expectStatus(ctx, client, origin.String()+"/metrics", http.MethodGet, nil, nil, http.StatusUnauthorized); err != nil {
		fatal(err)
	}
	wrongBody := map[string]any{"secret": "intentionally-wrong-enrollment-secret", "name": "cookai-smoke-host"}
	if err := expectStatus(ctx, client, origin.String()+"/host/enroll", http.MethodPost, wrongBody, nil, http.StatusUnauthorized); err != nil {
		fatal(err)
	}

	room := fmt.Sprintf("cookai-smoke-%d", time.Now().UnixNano())
	var enrolled enrollResponse
	if err := requestJSON(ctx, client, origin.String()+"/host/enroll", http.MethodPost, map[string]any{
		"secret": *enrollSecret,
		"room":   room,
		"name":   "cookai-smoke-host",
	}, nil, http.StatusOK, &enrolled); err != nil {
		fatal(err)
	}
	if enrolled.Token == "" || enrolled.Room != room {
		fatal(errors.New("host enrollment returned an incomplete credential"))
	}

	hostHeaders := http.Header{"Authorization": {"Bearer " + enrolled.Token}}
	var assignment assignmentResponse
	if err := requestJSON(ctx, client, origin.String()+"/v1/assign", http.MethodPost, map[string]any{
		"v": 1, "relayHostId": "CookAISmokeHost1",
	}, hostHeaders, http.StatusOK, &assignment); err != nil {
		fatal(err)
	}
	if assignment.V != 1 || assignment.CellURL != origin.String() || assignment.Lease == "" {
		fatal(errors.New("mobile assignment advertised an invalid Relay endpoint"))
	}
	var invite inviteResponse
	if err := requestJSON(ctx, client, origin.String()+"/rooms/invites", http.MethodPost, map[string]any{"access": "view"}, hostHeaders, http.StatusOK, &invite); err != nil {
		fatal(err)
	}
	if invite.Code == "" {
		fatal(errors.New("relay returned an empty invite code"))
	}
	var joined joinResponse
	if err := requestJSON(ctx, client, origin.String()+"/rooms/join", http.MethodPost, map[string]any{
		"code": invite.Code,
		"name": "cookai-smoke-viewer",
	}, nil, http.StatusOK, &joined); err != nil {
		fatal(err)
	}
	if joined.Token == "" {
		fatal(errors.New("relay returned an empty participant credential"))
	}

	wsOrigin := *origin
	wsOrigin.Scheme = "wss"
	badOrigin, badOriginResponse, badOriginErr := websocket.Dial(ctx, wsOrigin.String()+"/ws/participant", &websocket.DialOptions{
		HTTPHeader:   http.Header{"Origin": {"https://not-allowed.example"}},
		Subprotocols: []string{"pie-relay.ticket." + joined.Token},
	})
	if badOrigin != nil {
		_ = badOrigin.Close(websocket.StatusNormalClosure, "unexpected acceptance")
	}
	if badOriginResponse != nil {
		defer badOriginResponse.Body.Close()
	}
	if badOriginErr == nil || badOriginResponse == nil || badOriginResponse.StatusCode != http.StatusForbidden {
		fatal(errors.New("participant WSS accepted a disallowed Origin"))
	}
	wrongAxis, wrongAxisResponse, wrongAxisErr := websocket.Dial(ctx, wsOrigin.String()+"/ws/agent", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + joined.Token}},
	})
	if wrongAxis != nil {
		_ = wrongAxis.Close(websocket.StatusNormalClosure, "unexpected acceptance")
	}
	if wrongAxisResponse != nil {
		defer wrongAxisResponse.Body.Close()
	}
	wrongAxisRejected := wrongAxisResponse != nil && (wrongAxisResponse.StatusCode == http.StatusUnauthorized || wrongAxisResponse.StatusCode == http.StatusForbidden)
	if wrongAxisErr == nil || !wrongAxisRejected {
		status := 0
		if wrongAxisResponse != nil {
			status = wrongAxisResponse.StatusCode
		}
		fatal(fmt.Errorf("participant credential host-axis rejection mismatch: connected=%t status=%d error=%v", wrongAxis != nil, status, wrongAxisErr))
	}
	host, _, err := websocket.Dial(ctx, wsOrigin.String()+"/ws/agent", &websocket.DialOptions{HTTPHeader: hostHeaders})
	if err != nil {
		fatal(fmt.Errorf("host WSS connect: %w", err))
	}
	defer host.Close(websocket.StatusNormalClosure, "smoke complete")
	participant, _, err := websocket.Dial(ctx, wsOrigin.String()+"/ws/participant", &websocket.DialOptions{
		Subprotocols: []string{"pie-relay.ticket." + joined.Token},
	})
	if err != nil {
		fatal(fmt.Errorf("participant WSS connect: %w", err))
	}
	defer participant.Close(websocket.StatusNormalClosure, "smoke complete")

	joinFrame := []byte(`{"type":"relay_join","protocolVersion":"2.0","streamId":"terminal","clientId":"cookai-smoke"}`)
	if err := host.Write(ctx, websocket.MessageText, joinFrame); err != nil {
		fatal(fmt.Errorf("host relay_join: %w", err))
	}
	if err := participant.Write(ctx, websocket.MessageText, joinFrame); err != nil {
		fatal(fmt.Errorf("participant relay_join: %w", err))
	}
	if err := readUntilType(ctx, host, "relay_join_ack"); err != nil {
		fatal(fmt.Errorf("host handshake: %w", err))
	}
	if err := readUntilType(ctx, participant, "relay_join_ack"); err != nil {
		fatal(fmt.Errorf("participant handshake: %w", err))
	}

	marker := fmt.Sprintf("pie-azure-%d", time.Now().UnixNano())
	hostFrame, _ := json.Marshal(map[string]any{"type": "pty_output", "data": marker})
	if err := host.Write(ctx, websocket.MessageText, hostFrame); err != nil {
		fatal(fmt.Errorf("host-to-participant write: %w", err))
	}
	if err := readUntilMarker(ctx, participant, "pty_output", marker); err != nil {
		fatal(fmt.Errorf("host-to-participant relay: %w", err))
	}
	if err := participant.Write(ctx, websocket.MessageText, []byte(`{"type":"request_screen"}`)); err != nil {
		fatal(fmt.Errorf("participant-to-host write: %w", err))
	}
	if err := readUntilType(ctx, host, "request_screen"); err != nil {
		fatal(fmt.Errorf("participant-to-host relay: %w", err))
	}

	if err := participant.Close(websocket.StatusNormalClosure, "test participant reconnect"); err != nil {
		fatal(fmt.Errorf("close participant before reconnect: %w", err))
	}
	participant, _, err = websocket.Dial(ctx, wsOrigin.String()+"/ws/participant", &websocket.DialOptions{
		Subprotocols: []string{"pie-relay.ticket." + joined.Token},
	})
	if err != nil {
		fatal(fmt.Errorf("participant WSS reconnect: %w", err))
	}
	defer participant.Close(websocket.StatusNormalClosure, "smoke complete")
	if err := participant.Write(ctx, websocket.MessageText, joinFrame); err != nil {
		fatal(fmt.Errorf("participant relay_join after reconnect: %w", err))
	}
	if err := readUntilType(ctx, participant, "relay_join_ack"); err != nil {
		fatal(fmt.Errorf("participant reconnect handshake: %w", err))
	}
	reconnectMarker := fmt.Sprintf("pie-azure-reconnect-%d", time.Now().UnixNano())
	reconnectFrame, _ := json.Marshal(map[string]any{"type": "pty_output", "data": reconnectMarker})
	if err := host.Write(ctx, websocket.MessageText, reconnectFrame); err != nil {
		fatal(fmt.Errorf("host write after participant reconnect: %w", err))
	}
	if err := readUntilMarker(ctx, participant, "pty_output", reconnectMarker); err != nil {
		fatal(fmt.Errorf("participant reconnect relay: %w", err))
	}

	if err := host.Close(websocket.StatusNormalClosure, "test host reconnect"); err != nil {
		fatal(fmt.Errorf("close host before reconnect: %w", err))
	}
	host, _, err = websocket.Dial(ctx, wsOrigin.String()+"/ws/agent", &websocket.DialOptions{HTTPHeader: hostHeaders})
	if err != nil {
		fatal(fmt.Errorf("host WSS reconnect: %w", err))
	}
	defer host.Close(websocket.StatusNormalClosure, "smoke complete")
	if err := host.Write(ctx, websocket.MessageText, joinFrame); err != nil {
		fatal(fmt.Errorf("host relay_join after reconnect: %w", err))
	}
	if err := readUntilType(ctx, host, "relay_join_ack"); err != nil {
		fatal(fmt.Errorf("host reconnect handshake: %w", err))
	}
	if err := participant.Write(ctx, websocket.MessageText, []byte(`{"type":"request_screen"}`)); err != nil {
		fatal(fmt.Errorf("participant write after host reconnect: %w", err))
	}
	if err := readUntilType(ctx, host, "request_screen"); err != nil {
		fatal(fmt.Errorf("host reconnect relay: %w", err))
	}

	fmt.Printf("{\"ok\":true,\"relay\":%q,\"checks\":[\"health\",\"ready\",\"protected-metrics\",\"enroll-auth\",\"mobile-assignment\",\"invite-join\",\"origin-policy\",\"role-isolation\",\"host-wss\",\"participant-wss\",\"bidirectional-relay\",\"participant-reconnect\",\"host-reconnect\"]}\n", origin.String())
}

func requestJSON(ctx context.Context, client *http.Client, endpoint, method string, body any, headers http.Header, wantStatus int, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s %s: HTTP %d, want %d: %s", method, endpoint, resp.StatusCode, wantStatus, strings.TrimSpace(string(payload)))
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out); err != nil {
			return err
		}
	}
	return nil
}

func expectStatus(ctx context.Context, client *http.Client, endpoint, method string, body any, headers http.Header, wantStatus int) error {
	if body == nil {
		req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
		if err != nil {
			return err
		}
		for name, values := range headers {
			for _, value := range values {
				req.Header.Add(name, value)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != wantStatus {
			return fmt.Errorf("%s %s: HTTP %d, want %d", method, endpoint, resp.StatusCode, wantStatus)
		}
		return nil
	}
	return requestJSON(ctx, client, endpoint, method, body, headers, wantStatus, nil)
}

func readUntilType(ctx context.Context, conn *websocket.Conn, want string) error {
	for {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var frame struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(payload, &frame) == nil && frame.Type == want {
			return nil
		}
	}
}

func readUntilMarker(ctx context.Context, conn *websocket.Conn, wantType, marker string) error {
	for {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var frame struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if json.Unmarshal(payload, &frame) == nil && frame.Type == wantType && frame.Data == marker {
			return nil
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "relay-smoke:", err)
	os.Exit(1)
}
