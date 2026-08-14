// Command relay runs the Pie Relay bridge server.
//
// Server-side component: bridges, per ROOM, participant websockets
// (/ws/participant; /ws/browser is a deprecated alias) and the laptop daemon
// websocket (/ws/agent → executor → claude CLI). It relays payloads verbatim —
// only top-level type/from/to are ever inspected, no code executed here.
//
// Auth (shared-secret JWT on BOTH legs, verified offline — no callback to the
// issuer): host leg via `Authorization: Bearer <jwt>`, participant leg via
// Bearer or a WebSocket ticket subprotocol. Legacy query-string credentials
// and unscoped JWTs are explicit migration opt-ins only. Tokens are minted by
// Pie Control or by this relay's own host/invite issuers using the same
// RELAY_JWT_SECRET.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"cli-relay/server/internal/relay"
)

func main() {
	_ = godotenv.Load(".env.local")

	addr := flag.String("addr", ":13412", "listen address")
	allowLoopbackEnroll := flag.Bool(
		"allow-loopback-enroll-without-secret",
		strings.EqualFold(os.Getenv("ALLOW_LOOPBACK_ENROLL_WITHOUT_SECRET"), "true") || os.Getenv("ALLOW_LOOPBACK_ENROLL_WITHOUT_SECRET") == "1",
		"allow direct loopback /host/enroll without HOST_ENROLL_SECRET (local-only; never enable behind a reverse proxy)",
	)
	allowLegacyQueryTicket := flag.Bool(
		"allow-legacy-query-ticket",
		strings.EqualFold(os.Getenv("RELAY_ALLOW_LEGACY_QUERY_TICKET"), "true") || os.Getenv("RELAY_ALLOW_LEGACY_QUERY_TICKET") == "1",
		"temporarily accept participant JWTs in ?ticket= URLs (unsafe for production access logs)",
	)
	allowLegacyUnscopedTokens := flag.Bool(
		"allow-legacy-unscoped-tokens",
		strings.EqualFold(os.Getenv("RELAY_ALLOW_LEGACY_UNSCOPED_TOKENS"), "true") || os.Getenv("RELAY_ALLOW_LEGACY_UNSCOPED_TOKENS") == "1",
		"temporarily accept JWTs without audience, issuer, jti, and capability claims",
	)
	requirePoolScope := flag.Bool(
		"require-pool-scope",
		strings.EqualFold(os.Getenv("RELAY_REQUIRE_POOL_SCOPE"), "true") || os.Getenv("RELAY_REQUIRE_POOL_SCOPE") == "1",
		"require versioned application, pool, resource, and relay-node claims",
	)
	allowDirectTokensWithoutPoolScope := flag.Bool(
		"allow-direct-tokens-without-pool-scope",
		envBool("RELAY_ALLOW_DIRECT_TOKENS_WITHOUT_POOL_SCOPE", true),
		"allow Relay-issued mobile/manual/invite tokens without application and pool claims while managed Pie Control tokens remain pool-scoped",
	)
	secret := flag.String("jwt-secret", os.Getenv("RELAY_JWT_SECRET"), "shared HMAC secret for JWT verification, both legs (env: RELAY_JWT_SECRET; required)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (optional; omit behind a TLS-terminating proxy)")
	tlsKey := flag.String("tls-key", "", "TLS key file (required if --tls-cert is set)")
	mobilePublicURL := flag.String("mobile-public-url", os.Getenv("RELAY_PUBLIC_URL"), "public HTTP(S) origin for Pie Relay mobile clients (env: RELAY_PUBLIC_URL)")
	mobileStateFile := flag.String("mobile-state-file", os.Getenv("RELAY_MOBILE_STATE_FILE"), "file that persists hashed mobile resume credentials (env: RELAY_MOBILE_STATE_FILE)")
	mobileStateAllowUnsupportedMode := flag.Bool(
		"mobile-state-allow-unsupported-mode",
		strings.EqualFold(os.Getenv("RELAY_MOBILE_STATE_ALLOW_UNSUPPORTED_MODE"), "true") || os.Getenv("RELAY_MOBILE_STATE_ALLOW_UNSUPPORTED_MODE") == "1",
		"allow a state volume without POSIX chmod support after a private temporary file is created (env: RELAY_MOBILE_STATE_ALLOW_UNSUPPORTED_MODE)",
	)
	nodeID := flag.String("node-id", defaultNodeID(), "stable relay node id (env: RELAY_NODE_ID)")
	poolID := flag.String("pool-id", strings.TrimSpace(os.Getenv("RELAY_POOL_ID")), "relay pool id (env: RELAY_POOL_ID)")
	maxParticipants := flag.Int("max-participants-per-room", envInt("RELAY_MAX_PARTICIPANTS_PER_ROOM", 64), "maximum participant sockets per room")
	framesPerSecond := flag.Float64("frames-per-second", envFloat("RELAY_FRAMES_PER_SECOND", 240), "per-connection inbound frame rate")
	bytesPerSecond := flag.Float64("bytes-per-second", envFloat("RELAY_BYTES_PER_SECOND", 8<<20), "per-connection inbound byte rate")
	healthURL := flag.String("healthcheck", "", "GET the URL and exit 0 on 2xx (Docker healthcheck probe mode)")
	// Dev convenience: mint a short-lived JWT for a user and print it, then
	// exit — for manual testing without the external membership/control flow.
	//   relay -mint <userID> [-mint-ttl 24h] [-mint-room <room>] [-mint-role host|participant]
	mintUser := flag.String("mint", "", "mint a JWT for this userID and print it, then exit (dev/testing only)")
	mintTTL := flag.Duration("mint-ttl", 24*time.Hour, "token TTL for -mint")
	mintRoom := flag.String("mint-room", "", "room claim for -mint (default: same as userID / legacy shape)")
	mintRole := flag.String("mint-role", "", "role claim for -mint: host|participant (default: empty, inferred from ws axis)")
	mintAccess := flag.String("mint-access", "", "access claim for -mint: view|control (default: empty = control/legacy)")
	flag.Parse()

	if *healthURL != "" {
		client := &http.Client{Timeout: 4 * time.Second}
		resp, err := client.Get(*healthURL)
		if err != nil {
			log.Fatalf("relay: healthcheck: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Fatalf("relay: healthcheck: status %d", resp.StatusCode)
		}
		os.Exit(0)
	}

	if err := validateSecret("RELAY_JWT_SECRET", *secret, true); err != nil {
		log.Fatal(err)
	}

	allowedApplicationValues := envList("RELAY_ALLOWED_APPLICATIONS", nil)
	allowedApplications := make(map[string]bool, len(allowedApplicationValues))
	for _, applicationID := range allowedApplicationValues {
		allowedApplications[applicationID] = true
	}
	if *requirePoolScope && (*poolID == "" || *nodeID == "" || len(allowedApplications) == 0) {
		log.Fatal("RELAY_REQUIRE_POOL_SCOPE requires RELAY_POOL_ID, RELAY_NODE_ID, and RELAY_ALLOWED_APPLICATIONS")
	}
	auth := relay.JWTAuth{
		Secret: []byte(*secret), RequireScopedCapabilities: !*allowLegacyUnscopedTokens,
		RequirePoolScope: *requirePoolScope, PoolID: *poolID, NodeID: *nodeID,
		AllowedApplications: allowedApplications, AllowDirectTokensWithoutPoolScope: *allowDirectTokensWithoutPoolScope,
	}

	if *mintUser != "" {
		room, role := *mintRoom, *mintRole
		if role == "" && !*allowLegacyUnscopedTokens {
			room, role = *mintUser, relay.RoleHost
		}
		signed, err := auth.Mint(*mintUser, room, role, *mintAccess, *mintTTL)
		if err != nil {
			log.Fatalf("mint: %v", err)
		}
		fmt.Println(signed)
		return
	}

	mobileRelay, err := relay.NewMobileRelay(relay.MobileRelayOptions{
		Auth: auth, PublicURL: *mobilePublicURL, StateFile: *mobileStateFile,
		AllowUnsupportedStateFileMode: *mobileStateAllowUnsupportedMode,
	})
	if err != nil {
		log.Fatalf("Pie Relay 모바일 초기화 실패: %v", err)
	}
	opts := relay.ServerOptions{
		AgentAuth: auth, ParticipantAuth: auth, Inviter: relay.NewInviter(auth), MobileRelay: mobileRelay,
		NodeID: *nodeID, PublicURL: *mobilePublicURL, ControlURL: strings.TrimSpace(os.Getenv("PIE_RELAY_CONTROL_URL")), ControlToken: strings.TrimSpace(os.Getenv("PIE_RELAY_CONTROL_TOKEN")),
		MetricsToken: strings.TrimSpace(os.Getenv("PIE_RELAY_METRICS_TOKEN")), AllowLegacyQueryTicket: *allowLegacyQueryTicket,
		OriginPatterns: envList("RELAY_ALLOWED_ORIGINS", []string{
			"tauri://localhost", "http://tauri.localhost", "https://tauri.localhost",
			"http://localhost:1420", "http://127.0.0.1:1420",
		}),
		Limits: relay.Limits{
			MaxParticipantsPerRoom: *maxParticipants,
			FramesPerSecond:        *framesPerSecond, FrameBurst: *framesPerSecond * 2,
			BytesPerSecond: *bytesPerSecond, ByteBurst: *bytesPerSecond * 2,
		},
	}
	if opts.ControlToken == "" {
		log.Print("relay: 운영 제어 API 비활성화 (PIE_RELAY_CONTROL_TOKEN 미설정)")
	} else {
		log.Print("relay: 운영 제어 API 활성화")
	}
	if opts.MetricsToken == "" {
		log.Print("relay: /metrics 비활성화 (PIE_RELAY_METRICS_TOKEN 미설정)")
	} else {
		log.Print("relay: /metrics bearer 인증 활성화")
	}
	var presenceReporter *relay.HTTPPresenceReporter
	controlURL := strings.TrimSpace(os.Getenv("PIE_CONTROL_PLANE_URL"))
	controlToken := strings.TrimSpace(os.Getenv("PIE_CONTROL_PLANE_TOKEN"))
	if (controlURL == "") != (controlToken == "") {
		log.Fatal("PIE_CONTROL_PLANE_URL and PIE_CONTROL_PLANE_TOKEN must be configured together")
	}
	if controlURL != "" {
		presenceReporter, err = relay.NewHTTPPresenceReporter(controlURL, controlToken, envInt("PIE_CONTROL_PRESENCE_QUEUE", 4096))
		if err != nil {
			log.Fatalf("Pie Control presence reporter: %v", err)
		}
		defer presenceReporter.Close()
		opts.Presence = presenceReporter
		log.Printf("relay: Pie Control presence reporting enabled: %s", controlURL)
	}
	log.Print("relay: shared-secret JWT auth on both legs (offline verify, host Bearer · participant Bearer/ticket)")
	if *allowLegacyUnscopedTokens {
		log.Print("relay: 경고: scope 없는 구형 JWT 허용됨 (마이그레이션 후 RELAY_ALLOW_LEGACY_UNSCOPED_TOKENS=false 필요)")
	} else {
		log.Print("relay: scoped JWT 강제 (iss/aud/jti/role/cap 필수)")
	}
	if *requirePoolScope {
		log.Printf("relay: Pool scope 강제 pool=%s node=%s applications=%s", *poolID, *nodeID, strings.Join(allowedApplicationValues, ","))
		if *allowDirectTokensWithoutPoolScope {
			log.Print("relay: Relay 직접 발급 모바일·수동·초대 토큰은 명시적 호환 경로로 허용")
		}
	} else if *poolID != "" || len(allowedApplications) > 0 {
		log.Printf("relay: Pool scope 호환 모드 pool=%s node=%s applications=%s", *poolID, *nodeID, strings.Join(allowedApplicationValues, ","))
	}

	// Host self-enrollment (/host/enroll): the operator sets HOST_ENROLL_SECRET
	// to hand out host tokens without an external browser login. Unset = the
	// endpoint stays off (503). HOST_ENROLL_TTL (Go duration) overrides the
	// default host token lifetime. Never log the secret itself — only whether
	// it is configured.
	enrollSecret := os.Getenv("HOST_ENROLL_SECRET")
	if err := validateSecret("HOST_ENROLL_SECRET", enrollSecret, false); err != nil {
		log.Fatal(err)
	}
	enrollTTL := time.Duration(0) // 0 = Enroller uses its default
	if v := os.Getenv("HOST_ENROLL_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("HOST_ENROLL_TTL 파싱 실패(%q): %v", v, err)
		}
		enrollTTL = d
	}
	opts.Enroller = relay.NewEnroller(auth, enrollSecret, enrollTTL, *allowLoopbackEnroll)
	if enrollSecret != "" {
		log.Print("relay: host enroll 활성화 (POST /host/enroll, 운영자 시크릿 설정됨)")
	} else {
		log.Print("relay: host enroll 미설정 (HOST_ENROLL_SECRET 없음 → /host/enroll 503)")
	}
	if *allowLoopbackEnroll {
		log.Print("relay: 직접 loopback 호스트 등록 무키 허용 (리버스 프록시 환경에서는 사용 금지)")
	}

	relayHandler := relay.NewServerOpts(opts)
	srv := &http.Server{
		Addr: *addr, Handler: relayHandler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	if *mobilePublicURL == "" {
		log.Print("Pie Relay 모바일 공개 URL 미지정: 요청 Host를 사용합니다 (운영 환경에서는 RELAY_PUBLIC_URL 권장)")
	} else {
		log.Printf("Pie Relay 모바일 활성화: %s", *mobilePublicURL)
	}
	if *mobileStateFile == "" {
		log.Print("Pie Relay 모바일 상태 파일 미지정: 서버 재시작 시 모바일 재페어링 필요")
	} else {
		log.Printf("Pie Relay 모바일 해시 상태 저장: %s", *mobileStateFile)
	}
	log.Printf("Pie Relay listening on %s (/ws/agent, /ws/participant, /v1/*)", *addr)
	if *tlsCert != "" && *tlsKey == "" {
		log.Fatal("--tls-key is required when --tls-cert is set")
	}

	errCh := make(chan error, 1)
	go func() {
		if *tlsCert != "" {
			errCh <- srv.ListenAndServeTLS(*tlsCert, *tlsKey)
			return
		}
		errCh <- srv.ListenAndServe()
	}()

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if presenceReporter != nil && *poolID != "" && len(allowedApplicationValues) > 0 && *mobilePublicURL != "" {
		reportRelayNode(presenceReporter, *nodeID, *poolID, allowedApplicationValues, *mobilePublicURL, true)
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-signalCtx.Done():
					return
				case <-ticker.C:
					reportRelayNode(presenceReporter, *nodeID, *poolID, allowedApplicationValues, *mobilePublicURL, true)
				}
			}
		}()
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
		return
	case <-signalCtx.Done():
		log.Printf("relay: drain 시작 node=%s active=%d", *nodeID, relayHandler.ActiveConnections())
	}
	if presenceReporter != nil && *poolID != "" && len(allowedApplicationValues) > 0 && *mobilePublicURL != "" {
		reportRelayNode(presenceReporter, *nodeID, *poolID, allowedApplicationValues, *mobilePublicURL, false)
	}

	relayHandler.BeginDrain()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	_ = srv.Shutdown(shutdownCtx) // stop new HTTP/TCP accepts; websocket peers are drained below
	cancelShutdown()

	drainDeadline := time.Now().Add(20 * time.Second)
	for relayHandler.ActiveConnections() > 0 && time.Now().Before(drainDeadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if remaining := relayHandler.ActiveConnections(); remaining > 0 {
		log.Printf("relay: drain 제한시간 만료, %d 연결 종료", remaining)
		relayHandler.CloseActiveConnections()
	}
	log.Printf("relay: 종료 완료 node=%s", *nodeID)
}

func reportRelayNode(reporter *relay.HTTPPresenceReporter, nodeID, poolID string, applicationIDs []string, publicURL string, online bool) {
	now := time.Now().UTC()
	for _, applicationID := range applicationIDs {
		reporter.Report(relay.PresenceEvent{
			EventID: fmt.Sprintf("node-%s-%d-%s", nodeID, now.UnixNano(), applicationID),
			NodeID:  nodeID, ApplicationID: applicationID, PoolID: poolID, PublicURL: publicURL,
			ConnectionID: "node:" + nodeID, Kind: "node", Connected: online, Heartbeat: online, At: now,
		})
	}
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		log.Fatalf("%s must be a positive integer, got %q", name, raw)
	}
	return v
}

func envFloat(name string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		log.Fatalf("%s must be a positive number, got %q", name, raw)
	}
	return v
}

func envBool(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		log.Fatalf("%s must be true or false, got %q", name, raw)
	}
	return value
}

func envList(name string, fallback []string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return append([]string(nil), fallback...)
	}
	values := make([]string, 0, strings.Count(raw, ",")+1)
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func validateSecret(name, value string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
	if len([]byte(value)) < 32 {
		return fmt.Errorf("%s must be at least 32 bytes", name)
	}
	return nil
}

func defaultNodeID() string {
	if configured := strings.TrimSpace(os.Getenv("RELAY_NODE_ID")); configured != "" {
		return configured
	}
	host, err := os.Hostname()
	if err == nil && host != "" {
		return host
	}
	return "pie-relay-node"
}
