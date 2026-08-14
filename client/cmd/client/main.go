// Command client runs the cli-relay local daemon.
//
// Client-side component (runs on YOUR machine): starts the Node executor
// (node-executor/executor.mjs, which drives the local `claude` CLI headless)
// and bridges it to the relay's /ws/agent leg. Payloads are relayed verbatim;
// the CLI runs here, on your machine — the relay never sees your files.
//
// Auth (env or flags):
//
//	client            — normal run. Prefers a host token from --ticket/RELAY_TICKET
//	                     (issued by the desktop app's "방 만들기"). If no ticket is
//	                     given it falls back to ~/.cli-relay/credentials.json — a
//	                     legacy/manual artifact whose accessToken is used verbatim,
//	                     with no refresh. If neither is present the daemon prints a
//	                     "먼저 방을 만드세요" hint and exits. On a 401 at the handshake
//	                     the token is expired or revoked: re-enroll from the app to
//	                     get a fresh one.
//
// Config (env or flags):
//
//	PIE_RELAY_URL Relay origin                  (default: CookAI Relay)
//	RELAY_TICKET  host token (primary auth) — bypasses credentials.json (optional)
//	EXECUTOR_PATH path to executor.mjs           (default: ./node-executor/executor.mjs
//	                                              resolved next to the binary, then cwd)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cli-relay/client/internal/chatagent"
	"cli-relay/client/internal/credentials"
	"cli-relay/client/internal/ptyagent"
	"cli-relay/client/internal/rooms"
	"cli-relay/client/internal/tui"
)

// enrollHint is printed whenever the daemon has no host token to present. The
// desktop app's "방 만들기" flow issues one via the relay's /host/enroll and
// stores it as the manual ticket.
const enrollHint = "호스트 토큰이 없습니다 — 먼저 데스크톱 앱에서 '방 만들기'로 호스트 토큰을 발급하세요 (RELAY_TICKET 로 전달됩니다)."

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func resolveExecutorPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return resolveBundledPath("node-executor/executor.mjs")
}

func resolveACPExecutorPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return resolveBundledPath("node-executor/acp-executor.mjs")
}

// resolvePTYHostPath mirrors resolveExecutorPath but for the terminal-room
// host (node-executor/pty-host.mjs). An explicit override wins; otherwise it
// resolves next to the binary, then falls back to a cwd-relative path.
func resolvePTYHostPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return resolveBundledPath("node-executor/pty-host.mjs")
}

func resolveBundledPath(rel string) string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, base := range []string{dir, filepath.Dir(dir)} {
			candidate := filepath.Join(base, rel)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		for _, base := range []string{cwd, filepath.Join(cwd, "client")} {
			candidate := filepath.Join(base, rel)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return rel
}

// runDaemon presents a host token to the relay: --ticket/RELAY_TICKET is the
// primary path; absent that it falls back to the accessToken in
// credentials.json (legacy/manual, no refresh). With neither, it prints the
// enroll hint and exits. A 401 at the handshake means the token is expired or
// revoked — the host must re-enroll from the desktop app.
// watchParent, if CLI_RELAY_APP_PID is set, polls whether that pid is still
// alive and calls stop() (graceful shutdown) once it dies. Signal 0 probes a
// pid without affecting it: nil = alive, ESRCH = gone. No env → no-op.
func watchParent(ctx context.Context, stop func()) {
	v := os.Getenv("CLI_RELAY_APP_PID")
	if v == "" {
		return
	}
	pid, err := strconv.Atoi(v)
	if err != nil || pid <= 1 {
		return
	}
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !pidAlive(pid) {
					// ESRCH (or any non-permission error): the app is gone.
					log.Printf("cli-relay client: 부모 앱(pid=%d) 종료 감지 — 데몬을 종료합니다", pid)
					stop()
					return
				}
			}
		}
	}()
}

func runDaemon() {
	relayURL := flag.String("relay-url", relayEnvironmentURL(), "relay origin/agent endpoint (env: PIE_RELAY_URL; default: CookAI Relay)")
	ticket := flag.String("ticket", os.Getenv("RELAY_TICKET"), "호스트 토큰 (주 경로) — 지정하면 credentials.json 을 무시한다 (env: RELAY_TICKET)")
	executorPath := flag.String("executor", envOr("EXECUTOR_PATH", ""), "path to node-executor/executor.mjs (env: EXECUTOR_PATH)")
	acpExecutorPath := flag.String("acp-executor", envOr("ACP_EXECUTOR_PATH", ""), "path to node-executor/acp-executor.mjs (env: ACP_EXECUTOR_PATH)")
	ptyHostPath := flag.String("pty-host", envOr("PTY_HOST_PATH", ""), "path to node-executor/pty-host.mjs, terminal 모드 전용 (env: PTY_HOST_PATH)")
	flag.Parse()

	// Room mode selects which local agent the daemon supervises. Default (env
	// unset) is the SDK chat executor — unchanged behavior. "terminal" swaps in
	// the PTY host (pty-host.mjs) and the ptyagent byte bridge. Both share the
	// same relay agent leg, connectFunc signature, and token, so only the
	// supervised process + resolved path differ.
	roomMode := os.Getenv("CLI_RELAY_ROOM_MODE")
	terminalMode := roomMode == "terminal"
	runFn := chatagent.Run
	agentPath := resolveExecutorPath(*executorPath)
	if terminalMode {
		runFn = ptyagent.Run
		agentPath = resolvePTYHostPath(*ptyHostPath)
	} else if roomMode == "acp" {
		agentPath = resolveACPExecutorPath(*acpExecutorPath)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Die-with-parent: the desktop app spawns this daemon as a Tauri sidecar and
	// passes its own pid in CLI_RELAY_APP_PID. A sidecar is NOT killed when the
	// app is force-quit (a dev rebuild's SIGKILL, a crash) — it reparents to init
	// (PPID=1) and keeps its relay ws open, so the app shows "정지됨" while an
	// orphan stays registered as the room's host and participants keep seeing
	// "호스트 연결". Watch that pid and shut down gracefully (stop() → executor
	// cleanup) once it's gone. Only active when the env is set (sidecar launch);
	// standalone/backgrounded runs are unaffected.
	watchParent(ctx, stop)

	credPath, err := credentials.DefaultPath()
	if err != nil {
		log.Fatalf("자격증명 경로 확인 실패: %v", err)
	}

	usingCredentials := *ticket == ""
	token := *ticket
	if usingCredentials {
		creds, err := credentials.LoadFrom(credPath)
		if err != nil {
			log.Fatal(enrollHint)
		}
		if creds.AccessToken == "" {
			log.Fatal(enrollHint)
		}
		token = creds.AccessToken
	}
	resolvedRelay, err := resolveRelayEndpoint(*relayURL)
	if err != nil {
		log.Fatalf("Relay URL 확인 실패: %v", err)
	}
	*relayURL = resolvedRelay

	// 인증 모드를 명시적으로 남긴다 — GUI 사이드카 로그에서 "티켓을 넣었는데
	// credentials 로 붙는" 류의 전달 문제를 한 줄로 판별하기 위함.
	authMode := "호스트 토큰(RELAY_TICKET)"
	if usingCredentials {
		authMode = "credentials.json(" + credPath + ")"
	}
	modeDesc := "chat(SDK)"
	if terminalMode {
		modeDesc = "terminal(PTY)"
	} else if roomMode == "acp" {
		modeDesc = "ACP agent"
	}
	log.Printf("Pie Relay client: mode=%s agent=%s → relay=%s · 인증=%s", modeDesc, agentPath, *relayURL, authMode)
	err = runConnect(ctx, *relayURL, agentPath, token, runFn)
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		log.Print("cli-relay client: shut down")
	case errors.Is(err, chatagent.ErrUnauthorized):
		if usingCredentials {
			log.Fatalf("relay 인증 실패: %v — credentials.json 의 토큰이 만료·폐기되었습니다. 데스크톱 앱에서 '방 만들기'로 호스트 토큰을 재발급하세요", err)
		} else {
			log.Fatalf("relay 인증 실패: %v — 호스트 토큰(--ticket/RELAY_TICKET)이 거부되었습니다. 데스크톱 앱에서 '방 만들기'로 호스트 토큰을 재발급하세요", err)
		}
	default:
		log.Fatalf("cli-relay client: %v", err)
	}
}

// connectFunc matches chatagent.Run / ptyagent.Run's signature. runConnect
// takes it as a parameter (rather than calling chatagent.Run directly) so tests
// can inject a stub without dialing a real WebSocket or spawning the executor
// process.
type connectFunc func(ctx context.Context, relayURL, executorPath, token string) error

// runConnect runs the connect func once with the resolved host token and
// surfaces its error verbatim. There is no longer a refresh/re-auth retry: the
// browser-login flow that produced refreshable credentials is gone, so on a 401
// the host re-enrolls from the desktop app rather than the daemon refreshing.
func runConnect(ctx context.Context, relayURL, executorPath, token string, run connectFunc) error {
	return run(ctx, relayURL, executorPath, token)
}

// runRoomCreate implements `client room create`: it reads the saved host token
// (credentials.json), derives the relay's HTTP base from PIE_RELAY_URL (or an
// explicit --relay-http), and asks the relay to mint an invite code for the
// host's room. The desktop app issues invites over HTTP directly with the
// enrolled host token; this CLI path is the credentials.json fallback.
func runRoomCreate(args []string) {
	fs := flag.NewFlagSet("room create", flag.ExitOnError)
	relayURL := fs.String("relay-url", relayEnvironmentURL(), "relay origin/agent endpoint (env: PIE_RELAY_URL; default: CookAI Relay)")
	relayHTTP := fs.String("relay-http", "", "relay HTTP base http(s)://host (optional; overrides --relay-url derivation)")
	_ = fs.Parse(args)
	resolvedRelay, err := resolveRelayEndpoint(*relayURL)
	if err != nil {
		log.Fatalf("Relay URL 확인 실패: %v", err)
	}
	*relayURL = resolvedRelay

	base := *relayHTTP
	if base == "" {
		b, err := rooms.HTTPBase(*relayURL)
		if err != nil {
			log.Fatalf("relay HTTP 주소 확인 실패: %v", err)
		}
		base = b
	}

	credPath, err := credentials.DefaultPath()
	if err != nil {
		log.Fatalf("자격증명 경로 확인 실패: %v", err)
	}
	creds, err := credentials.LoadFrom(credPath)
	if err != nil || creds.AccessToken == "" {
		log.Fatal(enrollHint)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := rooms.CreateInvite(ctx, base, creds.AccessToken)
	if err != nil {
		log.Fatalf("초대 코드 발급 실패: %v", err)
	}
	exp := time.Unix(res.ExpiresAt, 0).Local().Format("15:04:05")
	fmt.Printf("초대 코드: %s\n만료: %s 까지 (약 %d분)\n참가: client join %s --name <이름>\n",
		res.Code, exp, int(time.Until(time.Unix(res.ExpiresAt, 0)).Minutes()), res.Code)
}

// runJoin implements `client join <code> [--name] [--relay-url]`: it exchanges
// the invite code for a participant token (no host credentials needed) and
// enters the Bubble Tea chat UI over /ws/participant.
func runJoin(args []string) {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	name := fs.String("name", "", "표시 이름 (게스트 식별용)")
	relayURL := fs.String("relay-url", relayEnvironmentURL(), "relay origin/agent endpoint (env: PIE_RELAY_URL; default: CookAI Relay)")
	_ = fs.Parse(args)
	resolvedRelay, err := resolveRelayEndpoint(*relayURL)
	if err != nil {
		log.Fatalf("Relay URL 확인 실패: %v", err)
	}
	*relayURL = resolvedRelay

	code := strings.TrimSpace(fs.Arg(0))
	if code == "" {
		log.Fatal("초대 코드가 필요합니다 — 사용법: client join <code> [--name 이름]")
	}

	base, err := rooms.HTTPBase(*relayURL)
	if err != nil {
		log.Fatalf("relay HTTP 주소 확인 실패: %v", err)
	}
	wsURL, err := rooms.ParticipantWSURL(*relayURL)
	if err != nil {
		log.Fatalf("relay participant 주소 확인 실패: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := rooms.Join(ctx, base, code, *name)
	if err != nil {
		log.Fatalf("방 참가 실패: %v", err)
	}

	myName := *name
	if myName == "" {
		myName = "나"
	}
	if err := tui.Run(ctx, wsURL, res.Token, myName); err != nil {
		log.Fatalf("채팅 종료: %v", err)
	}
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "help", "-h", "--help":
			printClientUsage()
			return
		case "version", "--version":
			runClientVersion(os.Args[2:])
			return
		case "connect":
			runDeviceConnect(os.Args[2:])
			return
		case "start":
			runDeviceStart(os.Args[2:])
			return
		case "stop":
			runDeviceStop(os.Args[2:])
			return
		case "status":
			runDeviceStatus(os.Args[2:])
			return
		case "disconnect":
			runDeviceDisconnect(os.Args[2:])
			return
		case "pair":
			runDevicePair(os.Args[2:])
			return
		case "sessions":
			if len(os.Args) > 2 && os.Args[2] == "serve" {
				runSessionManager(os.Args[3:])
				return
			}
			if len(os.Args) > 2 && os.Args[2] == "request" {
				runSessionRequest(os.Args[3:])
				return
			}
			log.Fatal("사용법: client sessions serve|request")
		case "room":
			// Only `room create` exists today; keep the dispatch explicit so an
			// unknown subcommand fails loudly rather than silently starting the daemon.
			if len(os.Args) > 2 && os.Args[2] == "create" {
				runRoomCreate(os.Args[3:])
				return
			}
			log.Fatal("사용법: client room create [--relay-url URL | --relay-http URL]")
		case "join":
			runJoin(os.Args[2:])
			return
		default:
			if !strings.HasPrefix(os.Args[1], "-") {
				fmt.Fprintf(os.Stderr, "알 수 없는 Pie Client 명령: %s\n\n", os.Args[1])
				printClientUsage()
				os.Exit(2)
			}
		}
	}
	runDaemon()
}

func printClientUsage() {
	fmt.Println(`Pie Client

사용법:
  pie-client version [--json]                    설치 버전과 빌드 정보 확인
  pie-client connect --server URL --code CODE  최초 연결 또는 기존 장치 재연결 후 즉시 실행
  pie-client start                              연결된 장치 실행
  pie-client stop                               정상 종료
  pie-client status [--json]                    연결·실행 상태 확인
  pie-client disconnect [--local-only]          장치 연결과 로컬 자격 해제

호환 명령:
  pie-client pair                               페어링만 수행
  pie-client sessions serve|request             Session Manager 저수준 명령
  pie-client room create | join                 기존 Relay 방 명령`)
}
