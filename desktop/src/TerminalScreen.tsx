import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import type { Session } from "./App";
import type { RoomStatus } from "./ChatScreen";
import {
  parseTerminalEvent,
  parseServerEvent,
  buildPtyInput,
  buildPtyScroll,
  buildPtyResize,
  buildSetDriver,
  buildRequestScreen,
  buildRoomChat,
  buildDriverHeartbeat,
  buildDriverRequest,
  base64ToBytes,
  isScrollSeq,
  shouldSendInput,
  keyToSequence,
  cursorPx,
  subFromToken,
  accessFromToken,
  speakerName,
  type RoomMode,
} from "./protocol";
import { RelayConnection, type ConnStatus } from "./connection";
import { participantTicketProtocol, participantWsEndpoint } from "./relay";
import { TerminalStreamTracker } from "./terminal-stream";
import { TerminalOutputScheduler } from "./terminal-output-scheduler";
import {
  readTermFont,
  writeTermFont,
  onTermFontChange,
  TERM_FONTS,
  TERM_SIZES,
  type TermFont,
} from "./termfont";
import { UiIcon } from "./UiIcon";

// .term-host padding (6px 8px) — the origin cell (0,0) sits at this offset inside
// .term-wrap, so the IME overlay's pixel position must add it (see cursorPx).
const TERM_PAD_L = 8;
const TERM_PAD_T = 6;
const UTF8_ENCODER = new TextEncoder();
// A few cells of width for the inline composition; overflow:visible lets a longer
// composition/candidate string spill past it rather than clip.
const INPUT_MIN_W = 120;

// SideChatMsg is one line of the human-to-human side chat: a stable id (from a
// monotonic ref counter — never a Date.now()/random collision), the verified
// sub of the sender (or my own for my optimistic line), the text, and whether
// it is mine. Ephemeral — never persisted; lives only in component state.
interface SideChatMsg {
  id: string;
  from: string;
  text: string;
  mine: boolean;
}

interface Props {
  session: Session;
  onLeave: () => void;
  // Same multi-room contract as ChatScreen: `hidden` only toggles display:none —
  // it must NEVER close the socket or dispose the terminal (the connection and
  // xterm instance are owned solely by the mount effect below, so a background
  // terminal room keeps streaming and holds its scrollback). onActivity fires
  // only while hidden so the shell can raise an unread badge; onStatus mirrors
  // host presence / socket liveness up to the sidebar; onMode confirms the room
  // classification when the host (re)announces room_mode.
  hidden?: boolean;
  onActivity?: () => void;
  onStatus?: (status: RoomStatus) => void;
  onMode?: (mode: RoomMode) => void;
}

// TerminalScreen is the terminal-room counterpart of ChatScreen: it owns one
// relay socket and an xterm.js terminal, streams the host PTY, and (for the
// host operator) exposes the driver-control UI. Frame meaning lives in
// protocol.ts (pure parsers/builders); this component wires the socket to
// xterm and enforces the local driver gate.
export function TerminalScreen({
  session,
  onLeave,
  hidden = false,
  onActivity,
  onStatus,
  onMode,
}: Props) {
  const [status, setStatus] = useState<ConnStatus>("connecting");
  const [hostUp, setHostUp] = useState(false);
  const [driver, setDriver] = useState(""); // current driver sub, "" = view-only
  const [observed, setObserved] = useState<string[]>([]); // subs seen as drivers
  const [viewHint, setViewHint] = useState(false); // "보기 전용" flash on blocked key
  const [terminalGeneration, setTerminalGeneration] = useState(0);
  // Terminal font/size — one setting shared by every room (persisted in
  // localStorage, synced live across mounted TerminalScreens via a window event).
  // Seeded from storage so a re-mounted / newly-opened room honours the choice.
  const [font, setFont] = useState<TermFont>(() => readTermFont());

  // Side-chat (human↔human) panel state. Messages are ephemeral and separate
  // from the PTY stream entirely (they never touch the shell). unread counts
  // messages that arrived while the panel was closed or the room backgrounded.
  const [chatMsgs, setChatMsgs] = useState<SideChatMsg[]>([]);
  const [panelOpen, setPanelOpen] = useState(false);
  const [unread, setUnread] = useState(0);
  const [chatDraft, setChatDraft] = useState("");

  // My verified identity (JWT sub). Used for the driver gate and shown so I can
  // tell the operator my id — there is no participant roster on the wire.
  const mySub = useMemo(() => subFromToken(session.token), [session.token]);
  const myAccess = useMemo(
    () => accessFromToken(session.token),
    [session.token],
  );

  const containerRef = useRef<HTMLDivElement | null>(null);
  // The IME input overlay: a plain textarea that owns ALL keyboard input for the
  // terminal. In Tauri's WKWebView xterm's own hidden textarea never engages IME
  // composition, so we take input through this normal textarea (which composes
  // Hangul/CJK correctly) and forward it to the PTY; xterm is display-only.
  const taRef = useRef<HTMLTextAreaElement | null>(null);
  // The composing-text preview: an OPAQUE element drawn over the textarea at the
  // cursor. WebKit paints its own blue "marked text" highlight behind the
  // composing glyph in the textarea and that can't be styled away with CSS —
  // so we hide the textarea's own text and render the composing string here,
  // on top, with an opaque terminal-background so the blue is fully covered.
  const composeRef = useRef<HTMLDivElement | null>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const connRef = useRef<RelayConnection | null>(null);
  // driverRef mirrors `driver` so sendKey / fitAndSend (bound once) always read
  // the freshest value without re-subscribing on every driver change.
  const driverRef = useRef("");
  // ptySizeRef holds the authoritative PTY grid size announced by the host (via
  // the pty_size frame + ~2s heartbeat). A NON-driver renders its xterm at THIS
  // exact cols/rows and scales-to-fit (letterbox) instead of re-fitting to its
  // own container — otherwise the pre-wrapped byte stream re-wraps at the wrong
  // width and garbles (words split mid-token). null until the first pty_size.
  const ptySizeRef = useRef<{ cols: number; rows: number } | null>(null);
  const resizeTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const hintTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const didFirstFit = useRef(false);
  // fontRef mirrors the latest font so the mount effect (bound once) seeds the
  // terminal with the persisted choice without listing `font` as a dependency
  // (that would violate the connection keep-alive invariant).
  const fontRef = useRef(font);
  // hiddenRef / activityRef mirror the latest hidden flag and unread callback so
  // the socket's onMessage closure (bound once) reads fresh values — the unread
  // badge must reflect whether the room is backgrounded *now*, not at mount.
  const hiddenRef = useRef(hidden);
  const activityRef = useRef(onActivity);
  // Side-chat closure mirrors: the bound-once socket onMessage reads these to
  // decide whether an incoming room_chat should raise the unread badge, without
  // re-subscribing (that would violate the keep-alive invariant). msgIdRef is a
  // simple monotonic counter for collision-free message ids.
  const panelOpenRef = useRef(panelOpen);
  const chatListRef = useRef<HTMLDivElement | null>(null);
  const msgIdRef = useRef(0);
  const nextMsgId = () => `sc${(msgIdRef.current += 1)}`;

  useEffect(() => {
    panelOpenRef.current = panelOpen;
  }, [panelOpen]);

  useEffect(() => {
    driverRef.current = driver;
  }, [driver]);

  useEffect(() => {
    hiddenRef.current = hidden;
    activityRef.current = onActivity;
  }, [hidden, onActivity]);

  // Report host presence / socket liveness to the shell (sidebar entry). App
  // ignores no-op updates, so running on every change cannot loop.
  useEffect(() => {
    onStatus?.({ hostUp, live: status === "open" });
  }, [hostUp, status, onStatus]);

  // Position the IME input overlay over the current cursor cell so composing
  // Hangul/CJK previews inline (and the OS candidate window opens beside the
  // cursor, not the top-left corner). Imperative (direct style writes) because
  // onCursorMove fires per keystroke — cheaper than a React re-render. Measures
  // the real cell size from the rendered .xterm-screen; skips before layout
  // (cellW/H === 0) and parks the overlay at the top-left when the cursor is
  // scrolled out of the viewport (scrollback), so it never floats mid-screen.
  const updateInputPos = useCallback(() => {
    const term = termRef.current;
    const ta = taRef.current;
    if (!term || !ta || !term.element) return;
    const screen = term.element.querySelector(
      ".xterm-screen",
    ) as HTMLElement | null;
    if (!screen) return;
    const { cols, rows } = term;
    const cellW = screen.clientWidth / cols;
    const cellH = screen.clientHeight / rows;
    if (!cellW || !cellH) return; // not laid out yet — leave the last position
    const buf = term.buffer.active;
    // cursorX/cursorY are viewport-relative; when scrolled back, viewportY moves
    // off the cursor's absolute row (baseY + cursorY), so re-project onto the
    // visible grid and hide (park) if it falls outside it.
    const col = buf.cursorX;
    const row = buf.cursorY + (buf.baseY - buf.viewportY);
    const inView = col >= 0 && col < cols && row >= 0 && row < rows;
    const { left, top } = inView
      ? cursorPx({
          cursorX: col,
          cursorY: row,
          cellW,
          cellH,
          padL: TERM_PAD_L,
          padT: TERM_PAD_T,
        })
      : { left: TERM_PAD_L, top: TERM_PAD_T };
    ta.style.left = `${left}px`;
    ta.style.top = `${top}px`;
    ta.style.width = `${Math.max(cellW * 8, INPUT_MIN_W)}px`;
    ta.style.height = `${cellH}px`;
    ta.style.lineHeight = `${cellH}px`;
    // Keep the composing-preview box on the same cell as the textarea/cursor.
    const cp = composeRef.current;
    if (cp) {
      cp.style.left = `${left}px`;
      cp.style.top = `${top}px`;
      cp.style.height = `${cellH}px`;
      cp.style.lineHeight = `${cellH}px`;
    }
  }, []);

  // Fit-and-send: refit the terminal to its container, then (debounced, and only
  // while I am the driver) tell the host the new size. Non-drivers still refit
  // locally so their view is legible; only the driver's size drives the PTY.
  const fitAndSend = useCallback(() => {
    const fit = fitRef.current;
    const term = termRef.current;
    if (!fit || !term) return;
    try {
      fit.fit();
    } catch {
      /* zero-size container (e.g. hidden) — ignore */
    }
    // A fit changes cell size / cols / rows, so reposition the overlay to match.
    updateInputPos();
    if (resizeTimer.current) clearTimeout(resizeTimer.current);
    resizeTimer.current = setTimeout(() => {
      if (shouldSendInput(driverRef.current, mySub)) {
        connRef.current?.send(buildPtyResize(term.cols, term.rows));
      }
    }, 120);
  }, [mySub, updateInputPos]);

  // applyViewerLayout renders a NON-driver's xterm at the PTY's authoritative
  // cols/rows and scales-to-fit (letterbox) instead of re-fitting. The PTY is
  // wrapped to the driver's width, so a viewer that fit()s to its own (different)
  // container width would re-wrap the pre-wrapped bytes at the wrong column and
  // garble text. Instead we resize the grid to the PTY size and CSS-scale the
  // whole .term-host down so the (possibly wider) grid fits, leaving empty
  // letterbox space at the right/bottom (transform-origin: top left). If the PTY
  // size isn't known yet (null), do nothing — the ~2s heartbeat will deliver it.
  //
  // The clientWidth/clientHeight measurement below reads layout, so it must
  // happen AFTER the browser has actually laid the resized grid out — reading
  // it synchronously right after term.resize() can observe stale (pre-resize)
  // dimensions. Mirror the mount effect's double-rAF trick (see the comment
  // near the "Defer to the next frame(s)" mount code below) with a single rAF,
  // which is enough here since resize() itself already forces a reflow.
  const applyViewerLayout = useCallback(() => {
    const term = termRef.current;
    const host = containerRef.current;
    if (!term || !host || !term.element) return;
    const size = ptySizeRef.current;
    if (!size) return; // heartbeat will deliver it
    // Match the PTY grid exactly (guard against a redundant resize → no churn).
    if (term.cols !== size.cols || term.rows !== size.rows) {
      try {
        term.resize(size.cols, size.rows);
      } catch {
        /* transient measure/render race — ignore */
      }
    }
    updateInputPos();
    requestAnimationFrame(() => {
      // Re-check: by the time this frame runs, the driver flag, PTY size, or
      // the mounted term/host could have changed (driver switch, unmount).
      const term2 = termRef.current;
      const host2 = containerRef.current;
      if (!term2 || !host2 || !term2.element) return;
      if (shouldSendInput(driverRef.current, mySub)) return; // became the driver meanwhile
      // Measure the rendered grid px (same technique as updateInputPos) and the
      // container px, then scale down so the whole grid fits (never scale up).
      const screen = term2.element.querySelector(
        ".xterm-screen",
      ) as HTMLElement | null;
      if (!screen) return;
      const gridW = screen.clientWidth;
      const gridH = screen.clientHeight;
      if (!gridW || !gridH) return; // not laid out yet
      const containerW = host2.clientWidth;
      const containerH = host2.clientHeight;
      if (!containerW || !containerH) return;
      const rawScale = Math.min(containerW / gridW, containerH / gridH, 1);
      // Round to a coarse step (2 decimal places) rather than an arbitrary
      // float — a non-integer scale sub-pixel-blurs glyph rendering, and
      // snapping to hundredths removes most of the jitter while keeping the
      // letterbox gap this introduces negligible. Round DOWN so we never
      // scale past fitting (never overflow the container).
      const scale = Math.floor(rawScale * 100) / 100;
      host2.style.transformOrigin = "top left";
      host2.style.willChange = "transform";
      host2.style.transform = `scale(${scale})`;
    });
  }, [updateInputPos, mySub]);

  // applyLayout is the single layout dispatcher used by every layout trigger
  // (resize, visibility, font, driver change, first output). The driver keeps
  // fitting its container and driving the PTY (fitAndSend); every non-driver
  // letterboxes to the PTY's exact size (applyViewerLayout). Reads driver via
  // driverRef so the bound-once mount effect can call it with a fresh value.
  const applyLayout = useCallback(() => {
    if (shouldSendInput(driverRef.current, mySub)) {
      // I'm the driver: drop any letterbox scale left over from being a viewer,
      // then fit my container and resize the PTY as before.
      const host = containerRef.current;
      if (host) host.style.transform = "";
      fitAndSend();
    } else {
      applyViewerLayout();
    }
  }, [mySub, fitAndSend, applyViewerLayout]);

  // sendKey transmits one chunk of input (a keystroke sequence, a composed
  // string, or pasted text), but only while I am the driver (mirrors the host's
  // from===driver gate); otherwise it flashes a view-only hint and drops it, so a
  // spectator never wastes a relay round-trip. Empty payloads are no-ops.
  const sendKey = useCallback(
    (data: string) => {
      if (!data) return;
      if (shouldSendInput(driverRef.current, mySub)) {
        connRef.current?.send(buildPtyInput(data));
      } else {
        setViewHint(true);
        if (hintTimer.current) clearTimeout(hintTimer.current);
        hintTimer.current = setTimeout(() => setViewHint(false), 1500);
      }
    },
    [mySub],
  );

  // Apply a font/size to the live terminal: swap xterm's options, keep the IME
  // overlay's font in step (its box position is re-measured from the rendered
  // .xterm-screen, so only the glyph font needs matching), then refit so the
  // grid and cursor are recomputed for the new cell metrics. Runs regardless of
  // didFirstFit — a font change must re-fit immediately. Nothing else about the
  // connection / driver gate / scrollback is touched.
  const applyFont = useCallback(
    (f: TermFont) => {
      fontRef.current = f;
      const term = termRef.current;
      if (term) {
        term.options.fontFamily = f.family;
        term.options.fontSize = f.size;
        // A font change alters cell metrics — re-lay-out (driver: fit+resize PTY;
        // viewer: re-measure the grid px and re-scale the letterbox).
        applyLayout();
      }
      const ta = taRef.current;
      if (ta) {
        ta.style.fontFamily = f.family;
        ta.style.fontSize = `${f.size}px`;
      }
      // Keep the composing preview glyphs matching the terminal font too.
      const cp = composeRef.current;
      if (cp) {
        cp.style.fontFamily = f.family;
        cp.style.fontSize = `${f.size}px`;
      }
    },
    [applyLayout],
  );

  // Re-apply whenever the shared selection changes (initial mount is a no-op
  // since the terminal was already seeded from the same value via fontRef).
  useEffect(() => {
    applyFont(font);
  }, [font, applyFont]);

  // Live sync: adopt a selection made from any room's picker. writeTermFont
  // broadcasts, so this room's own picker also lands here (single update path).
  useEffect(() => onTermFontChange(setFont), []);

  // A backgrounded terminal room is display:none, so FitAddon can't measure it.
  // When it becomes visible again, re-lay-out to the now-laid-out container (a
  // driver refits + resyncs the PTY; a viewer re-letterboxes) — otherwise it
  // keeps a stale grid / scale.
  useEffect(() => {
    if (!hidden) requestAnimationFrame(() => applyLayout());
  }, [hidden, applyLayout]);

  // Keep keyboard focus on the input overlay whenever this room is the visible
  // one, so typing lands in the terminal without an extra click. Never focus
  // while hidden — that would steal focus from the foreground room.
  useEffect(() => {
    if (!hidden) taRef.current?.focus();
  }, [hidden]);

  // Create the terminal + socket once per session; tear both down on unmount /
  // leave. This is the ONLY place the socket or xterm lifecycle is touched.
  useEffect(() => {
    const term = new Terminal({
      cursorBlink: true,
      scrollback: 10_000,
      lineHeight: 1.08,
      // Seed from the persisted selection. Default is bundled D2Coding, which is
      // fixed-width for BOTH Latin and Hangul/CJK so Korean and ASCII columns
      // align; the bytes arrive correct on the wire and D2Coding has the glyphs.
      // The user can switch font/size at runtime (see applyFont / TermFontMenu).
      fontSize: fontRef.current.size,
      fontFamily: fontRef.current.family,
      theme: {
        background: "#1d2126",
        foreground: "#d8dde5",
        cursor: "#f4f6f8",
        cursorAccent: "#1d2126",
        selectionBackground: "#355d8588",
        black: "#171a1f",
        brightBlack: "#666d77",
        red: "#f07178",
        brightRed: "#ff8b92",
        green: "#a7c080",
        brightGreen: "#b7d38d",
        yellow: "#dbbc7f",
        brightYellow: "#e8c98d",
        blue: "#7fbbb3",
        brightBlue: "#8ec9c1",
        magenta: "#d699b6",
        brightMagenta: "#e3a6c3",
        cyan: "#83c092",
        brightCyan: "#93d0a2",
        white: "#d8dde5",
        brightWhite: "#f4f6f8",
      },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    if (containerRef.current) term.open(containerRef.current);
    termRef.current = term;
    fitRef.current = fit;
    const streamTracker = new TerminalStreamTracker();
    const requestSnapshot = () => {
      connRef.current?.send(buildRequestScreen(streamTracker.resumeCursor()));
    };
    const outputScheduler = new TerminalOutputScheduler(term, {
      onOverflow: () => {
        term.reset();
        requestSnapshot();
      },
      onStall: () => {
        // A missing xterm write callback means its parser pipeline wedged. Reset
        // the entire xterm/socket mount; reusing the same internal WriteBuffer can
        // remain wedged even after reset(). The new mount requests a snapshot.
        setTerminalGeneration((generation) => generation + 1);
      },
    });
    // Keep the IME overlay glued to the cursor cell: every cursor move (each
    // keystroke, each line of shell output) repositions it so composition
    // previews inline. fit/resize repositioning is handled inside fitAndSend.
    term.onCursorMove(() => updateInputPos());
    // First fit only measures correctly once the container has laid out and the
    // monospace font metrics are ready — a synchronous fit here usually lands on
    // xterm's default 80 cols. Defer to the next frame(s), then sync the size to
    // the PTY (fitAndSend sends only while driver). If the font loads later,
    // refit again so the grid matches the real cell size.
    requestAnimationFrame(() => {
      applyLayout();
      requestAnimationFrame(applyLayout);
    });
    if (document.fonts?.ready) {
      document.fonts.ready.then(() => applyLayout()).catch(() => {});
    }

    // KEYBOARD is not taken from xterm — the .term-input overlay owns it (see
    // taRef) so IME composition works in WKWebView, and xterm's helper textarea
    // stays unfocused (so xterm never emits key data via onData).
    //
    // MOUSE / WHEEL, however, must be forwarded: full-screen apps (Claude Code,
    // vim, less, htop) put the terminal in the ALTERNATE screen, where the wheel
    // does NOT scroll a local scrollback. Depending on the app, xterm encodes the
    // wheel as either a mouse-report sequence (CSI M / CSI < …, when the app turns
    // on mouse tracking) OR arrow-key sequences (alternate-scroll mode). If we drop
    // these, scrolling the app does nothing — the reported bug. So we forward every
    // xterm-generated ESC-prefixed sequence (mouse reports, alt-scroll arrows,
    // other control) to the PTY, gated on the driver. We deliberately forward ONLY
    // ESC-prefixed data: plain typed characters go through the IME overlay, so
    // excluding non-ESC data means a keystroke can never be double-sent even if
    // xterm's textarea momentarily had focus. In a plain shell (no alt-screen)
    // xterm scrolls its own scrollback locally and emits nothing here — untouched.
    term.onData((data) => {
      // Only ESC-prefixed xterm output is forwarded; plain typed characters go
      // through the IME overlay, so they can never be double-sent from here.
      if (data.charCodeAt(0) !== 0x1b) return;
      if (isScrollSeq(data)) {
        // Wheel / alt-scroll: a SHARED, non-destructive view move that ANY
        // participant may perform (the relay doesn't view-gate pty_scroll and the
        // pty-host writes it without the driver check). NOT driver-gated.
        connRef.current?.send(buildPtyScroll(data));
        return;
      }
      // Other mouse (clicks/drags) and control sequences xterm emits are actual
      // interaction — driver only, via the gated pty_input path.
      if (shouldSendInput(driverRef.current, mySub)) {
        connRef.current?.send(buildPtyInput(data));
      }
    });

    const url = participantWsEndpoint(session.wsUrl);
    const conn = new RelayConnection(
      url,
      {
        onMessage: (raw) => {
          // host presence rides the same host:status/agent:status frames as chat.
          const chatEv = parseServerEvent(raw);
          if (chatEv?.type === "host_status") setHostUp(chatEv.connected);

          const ev = parseTerminalEvent(raw);
          if (!ev) return;
          switch (ev.type) {
            case "room_mode":
              onMode?.(ev.mode);
              break;
            case "pty_output": {
              const decision = streamTracker.acceptOutput(ev);
              if (decision === "gap") {
                requestSnapshot();
                break;
              }
              if (decision === "duplicate") break;
              outputScheduler.enqueue(
                base64ToBytes(ev.data),
                hiddenRef.current,
              );
              // First real output means the view is up and laid out — do one more
              // layout so the grid matches the container/PTY even if the earlier
              // rAF/font layouts ran before layout settled (a stale grid otherwise).
              if (!didFirstFit.current) {
                didFirstFit.current = true;
                requestAnimationFrame(() => applyLayout());
              }
              if (hiddenRef.current) activityRef.current?.();
              break;
            }
            case "pty_snapshot":
              streamTracker.acceptSnapshot(ev);
              outputScheduler.recover();
              term.reset();
              if (ev.cols && ev.rows) {
                ptySizeRef.current = { cols: ev.cols, rows: ev.rows };
              }
              outputScheduler.enqueue(
                base64ToBytes(ev.data),
                hiddenRef.current,
              );
              requestAnimationFrame(() => applyLayout());
              break;
            case "pty_exit":
              outputScheduler.enqueue(
                UTF8_ENCODER.encode(
                  `\r\n\x1b[33m[프로세스 종료 code=${ev.code ?? "?"}]\x1b[0m\r\n`,
                ),
                hiddenRef.current,
              );
              if (hiddenRef.current) activityRef.current?.();
              break;
            case "pty_size":
              // The host announced its authoritative grid size. Record it, and if
              // I'm a viewer (not the driver) re-letterbox to it now so the
              // pre-wrapped byte stream lines up. The driver ignores it (it owns
              // the size); this also avoids a resize feedback loop.
              ptySizeRef.current = { cols: ev.cols, rows: ev.rows };
              if (!shouldSendInput(driverRef.current, mySub))
                applyViewerLayout();
              break;
            case "driver":
              setDriver(ev.from);
              if (ev.from) {
                setObserved((prev) =>
                  prev.includes(ev.from) ? prev : [...prev, ev.from],
                );
              }
              // The driver seat changed. Re-lay-out regardless of who now holds it:
              // if I just became the driver, applyLayout clears the letterbox scale
              // and fits my container (pushing my size to the PTY); if I just became
              // a viewer, it applies the scale to the PTY's size. Deferred so
              // driverRef has committed before applyLayout's driver check.
              requestAnimationFrame(() => applyLayout());
              if (hiddenRef.current) activityRef.current?.();
              break;
            case "driver_state":
              setDriver(ev.driver);
              if (ev.driver) {
                setObserved((prev) =>
                  prev.includes(ev.driver) ? prev : [...prev, ev.driver],
                );
              }
              requestAnimationFrame(() => applyLayout());
              break;
            case "driver_request":
              if (ev.from) {
                setObserved((prev) =>
                  prev.includes(ev.from) ? prev : [...prev, ev.from],
                );
              }
              if (hiddenRef.current) activityRef.current?.();
              break;
            case "participant_roster": {
              const controlUsers = ev.participants
                .filter((p) => p.role === "host" || p.access === "control")
                .map((p) => p.userId);
              setObserved(Array.from(new Set(controlUsers)));
              setDriver(ev.driver);
              break;
            }
            case "room_chat": {
              // Human side-chat from another participant (the relay never echoes
              // my own sends, so every room_chat I receive is someone else's).
              // Functional setState keeps this bound-once closure valid; the id
              // comes from the monotonic ref counter, not Date.now()/random.
              const id = nextMsgId();
              setChatMsgs((prev) => [
                ...prev,
                { id, from: ev.from, text: ev.text, mine: false },
              ]);
              // Badge the panel toggle when it can't be seen right now, and light
              // the sidebar unread the same way pty_output does while hidden.
              if (!panelOpenRef.current || hiddenRef.current)
                setUnread((u) => u + 1);
              if (hiddenRef.current) activityRef.current?.();
              break;
            }
          }
        },
        onStatus: (s) => {
          setStatus(s);
          if (
            s === "reconnecting" ||
            s === "connecting" ||
            s === "auth-expired"
          )
            setHostUp(false);
          // On every (re)connect, ask the host to replay the current screen — the
          // relay is stateless, so a viewer joining an idle terminal otherwise sees
          // only a blinking cursor. The host replays its scrollback to us alone.
          if (s === "open") requestSnapshot();
        },
      },
      [participantTicketProtocol(session.token)],
    );
    connRef.current = conn;
    conn.start();

    // Refit on any container size change (debounced inside fitAndSend). Observe
    // BOTH the xterm host and its sized flex parent (.term-wrap): the host is
    // position:absolute inset:0, so observing the wrap catches the layout-driven
    // size the host inherits, which is what actually changes on window resize.
    const ro = new ResizeObserver(() => applyLayout());
    if (containerRef.current) {
      ro.observe(containerRef.current);
      if (containerRef.current.parentElement)
        ro.observe(containerRef.current.parentElement);
    }
    window.addEventListener("resize", applyLayout);

    return () => {
      window.removeEventListener("resize", applyLayout);
      ro.disconnect();
      if (resizeTimer.current) clearTimeout(resizeTimer.current);
      if (hintTimer.current) clearTimeout(hintTimer.current);
      conn.close();
      outputScheduler.dispose();
      term.dispose();
      connRef.current = null;
      termRef.current = null;
      fitRef.current = null;
    };
    // hidden/onActivity are read via closures/refs; re-subscribing the socket on
    // those would violate the keep-alive invariant. Only session identity may
    // recreate the connection.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [session.wsUrl, session.token, mySub, terminalGeneration]);

  // A display:none terminal measures as zero, so xterm can't fit while hidden.
  // Re-lay-out the moment the room becomes visible again (driver refits + resyncs
  // the PTY; viewer re-letterboxes) so switching back to a background terminal
  // room isn't missized.
  useEffect(() => {
    if (!hidden) requestAnimationFrame(() => applyLayout());
  }, [hidden, applyLayout]);

  const iAmDriver = shouldSendInput(driver, mySub);
  useEffect(() => {
    if (!iAmDriver) return;
    const renew = () => connRef.current?.send(buildDriverHeartbeat());
    renew();
    const timer = setInterval(renew, 5000);
    return () => clearInterval(timer);
  }, [iAmDriver]);
  const grant = useCallback((target: string) => {
    connRef.current?.send(buildSetDriver(target));
  }, []);
  const requestDriver = useCallback(() => {
    connRef.current?.send(buildDriverRequest());
  }, []);

  // Send one side-chat line: transmit it (the relay injects my sub and fans it
  // out to OTHERS), then optimistically append it locally as mine since I get no
  // echo back. Empty/whitespace-only is a no-op. View-tier guests may chat too,
  // so this is NOT gated on the driver — anyone in the room can talk.
  const sendChat = useCallback(() => {
    const text = chatDraft;
    if (!text.trim()) return;
    connRef.current?.send(buildRoomChat(text));
    setChatMsgs((prev) => [
      ...prev,
      { id: nextMsgId(), from: mySub, text, mine: true },
    ]);
    setChatDraft("");
  }, [chatDraft, mySub]);

  // Toggle the panel; opening it clears the unread badge (the messages are now
  // visible). Closed→open changes the terminal's width, so refit below.
  const togglePanel = useCallback(() => {
    setPanelOpen((v) => {
      if (!v) setUnread(0);
      return !v;
    });
  }, []);

  // Opening/closing the side panel changes the terminal's available width, so
  // re-lay-out (driver refits + resyncs the PTY; viewer re-letterboxes) once the
  // layout has settled. The ResizeObserver on .term-wrap also catches this, but
  // an explicit call guarantees the grid reflows even if the observer coalesces.
  useEffect(() => {
    if (!hidden) requestAnimationFrame(() => applyLayout());
  }, [panelOpen, hidden, applyLayout]);

  // Keep the side-chat list pinned to the newest message when it grows.
  useEffect(() => {
    const el = chatListRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [chatMsgs, panelOpen]);

  const disconnected = status !== "open";
  const connectionLabel =
    status === "open"
      ? "Relay 연결됨"
      : status === "auth-expired"
        ? "인증 만료"
        : status === "closed"
          ? "연결 종료"
          : "다시 연결 중";
  const executionLabel =
    session.executionTarget === "docker"
      ? "Docker 컨테이너"
      : session.executionTarget === "local"
        ? "Host OS"
        : "원격 실행 환경";

  return (
    <div className={"screen terminal" + (hidden ? " hidden" : "")}>
      <header className="topbar terminal-topbar">
        <div className="terminal-room-heading">
          <button
            className="leave terminal-leave"
            onClick={onLeave}
            title="세션 닫기"
            aria-label="세션 닫기"
          >
            <UiIcon name="arrow-left" size={15} />
          </button>
          <span className="terminal-room-icon">
            <UiIcon name="terminal" size={17} />
          </span>
          <div className="terminal-title-copy">
            <span className="terminal-kicker">REMOTE WORKSPACE</span>
            <div className="terminal-title-line">
              <span className="room-name">
                {session.label || session.room || "터미널"}
              </span>
              <span className="terminal-room-meta">
                {executionLabel} · {session.name} · Relay
              </span>
              {session.asHost && <span className="host-tag">HOST</span>}
            </div>
          </div>
        </div>
        <div className="topbar-right">
          <button
            className={"side-chat-toggle" + (panelOpen ? " active" : "")}
            onClick={togglePanel}
            title="사이드 채팅"
            aria-pressed={panelOpen}
            aria-label="사이드 채팅"
          >
            <UiIcon name="chat" size={15} />
            {unread > 0 && !panelOpen && (
              <span className="sc-unread">{unread > 99 ? "99+" : unread}</span>
            )}
          </button>
          <TermFontMenu font={font} onChange={writeTermFont} />
          <DriverBadge driver={driver} iAmDriver={iAmDriver} />
          {!session.asHost && myAccess === "control" && !iAmDriver && (
            <button
              className="driver-request-btn"
              onClick={requestDriver}
              disabled={status !== "open"}
              title={
                driver
                  ? "호스트에게 조작권 인계를 요청합니다"
                  : "비어 있는 조작권을 요청합니다"
              }
            >
              조작권 요청
            </button>
          )}
        </div>
      </header>

      {disconnected && (
        <div className="banner" role="status">
          {status === "closed"
            ? "연결이 종료되었습니다."
            : status === "auth-expired"
              ? "세션이 만료되었거나 거부되었습니다 — 방을 닫고 다시 참가하세요."
              : "릴레이 연결이 끊어졌습니다 — 자동으로 다시 연결하는 중…"}
        </div>
      )}

      {session.asHost && (
        <DriverControls
          driver={driver}
          mySub={mySub}
          observed={observed}
          onGrant={grant}
        />
      )}

      <div className="term-body">
        <div
          className="term-wrap"
          // The overlay is pointer-events:none so mouse selection/scroll still
          // reach xterm; a click anywhere on the terminal refocuses the input.
          onMouseDown={() => {
            if (!hidden) taRef.current?.focus();
          }}
        >
          <div className="term-host" ref={containerRef} />
          <textarea
            ref={taRef}
            className="term-input"
            aria-hidden="true"
            autoCapitalize="off"
            autoComplete="off"
            spellCheck={false}
            {...{ autoCorrect: "off" }}
            // Composition: render the in-progress string in our OWN opaque preview
            // (covers WebKit's un-styleable blue marked-text highlight) and hide
            // xterm's block cursor so the two don't overlap at the same cell. The
            // textarea's own text stays transparent (color:transparent) throughout.
            onCompositionStart={() => {
              termRef.current?.write("\x1b[?25l");
              const cp = composeRef.current;
              if (cp) {
                cp.textContent = "";
                cp.style.display = "block";
              }
            }}
            onCompositionUpdate={(e) => {
              const cp = composeRef.current;
              if (cp) cp.textContent = e.data;
            }}
            // Committed IME string (Hangul/CJK): send the composed text, then clear
            // the preview and restore the cursor.
            onCompositionEnd={(e) => {
              termRef.current?.write("\x1b[?25h");
              const cp = composeRef.current;
              if (cp) {
                cp.textContent = "";
                cp.style.display = "none";
              }
              sendKey(e.data);
              e.currentTarget.value = "";
            }}
            // Ordinary character entry. Ignore events fired mid-composition (those
            // are handled by compositionEnd) and anything that isn't a plain text
            // insertion (paste is handled below; Enter/Backspace/etc by keydown).
            onInput={(e) => {
              const ne = e.nativeEvent as InputEvent;
              if (ne.isComposing) return;
              if (ne.inputType === "insertText" && ne.data) sendKey(ne.data);
              e.currentTarget.value = "";
            }}
            // Paste: forward the clipboard text as one chunk; keep it out of the
            // textarea so no follow-up input event double-sends it.
            onPaste={(e) => {
              const text = e.clipboardData.getData("text");
              if (text) {
                sendKey(text);
                e.preventDefault();
              }
            }}
            // Control/navigation keys → terminal sequences. Skip while composing.
            // A null sequence is an ordinary character — leave it to onInput so IME
            // and multilingual entry keep working (don't preventDefault).
            onKeyDown={(e) => {
              if (e.nativeEvent.isComposing || e.keyCode === 229) return;
              const seq = keyToSequence(e);
              if (seq !== null) {
                sendKey(seq);
                e.preventDefault();
              }
            }}
          />
          {/* Opaque composing-text preview, drawn over the textarea's blue marked
            text. Positioned on the cursor cell by updateInputPos; shown only
            during composition. aria-hidden — xterm/textarea carry semantics. */}
          <div ref={composeRef} className="term-compose" aria-hidden="true" />
          {viewHint && (
            <div className="term-viewhint" role="status">
              보기 전용 — 조작권 없음
            </div>
          )}
        </div>

        {/* Side chat lives OUTSIDE .term-wrap so clicking it never refocuses the
          terminal input (the .term-wrap onMouseDown focuses taRef). It is a
          human↔human channel — messages ride the relay's room_chat frame and
          never reach the shell/Claude. View-tier guests may chat too. */}
        {panelOpen && (
          <aside className="side-chat">
            <div className="side-chat-head">
              <UiIcon name="chat" size={14} />
              <span>사이드 채팅</span>
            </div>
            <div className="side-chat-list" ref={chatListRef}>
              {chatMsgs.length === 0 ? (
                <div className="sc-empty">
                  방에 있는 사람들과 나누는 채팅입니다. 터미널/Claude에는
                  전달되지 않습니다.
                </div>
              ) : (
                chatMsgs.map((m) => (
                  <div
                    key={m.id}
                    className={"sc-row" + (m.mine ? " mine" : " other")}
                  >
                    <span className="sc-who">
                      {m.mine ? "나" : speakerName(m.from)}
                    </span>
                    <span className="sc-text">{m.text}</span>
                  </div>
                ))
              )}
            </div>
            <div className="side-chat-composer">
              <textarea
                className="sc-input"
                value={chatDraft}
                onChange={(e) => setChatDraft(e.target.value)}
                placeholder="메시지 입력 — Enter 전송, Shift+Enter 줄바꿈"
                rows={2}
                spellCheck={false}
                // Enter sends; Shift+Enter inserts a newline; never send mid-IME
                // composition (Enter there commits the Hangul candidate instead).
                onKeyDown={(e) => {
                  if (
                    e.key === "Enter" &&
                    !e.shiftKey &&
                    !e.nativeEvent.isComposing
                  ) {
                    e.preventDefault();
                    sendChat();
                  }
                }}
              />
              <button
                className="sc-send"
                onClick={sendChat}
                disabled={!chatDraft.trim()}
              >
                보내기
              </button>
            </div>
          </aside>
        )}
      </div>
      <footer className="terminal-statusbar">
        <div className="terminal-status-group">
          <span
            className={
              "terminal-connection " +
              (status === "open" ? "online" : "offline")
            }
          >
            <UiIcon name="wifi" size={13} />
            {connectionLabel}
          </span>
          <HostBadge up={hostUp} />
        </div>
        <div className="terminal-status-group end">
          <span className="terminal-access">
            <UiIcon name="shield" size={12} />
            {session.asHost
              ? "호스트"
              : myAccess === "control"
                ? "제어 가능"
                : "보기 전용"}
          </span>
          <span
            className={
              "terminal-target " + (session.executionTarget ?? "unknown")
            }
          >
            {session.executionTarget === "docker"
              ? "Docker"
              : session.executionTarget === "local"
                ? "Host OS"
                : "Remote"}
          </span>
          {session.room && (
            <span className="terminal-room-id" title={session.room}>
              {session.room}
            </span>
          )}
          {mySub && <MyIdChip sub={mySub} />}
        </div>
      </footer>
    </div>
  );
}

// DriverBadge shows who currently holds the keyboard, from everyone's view.
function DriverBadge({
  driver,
  iAmDriver,
}: {
  driver: string;
  iAmDriver: boolean;
}) {
  if (iAmDriver) {
    return (
      <span className="driver-badge me">
        <span className="driver-state-dot" />내가 조작 중
      </span>
    );
  }
  if (driver) {
    return (
      <span className="driver-badge other">
        <span className="driver-state-dot" />
        {speakerName(driver)} 조작 중
      </span>
    );
  }
  return (
    <span className="driver-badge none">
      <span className="driver-state-dot" />보기 전용
    </span>
  );
}

function HostBadge({ up }: { up: boolean }) {
  return (
    <span className={"host-badge " + (up ? "up" : "down")}>
      <span className="dot" />
      {up ? "호스트 연결" : "호스트 끊김"}
    </span>
  );
}

// MyIdChip exposes the verified sub for diagnostics and legacy relay fallback.
// Current relays also publish a participant roster to host-role participants.
function MyIdChip({ sub }: { sub: string }) {
  const [copied, setCopied] = useState(false);
  const copy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(sub);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard blocked — non-fatal */
    }
  }, [sub]);
  return (
    <button className="myid-chip" onClick={copy} title={`내 ID: ${sub}`}>
      {copied ? "복사됨 ✓" : `내 ID ${speakerName(sub)}`}
    </button>
  );
}

// TermFontMenu is the "Aa" top-bar picker for the terminal font family and size.
// The setting is app-wide (all rooms share it), so onChange persists+broadcasts
// via writeTermFont and every mounted TerminalScreen applies it (see applyFont).
function TermFontMenu({
  font,
  onChange,
}: {
  font: TermFont;
  onChange: (font: TermFont) => void;
}) {
  const [open, setOpen] = useState(false);
  return (
    <div className="term-font-menu">
      <button
        className="term-font-toggle"
        onClick={() => setOpen((v) => !v)}
        title="터미널 폰트·크기"
        aria-expanded={open}
      >
        Aa
      </button>
      {open && (
        <div className="term-font-pop" role="dialog">
          <div className="tf-section-label">폰트</div>
          <div className="tf-fonts">
            {TERM_FONTS.map((opt) => (
              <button
                key={opt.label}
                className={
                  "tf-font" + (opt.family === font.family ? " active" : "")
                }
                style={{ fontFamily: opt.family }}
                onClick={() =>
                  onChange({ family: opt.family, size: font.size })
                }
              >
                <span className="tf-font-name">{opt.label}</span>
                {opt.note && <span className="tf-font-note">{opt.note}</span>}
              </button>
            ))}
          </div>
          <div className="tf-section-label">크기</div>
          <div className="tf-sizes">
            {TERM_SIZES.map((sz) => (
              <button
                key={sz}
                className={"tf-size" + (sz === font.size ? " active" : "")}
                onClick={() => onChange({ family: font.family, size: sz })}
              >
                {sz}
              </button>
            ))}
          </div>
          <p className="tf-hint">
            D2Coding은 앱에 번들되어 한글·영문이 정렬됩니다. 그 외 폰트는
            시스템에 설치된 경우에만 적용됩니다.
          </p>
        </div>
      )}
    </div>
  );
}

// DriverControls is the operator-only (role=host) panel to hand the keyboard to
// a participant or reclaim it. The relay roster populates quick-grant buttons
// for every host/control participant; manual id entry remains as a compatibility
// fallback for legacy relays that do not publish participant_roster. "" revokes.
// dcCollapsedKey persists the operator's show/hide choice for the panel so it
// stays collapsed (or open) across mounts and app restarts.
const DC_COLLAPSED_KEY = "pie-relay.dc-collapsed";
const LEGACY_DC_COLLAPSED_KEY = "cli-relay.dc-collapsed";

function DriverControls({
  driver,
  mySub,
  observed,
  onGrant,
}: {
  driver: string;
  mySub: string;
  observed: string[];
  onGrant: (target: string) => void;
}) {
  const [entry, setEntry] = useState("");
  // Collapse the whole panel to give the terminal more room. The header stays
  // visible as a toggle; collapsing frees vertical space and the term refits via
  // the ResizeObserver. Default expanded (discoverable); choice is persisted.
  const [collapsed, setCollapsed] = useState<boolean>(() => {
    try {
      return (
        localStorage.getItem(DC_COLLAPSED_KEY) === "1" ||
        (!localStorage.getItem(DC_COLLAPSED_KEY) &&
          localStorage.getItem(LEGACY_DC_COLLAPSED_KEY) === "1")
      );
    } catch {
      return false;
    }
  });
  const toggle = () => {
    setCollapsed((v) => {
      const next = !v;
      try {
        localStorage.setItem(DC_COLLAPSED_KEY, next ? "1" : "0");
      } catch {
        /* private mode / disabled storage — non-fatal */
      }
      return next;
    });
  };
  const submit = () => {
    const t = entry.trim();
    if (t) {
      onGrant(t);
      setEntry("");
    }
  };
  // Quick-grant candidates: observed drivers other than the current one and me.
  const candidates = observed.filter((s) => s !== driver && s !== mySub);
  return (
    <div className={"driver-controls" + (collapsed ? " collapsed" : "")}>
      <button
        className="driver-controls-head"
        onClick={toggle}
        aria-expanded={!collapsed}
        title={
          collapsed ? "조작권·참가자 패널 펼치기" : "조작권·참가자 패널 접기"
        }
      >
        <span className="dc-head-title">조작권 제어 · 참가자 (호스트)</span>
        {/* Up when open (click to collapse up), down when collapsed (click to
            expand down) — the arrow makes the current state unambiguous. */}
        <span className="dc-toggle" aria-hidden="true">
          {collapsed ? "▼" : "▲"}
        </span>
      </button>
      {!collapsed && (
        <>
          <div className="driver-controls-row">
            <button
              className="dc-btn"
              onClick={() => onGrant(mySub)}
              disabled={!mySub || driver === mySub}
            >
              나에게 조작권
            </button>
            <button
              className="dc-btn"
              onClick={() => onGrant("")}
              disabled={driver === ""}
            >
              회수 (보기 전용)
            </button>
          </div>
          <div className="driver-controls-row">
            <input
              className="dc-input"
              type="text"
              value={entry}
              onChange={(e) => setEntry(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && submit()}
              placeholder="참가자 ID(sub) 붙여넣기 — 예: guest:bob-x7k2"
              spellCheck={false}
              autoCapitalize="off"
            />
            <button
              className="dc-btn primary"
              onClick={submit}
              disabled={!entry.trim()}
            >
              조작권 주기
            </button>
          </div>
          {candidates.length > 0 && (
            <div className="driver-controls-row candidates">
              <span className="dc-label">최근:</span>
              {candidates.map((s) => (
                <button key={s} className="dc-chip" onClick={() => onGrant(s)}>
                  {speakerName(s)}
                </button>
              ))}
            </div>
          )}
          <p className="dc-hint">
            참가자 명단 브로드캐스트가 없어 ID를 자동 나열할 수 없습니다.
            참가자가 자기 화면의 “내 ID”를 알려주면 위에 붙여넣어 조작권을
            주세요.
          </p>
        </>
      )}
    </div>
  );
}
