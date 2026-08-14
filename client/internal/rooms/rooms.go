// Package rooms is the client-side HTTP client for the relay's room invite
// flow (/rooms/invites, /rooms/join) and derives the relay's HTTP/ws endpoints
// from the ws(s) PIE_RELAY_URL a user already configures for the daemon.
package rooms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpTimeout bounds the invite/join REST calls — these are quick control-plane
// requests, not the long-lived ws chat, so a short deadline surfaces an
// unreachable relay fast instead of hanging the CLI.
const httpTimeout = 10 * time.Second

// HTTPBase derives the relay's HTTP(S) base (scheme://host) from a ws(s) relay
// URL like ws://host:13412/ws/agent → http://host:13412. wss→https. A plain
// http(s) URL is accepted and reduced to its scheme+host. The path/query are
// dropped: /rooms/* live at the relay root, not under /ws/agent.
func HTTPBase(relayURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(relayURL))
	if err != nil {
		return "", fmt.Errorf("relay URL 파싱 실패: %w", err)
	}
	switch u.Scheme {
	case "ws", "http":
		u.Scheme = "http"
	case "wss", "https":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("relay URL 스킴이 ws/wss/http/https 가 아닙니다: %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("relay URL 에 호스트가 없습니다: %q", relayURL)
	}
	return u.Scheme + "://" + u.Host, nil
}

// ParticipantWSURL derives the /ws/participant endpoint from a ws(s)/http(s)
// relay URL, keeping the host and forcing a ws(s) scheme and the participant
// path. Query and any /ws/agent path on the input are discarded.
func ParticipantWSURL(relayURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(relayURL))
	if err != nil {
		return "", fmt.Errorf("relay URL 파싱 실패: %w", err)
	}
	switch u.Scheme {
	case "ws", "http":
		u.Scheme = "ws"
	case "wss", "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("relay URL 스킴이 ws/wss/http/https 가 아닙니다: %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("relay URL 에 호스트가 없습니다: %q", relayURL)
	}
	return u.Scheme + "://" + u.Host + "/ws/participant", nil
}

// CreateInviteResult is the decoded POST /rooms/invites response.
type CreateInviteResult struct {
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expiresAt"` // unix seconds
}

// CreateInvite calls POST {base}/rooms/invites with the host's access token as
// a Bearer credential. base is an HTTP(S) origin (see HTTPBase). The relay
// binds the invite to the room in the token's claims.
func CreateInvite(ctx context.Context, base, token string) (CreateInviteResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/rooms/invites", nil)
	if err != nil {
		return CreateInviteResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		return CreateInviteResult{}, fmt.Errorf("초대 코드 요청 실패: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return CreateInviteResult{}, fmt.Errorf("초대 코드 발급 거부(HTTP %d) — `client login` 으로 호스트 자격을 다시 확인하세요", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return CreateInviteResult{}, fmt.Errorf("초대 코드 발급 실패: HTTP %d %s", resp.StatusCode, snippet(resp.Body))
	}
	var out CreateInviteResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return CreateInviteResult{}, fmt.Errorf("초대 코드 응답 디코드 실패: %w", err)
	}
	return out, nil
}

// JoinResult is the decoded POST /rooms/join response.
type JoinResult struct {
	Token string `json:"token"`
	Room  string `json:"room"`
}

// Join calls POST {base}/rooms/join with {code, name} and returns the minted
// participant token — no host credentials required (guest flow).
func Join(ctx context.Context, base, code, name string) (JoinResult, error) {
	body, err := json.Marshal(map[string]string{"code": code, "name": name})
	if err != nil {
		return JoinResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/rooms/join", bytes.NewReader(body))
	if err != nil {
		return JoinResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		return JoinResult{}, fmt.Errorf("방 참가 요청 실패: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return JoinResult{}, fmt.Errorf("초대 코드가 유효하지 않거나 만료되었습니다")
	}
	if resp.StatusCode != http.StatusOK {
		return JoinResult{}, fmt.Errorf("방 참가 실패: HTTP %d %s", resp.StatusCode, snippet(resp.Body))
	}
	var out JoinResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return JoinResult{}, fmt.Errorf("방 참가 응답 디코드 실패: %w", err)
	}
	return out, nil
}

// snippet reads a short prefix of an error response body for diagnostics,
// avoiding dumping a full page into a log line.
func snippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 256))
	return strings.TrimSpace(string(b))
}
